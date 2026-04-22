// Package jobreg is the thin CRUD + reconcile surface for scheduled_jobs
// and webhook_handlers from Appendix C.4. Bot slash commands, CLI, and
// YAML reconcile all write through these functions.
package jobreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// Schedule is the input shape for scheduled_jobs registration.
type Schedule struct {
	Name            string         `yaml:"name"`
	Cron            string         `yaml:"cron"`
	Timezone        string         `yaml:"timezone,omitempty"`
	AgentID         string         `yaml:"agent_id,omitempty"`
	SoulTemplateID  string         `yaml:"soul_template_id,omitempty"`
	ContextMarkdown string         `yaml:"context_markdown,omitempty"`
	AgentCWD        string         `yaml:"agent_cwd,omitempty"`
	Prompt          map[string]any `yaml:"prompt"`
	ReplyChannel    map[string]any `yaml:"reply_channel,omitempty"`
	WarmSpawnBefore string         `yaml:"warm_spawn_before,omitempty"` // pg interval (e.g. "10 minutes")
	Enabled         *bool          `yaml:"enabled,omitempty"`
}

// Hook is the input shape for webhook_handlers registration.
type Hook struct {
	Name            string         `yaml:"name"`
	Path            string         `yaml:"path"`
	Secret          string         `yaml:"secret"`
	SignatureScheme string         `yaml:"signature_scheme,omitempty"`
	EventFilter     map[string]any `yaml:"event_filter,omitempty"`
	AgentID         string         `yaml:"agent_id"`
	PromptTemplate  string         `yaml:"prompt_template"`
	ReplyChannel    map[string]any `yaml:"reply_channel,omitempty"`
	RateLimitPerMin int            `yaml:"rate_limit_per_min,omitempty"`
	Enabled         *bool          `yaml:"enabled,omitempty"`
}

// AddSchedule inserts or updates a scheduled_jobs row by name.
// Returns the row's UUID.
func AddSchedule(ctx context.Context, pool *pgxpool.Pool, s Schedule) (string, error) {
	if err := validateSchedule(s); err != nil {
		return "", err
	}
	enabled := true
	if s.Enabled != nil {
		enabled = *s.Enabled
	}
	tz := s.Timezone
	if tz == "" {
		tz = "UTC"
	}

	next, err := computeNext(s.Cron, tz, time.Now())
	if err != nil {
		return "", fmt.Errorf("cron: %w", err)
	}

	promptJSON, _ := json.Marshal(s.Prompt)
	var replyJSON []byte
	if len(s.ReplyChannel) > 0 {
		replyJSON, _ = json.Marshal(s.ReplyChannel)
	}
	var warmPtr *string
	if s.WarmSpawnBefore != "" {
		warmPtr = &s.WarmSpawnBefore
	}

	var agentIDPtr *string
	if s.AgentID != "" {
		agentIDPtr = &s.AgentID
	}
	var soulTemplateIDPtr *string
	if s.SoulTemplateID != "" {
		soulTemplateIDPtr = &s.SoulTemplateID
	}

	var id string
	err = pool.QueryRow(ctx, `
		INSERT INTO scheduled_jobs
			(name, cron_expr, timezone, agent_id, soul_template_id, context_markdown, agent_cwd,
			 prompt, reply_channel, warm_spawn_before, enabled, next_run_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10::interval, $11, $12)
		ON CONFLICT (name) DO UPDATE SET
			cron_expr        = EXCLUDED.cron_expr,
			timezone         = EXCLUDED.timezone,
			agent_id         = EXCLUDED.agent_id,
			soul_template_id = EXCLUDED.soul_template_id,
			context_markdown = EXCLUDED.context_markdown,
			agent_cwd        = EXCLUDED.agent_cwd,
			prompt           = EXCLUDED.prompt,
			reply_channel    = EXCLUDED.reply_channel,
			warm_spawn_before = EXCLUDED.warm_spawn_before,
			enabled          = EXCLUDED.enabled,
			next_run_at      = EXCLUDED.next_run_at
		RETURNING id::text
	`, s.Name, s.Cron, tz, agentIDPtr, soulTemplateIDPtr, s.ContextMarkdown, s.AgentCWD,
		promptJSON, replyJSON, warmPtr, enabled, next).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert: %w", err)
	}
	return id, nil
}

// ListSchedules returns every schedule row, ordered by name.
func ListSchedules(ctx context.Context, pool *pgxpool.Pool) ([]ScheduleRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT sj.id::text, sj.name, sj.cron_expr, sj.timezone,
		       COALESCE(sj.agent_id, ''), sj.enabled,
		       sj.next_run_at, COALESCE(sj.last_run_at, 'epoch'::timestamptz),
		       COALESCE(sj.soul_template_id, ''),
		       COALESCE(sj.context_markdown, ''),
		       COALESCE(sj.agent_cwd, ''),
		       COALESCE(st.name, '') AS soul_template_name
		FROM scheduled_jobs sj
		LEFT JOIN soul_templates st ON st.id = sj.soul_template_id
		ORDER BY sj.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduleRow
	for rows.Next() {
		var r ScheduleRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Cron, &r.Timezone, &r.AgentID,
			&r.Enabled, &r.NextRunAt, &r.LastRunAt,
			&r.SoulTemplateID, &r.ContextMarkdown, &r.AgentCWD, &r.SoulTemplateName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ScheduleRow is the list-view projection.
type ScheduleRow struct {
	ID               string
	Name             string
	Cron             string
	Timezone         string
	AgentID          string
	SoulTemplateID   string
	SoulTemplateName string
	ContextMarkdown  string
	AgentCWD         string
	Enabled          bool
	NextRunAt        time.Time
	LastRunAt        time.Time
}

// RmSchedule deletes a schedule by name. Idempotent — missing names are not an error.
func RmSchedule(ctx context.Context, pool *pgxpool.Pool, name string) error {
	_, err := pool.Exec(ctx, `DELETE FROM scheduled_jobs WHERE name=$1`, name)
	return err
}

// DisableSchedule soft-deletes by clearing the enabled flag (keeps audit trail).
func DisableSchedule(ctx context.Context, pool *pgxpool.Pool, name string) error {
	tag, err := pool.Exec(ctx, `UPDATE scheduled_jobs SET enabled=FALSE WHERE name=$1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no schedule named %q", name)
	}
	return nil
}

// AddHook upserts a webhook_handlers row by name.
func AddHook(ctx context.Context, pool *pgxpool.Pool, h Hook) (string, error) {
	if err := validateHook(h); err != nil {
		return "", err
	}
	enabled := true
	if h.Enabled != nil {
		enabled = *h.Enabled
	}
	scheme := h.SignatureScheme
	if scheme == "" {
		scheme = "github-hmac-sha256"
	}
	rate := h.RateLimitPerMin
	if rate == 0 {
		rate = 60
	}

	var filterJSON, replyJSON []byte
	if len(h.EventFilter) > 0 {
		filterJSON, _ = json.Marshal(h.EventFilter)
	}
	if len(h.ReplyChannel) > 0 {
		replyJSON, _ = json.Marshal(h.ReplyChannel)
	}

	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO webhook_handlers
			(name, path, secret, signature_scheme, event_filter, agent_id,
			 prompt_template, reply_channel, rate_limit_per_min, enabled)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8::jsonb,$9,$10)
		ON CONFLICT (name) DO UPDATE SET
			path = EXCLUDED.path,
			secret = EXCLUDED.secret,
			signature_scheme = EXCLUDED.signature_scheme,
			event_filter = EXCLUDED.event_filter,
			agent_id = EXCLUDED.agent_id,
			prompt_template = EXCLUDED.prompt_template,
			reply_channel = EXCLUDED.reply_channel,
			rate_limit_per_min = EXCLUDED.rate_limit_per_min,
			enabled = EXCLUDED.enabled
		RETURNING id::text
	`, h.Name, h.Path, h.Secret, scheme, filterJSON, h.AgentID, h.PromptTemplate, replyJSON, rate, enabled).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert hook: %w", err)
	}
	return id, nil
}

// HookRow is the list-view projection.
type HookRow struct {
	ID      string
	Name    string
	Path    string
	AgentID string
	Enabled bool
}

// ListHooks returns every handler row, ordered by name.
func ListHooks(ctx context.Context, pool *pgxpool.Pool) ([]HookRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text, name, path, agent_id, enabled
		FROM webhook_handlers ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HookRow
	for rows.Next() {
		var r HookRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Path, &r.AgentID, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RmHook deletes a handler by name.
func RmHook(ctx context.Context, pool *pgxpool.Pool, name string) error {
	_, err := pool.Exec(ctx, `DELETE FROM webhook_handlers WHERE name=$1`, name)
	return err
}

// SetHookEnabled flips the enabled flag for /hook_enable, /hook_disable.
func SetHookEnabled(ctx context.Context, pool *pgxpool.Pool, name string, enabled bool) error {
	tag, err := pool.Exec(ctx, `UPDATE webhook_handlers SET enabled=$2 WHERE name=$1`, name, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no hook named %q", name)
	}
	return nil
}

// Reconcile loads every schedule YAML from schedulesDir and hook YAML from
// hooksDir, upserting each file into the DB. Rows present in the DB whose
// name is no longer on disk are soft-disabled (keeps audit trail per task
// spec). Missing directories are treated as empty.
func Reconcile(ctx context.Context, pool *pgxpool.Pool, schedulesDir, hooksDir string) error {
	schedules, err := loadSchedulesFromDir(schedulesDir)
	if err != nil {
		return fmt.Errorf("schedules: %w", err)
	}
	hooks, err := loadHooksFromDir(hooksDir)
	if err != nil {
		return fmt.Errorf("hooks: %w", err)
	}

	seenSchedules := map[string]bool{}
	for _, s := range schedules {
		if _, err := AddSchedule(ctx, pool, s); err != nil {
			return fmt.Errorf("upsert %s: %w", s.Name, err)
		}
		seenSchedules[s.Name] = true
	}
	rows, _ := pool.Query(ctx, `SELECT name FROM scheduled_jobs WHERE enabled=TRUE`)
	var stale []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		if !seenSchedules[n] {
			stale = append(stale, n)
		}
	}
	rows.Close()
	for _, n := range stale {
		if err := DisableSchedule(ctx, pool, n); err != nil {
			return err
		}
	}

	seenHooks := map[string]bool{}
	for _, h := range hooks {
		if _, err := AddHook(ctx, pool, h); err != nil {
			return fmt.Errorf("upsert hook %s: %w", h.Name, err)
		}
		seenHooks[h.Name] = true
	}
	rows, _ = pool.Query(ctx, `SELECT name FROM webhook_handlers WHERE enabled=TRUE`)
	var staleHooks []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		if !seenHooks[n] {
			staleHooks = append(staleHooks, n)
		}
	}
	rows.Close()
	for _, n := range staleHooks {
		if err := SetHookEnabled(ctx, pool, n, false); err != nil {
			return err
		}
	}
	return nil
}

// ────────────────────────────────────────────────────────────────────────────
// validation + loading helpers

func validateSchedule(s Schedule) error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("name required")
	}
	if strings.TrimSpace(s.AgentID) == "" && strings.TrimSpace(s.SoulTemplateID) == "" {
		return errors.New("agent_id or soul_template_id required")
	}
	if _, err := cron.ParseStandard(s.Cron); err != nil {
		return fmt.Errorf("invalid cron %q: %w", s.Cron, err)
	}
	if len(s.Prompt) == 0 {
		return errors.New("prompt required")
	}
	if s.Timezone != "" {
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return fmt.Errorf("invalid timezone %q: %w", s.Timezone, err)
		}
	}
	return nil
}

func validateHook(h Hook) error {
	if strings.TrimSpace(h.Name) == "" {
		return errors.New("name required")
	}
	if !strings.HasPrefix(h.Path, "/hooks/") {
		return fmt.Errorf("path must start with /hooks/, got %q", h.Path)
	}
	if h.Secret == "" {
		return errors.New("secret required")
	}
	if h.AgentID == "" {
		return errors.New("agent_id required")
	}
	if strings.TrimSpace(h.PromptTemplate) == "" {
		return errors.New("prompt_template required")
	}
	return nil
}

func computeNext(expr, tz string, ref time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, err
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(ref.In(loc)), nil
}

func loadSchedulesFromDir(dir string) ([]Schedule, error) {
	if dir == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		return nil, err
	}
	var out []Schedule
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var s Schedule
		if err := yaml.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func loadHooksFromDir(dir string) ([]Hook, error) {
	if dir == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		return nil, err
	}
	var out []Hook
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var h Hook
		if err := yaml.Unmarshal(b, &h); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out = append(out, h)
	}
	return out, nil
}
