// Package scheduler runs the cron-driven job source from Appendix C.2.
// A single replica claims due scheduled_jobs with FOR UPDATE SKIP LOCKED,
// enqueues one agent_inbox row per fire (idempotent via
// external_msg_id='sched:<job_id>:<fire_ts>'), and advances next_run_at
// via robfig/cron semantics. Missed fires collapse to a single catch-up.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
	"github.com/robfig/cron/v3"
)

// EnsureLive is a callback into the orchestrator: "make sure agent `id`
// has a live pty before I fire this job." Used by warm_spawn_before. May
// be nil (lazy spawn).
type EnsureLive func(ctx context.Context, agentID string) error

// FiredJob carries all fields of a due scheduled_job row when
// soul_template_id is set (fresh-agent spawn path).
type FiredJob struct {
	ID              string
	Name            string
	CronExpr        string
	Timezone        string
	AgentID         string // may be empty if soul_template_id set
	SoulTemplateID  string // may be empty if agent_id set
	ContextMarkdown string
	AgentCWD        string
	Prompt          []byte
	ReplyChannel    []byte
	NextRunAt       time.Time
}

// Config bundles scheduler knobs.
type Config struct {
	PollInterval time.Duration
	EnsureLive   EnsureLive
	// SpawnFunc is called instead of inbox-inject when a job has
	// soul_template_id set. If nil, fresh-spawn jobs are skipped with a
	// warning.
	SpawnFunc func(ctx context.Context, job FiredJob) error
	Now       func() time.Time // test hook
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		PollInterval: 30 * time.Second,
		Now:          time.Now,
	}
}

// Run drives the scheduler loop until ctx is cancelled. On each tick it
// drains every job whose next_run_at <= now.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	// Initial sweep before the first tick.
	if err := drain(ctx, pool, cfg); err != nil {
		log.Printf("scheduler: initial drain: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := drain(ctx, pool, cfg); err != nil {
				log.Printf("scheduler: %v", err)
			}
		}
	}
}

func drain(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	for {
		fired, err := FireOne(ctx, pool, cfg)
		if err != nil {
			return err
		}
		if !fired {
			return nil
		}
	}
}

// FireOne claims at most one due job and either enqueues its inbox row (legacy
// agent_id path) or calls cfg.SpawnFunc (soul_template_id fresh-spawn path).
// Returns (true, nil) when a job fired, (false, nil) when nothing was due.
func FireOne(ctx context.Context, pool *pgxpool.Pool, cfg Config) (bool, error) {
	now := cfg.Now()

	type job struct {
		id              uuid.UUID
		name            string
		cronExpr        string
		timezone        string
		agentID         string
		soulTemplateID  string
		contextMarkdown string
		agentCWD        string
		prompt          []byte
		replyChannel    []byte
		warmSpawn       *time.Duration
		nextRunAt       time.Time
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var j job
	var warmInterval *string
	err = tx.QueryRow(ctx, `
		SELECT id, name, cron_expr, timezone,
		       COALESCE(agent_id, ''), COALESCE(soul_template_id, ''),
		       COALESCE(context_markdown, ''), COALESCE(agent_cwd, ''),
		       prompt, reply_channel,
		       warm_spawn_before::text, next_run_at
		FROM scheduled_jobs
		WHERE enabled AND next_run_at <= $1
		ORDER BY next_run_at
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`, now).Scan(&j.id, &j.name, &j.cronExpr, &j.timezone,
		&j.agentID, &j.soulTemplateID, &j.contextMarkdown, &j.agentCWD,
		&j.prompt, &j.replyChannel, &warmInterval, &j.nextRunAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if warmInterval != nil {
		d, _ := parsePgInterval(*warmInterval)
		j.warmSpawn = &d
	}

	// Advance next_run_at via robfig/cron in the configured TZ (computed
	// before the inbox enqueue so we can commit in one shot regardless of path).
	next, err := nextAfter(j.cronExpr, j.timezone, now)
	if err != nil {
		return false, fmt.Errorf("cron next: %w", err)
	}

	var inboxID uuid.UUID // zero value → nil in last_inbox_id for spawn path

	if j.soulTemplateID != "" {
		// Fresh-agent spawn path: delegate to SpawnFunc (wired in cmd_start).
		if cfg.SpawnFunc == nil {
			log.Printf("scheduler %s: soul_template_id set but no SpawnFunc configured; skipping", j.name)
		} else {
			fj := FiredJob{
				ID:              j.id.String(),
				Name:            j.name,
				CronExpr:        j.cronExpr,
				Timezone:        j.timezone,
				AgentID:         j.agentID,
				SoulTemplateID:  j.soulTemplateID,
				ContextMarkdown: j.contextMarkdown,
				AgentCWD:        j.agentCWD,
				Prompt:          j.prompt,
				ReplyChannel:    j.replyChannel,
				NextRunAt:       j.nextRunAt,
			}
			if err := cfg.SpawnFunc(ctx, fj); err != nil {
				return false, fmt.Errorf("spawn job %s: %w", j.name, err)
			}
		}
	} else {
		// Legacy inbox-inject path.

		// Warm-spawn the agent if requested and we're within the window.
		if cfg.EnsureLive != nil && j.warmSpawn != nil && now.Add(*j.warmSpawn).After(j.nextRunAt) {
			if err := cfg.EnsureLive(ctx, j.agentID); err != nil {
				log.Printf("scheduler %s: ensure_live: %v", j.name, err)
			}
		}

		fireTS := j.nextRunAt.UTC().Format(time.RFC3339)
		externalID := fmt.Sprintf("sched:%s:%s", j.id, fireTS)

		// Pull reply_channel fields for inbox origin_* columns.
		channel, userID, threadID, chatID := unpackReplyChannel(j.replyChannel)
		if channel == "" {
			channel = "scheduled"
		}

		inboxMsg := mailbox.InboxMessage{
			AgentID:        j.agentID,
			FromKind:       "scheduled",
			FromID:         j.id.String(),
			OriginChannel:  channel,
			OriginUserID:   userID,
			OriginThreadID: threadID,
			OriginChatID:   chatID,
			ExternalMsgID:  externalID,
			Content:        j.prompt,
		}
		var inserted bool
		inboxID, inserted, err = mailbox.EnqueueInbox(ctx, tx, inboxMsg)
		if err != nil {
			return false, fmt.Errorf("enqueue inbox: %w", err)
		}
		_ = inserted // duplicate fire is benign
	}

	// For the fresh-spawn path (soulTemplateID set), inboxID remains the zero
	// UUID — pass nil so last_inbox_id stays NULL (avoids FK violation).
	var inboxIDArg interface{}
	if inboxID != (uuid.UUID{}) {
		inboxIDArg = inboxID
	}
	if _, err := tx.Exec(ctx, `
		UPDATE scheduled_jobs
		SET next_run_at = $2, last_run_at = $3, last_inbox_id = $4
		WHERE id = $1
	`, j.id, next, now, inboxIDArg); err != nil {
		return false, fmt.Errorf("advance job: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// nextAfter computes the next firing time strictly after `ref` using
// robfig/cron's 5-field parser (no seconds), honoring the named timezone.
func nextAfter(expr, tz string, ref time.Time) (time.Time, error) {
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("timezone %q: %w", tz, err)
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron %q: %w", expr, err)
	}
	return sched.Next(ref.In(loc)), nil
}

// parsePgInterval converts Postgres's text interval (e.g. "00:05:00" or
// "1 day 00:05:00") to a time.Duration. Best-effort: returns 0 on failure.
func parsePgInterval(s string) (time.Duration, error) {
	// Handle common HH:MM:SS form first.
	var h, m, sec int
	if n, _ := fmt.Sscanf(s, "%d:%d:%d", &h, &m, &sec); n == 3 {
		return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second, nil
	}
	d, err := time.ParseDuration(s)
	return d, err
}

func unpackReplyChannel(raw []byte) (channel, userID, threadID string, chatID *int64) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", "", "", nil
	}
	var rc struct {
		Channel  string `json:"channel"`
		UserID   string `json:"user_id"`
		ThreadID string `json:"thread_id"`
		ChatID   *int64 `json:"chat_id"`
	}
	if err := json.Unmarshal(raw, &rc); err != nil {
		return "", "", "", nil
	}
	return rc.Channel, rc.UserID, rc.ThreadID, rc.ChatID
}
