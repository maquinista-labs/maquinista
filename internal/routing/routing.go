// Package routing implements the §8.1 routing ladder: explicit @mention →
// owner binding → spawn per-topic agent → explicit attach via picker. See
// plans/maquinista-v2.md §8.1 and plans/per-topic-agent-pivot.md for the
// full rationale.
package routing

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tier identifies which routing tier matched.
type Tier int

const (
	TierNone         Tier = 0
	TierMention      Tier = 1
	TierOwnerBinding Tier = 2
	TierSpawn        Tier = 3
	TierPicker       Tier = 4
)

func (t Tier) String() string {
	switch t {
	case TierMention:
		return "mention"
	case TierOwnerBinding:
		return "owner"
	case TierSpawn:
		return "spawn"
	case TierPicker:
		return "picker"
	}
	return "none"
}

// Resolution describes how a message was routed.
type Resolution struct {
	AgentID    string // the resolved agent; empty iff Tier == TierPicker
	Tier       Tier
	Text       string // the message text (mention stripped when Tier == TierMention)
	BindingSet bool   // true when this call wrote an owner binding
}

// SpawnFunc spawns a fresh per-topic agent for (userID, threadID, chatID)
// and returns the new agent's canonical id. Supplied by the caller (the
// bot) so routing stays DB-focused and the spawn/tmux/runner machinery
// lives in cmd/maquinista. A nil SpawnFunc forces tier-4 picker fallback.
type SpawnFunc func(ctx context.Context, userID, threadID string, chatID *int64) (string, error)

// ErrRequirePicker means no tier resolved — caller must prompt the user.
var ErrRequirePicker = errors.New("routing: no tier matched; show picker")

// mentionPattern matches a leading @<id-or-handle> token.
var mentionPattern = regexp.MustCompile(`^\s*@([A-Za-z0-9][A-Za-z0-9_-]*)\b\s*`)

// ParseMention extracts an @<token> prefix; returns ("", text, false) if absent.
func ParseMention(text string) (token, stripped string, ok bool) {
	m := mentionPattern.FindStringSubmatchIndex(text)
	if m == nil {
		return "", text, false
	}
	id := text[m[2]:m[3]]
	return id, strings.TrimLeft(text[m[1]:], " \t"), true
}

// ResolveAgentByToken looks up an agent by id or handle (case-insensitive).
// Returns ("", nil) if not found so callers can surface a "no such agent"
// error without conflating a DB failure.
func ResolveAgentByToken(ctx context.Context, pool *pgxpool.Pool, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	var id string
	err := pool.QueryRow(ctx, `
		SELECT id FROM agents
		WHERE id = $1 OR LOWER(handle) = LOWER($1)
		LIMIT 1
	`, token).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve agent token: %w", err)
	}
	return id, nil
}

// Resolve walks the §8.1 routing ladder in order. Tiers 1 and 2 don't
// mutate state; tier 3 spawns a fresh per-topic agent and writes an owner
// binding in one go. Tier 4 (picker) is surfaced via ErrRequirePicker when
// SpawnFunc is nil or returns an error.
//
// Concurrent tier-3 writers race cleanly: the partial unique index on
// (user_id, thread_id) WHERE binding_type='owner' picks a single winner and
// the loser reads the committed row.
func Resolve(
	ctx context.Context,
	pool *pgxpool.Pool,
	spawnFn SpawnFunc,
	userID, threadID string,
	chatID *int64,
	text string,
) (*Resolution, error) {
	// Tier 1: explicit @mention — resolve id-or-handle to canonical id.
	if token, stripped, ok := ParseMention(text); ok {
		canonical, err := ResolveAgentByToken(ctx, pool, token)
		if err != nil {
			return nil, fmt.Errorf("tier 1 resolve token: %w", err)
		}
		resolved := canonical
		if resolved == "" {
			// Token didn't match any agent. Pass the raw token back so the
			// caller can render a "no such agent" error to the user.
			resolved = token
		}
		return &Resolution{AgentID: resolved, Tier: TierMention, Text: stripped}, nil
	}

	// Tier 2: owner binding.
	ownerID, err := lookupOwner(ctx, pool, userID, threadID)
	if err != nil {
		return nil, fmt.Errorf("tier 2 owner lookup: %w", err)
	}
	if ownerID != "" {
		return &Resolution{AgentID: ownerID, Tier: TierOwnerBinding, Text: text}, nil
	}

	// Tier 3: spawn a fresh per-topic agent, then write the owner binding.
	// If the caller hasn't provided SpawnFunc, fall through to the picker —
	// useful for contexts (tests, admin tools) that want to route without
	// auto-spawning.
	if spawnFn != nil {
		newAgentID, serr := spawnFn(ctx, userID, threadID, chatID)
		if serr != nil {
			return nil, fmt.Errorf("tier 3 spawn: %w", serr)
		}
		if newAgentID != "" {
			bindingSet, resolvedID, err := writeOwnerBinding(ctx, pool, userID, threadID, chatID, newAgentID)
			if err != nil {
				return nil, fmt.Errorf("tier 3 bind: %w", err)
			}
			return &Resolution{AgentID: resolvedID, Tier: TierSpawn, Text: text, BindingSet: bindingSet}, nil
		}
	}

	// Tier 4: picker — caller handles UI.
	return nil, ErrRequirePicker
}

// ConfirmPickerChoice is called when the user selects an agent in the tier-4
// picker. Writes the owner binding and returns the chosen agent.
func ConfirmPickerChoice(ctx context.Context, pool *pgxpool.Pool, userID, threadID string, chatID *int64, agentID string) (*Resolution, error) {
	if agentID == "" {
		return nil, errors.New("ConfirmPickerChoice: empty agent_id")
	}
	bindingSet, resolvedID, err := writeOwnerBinding(ctx, pool, userID, threadID, chatID, agentID)
	if err != nil {
		return nil, fmt.Errorf("picker bind: %w", err)
	}
	return &Resolution{AgentID: resolvedID, Tier: TierPicker, BindingSet: bindingSet}, nil
}

// SetUserDefault is the /agent_default slash command: attach (user, thread)
// to an already-existing agent identified by id or handle. Unknown tokens
// return an error — creation happens only via tier-3 spawn on a regular
// message, never from /agent_default.
func SetUserDefault(ctx context.Context, pool *pgxpool.Pool, userID, threadID string, chatID *int64, token string) (*Resolution, error) {
	canonical, err := ResolveAgentByToken(ctx, pool, token)
	if err != nil {
		return nil, err
	}
	if canonical == "" {
		return nil, ErrUnknownAgent
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		DELETE FROM topic_agent_bindings
		WHERE binding_type='owner' AND user_id=$1 AND thread_id=$2
	`, userID, threadID); err != nil {
		return nil, fmt.Errorf("clear owner: %w", err)
	}
	var threadNum int64
	fmt.Sscanf(threadID, "%d", &threadNum)
	if _, err := tx.Exec(ctx, `
		INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
		VALUES ($1, $2, 'owner', $3, $4, $5)
	`, threadNum, canonical, userID, threadID, chatID); err != nil {
		return nil, fmt.Errorf("insert owner: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &Resolution{AgentID: canonical, Tier: TierOwnerBinding, BindingSet: true}, nil
}

// ErrUnknownAgent is returned by SetUserDefault when the token doesn't
// resolve to an existing agent. Callers render a user-facing guidance
// message (list agents, or start a new topic).
var ErrUnknownAgent = errors.New("routing: no agent with that id or handle")

// Handle format: lowercase [a-z0-9_-], 2-32 chars. 't-' prefix reserved for
// auto-generated ids so handles cannot shadow them.
var handlePattern = regexp.MustCompile(`^[a-z0-9_-]{2,32}$`)

// ValidateHandle enforces the handle contract. Returns a descriptive error
// suitable for surfacing directly to the user.
func ValidateHandle(handle string) error {
	if handle == "" {
		return errors.New("handle cannot be empty")
	}
	h := strings.ToLower(handle)
	if !handlePattern.MatchString(h) {
		return errors.New("handle must match ^[a-z0-9_-]{2,32}$")
	}
	if strings.HasPrefix(h, "t-") {
		return errors.New("handle prefix 't-' is reserved for auto-ids")
	}
	return nil
}

// SetHandle assigns handle to agentID. Returns ErrHandleTaken if another
// agent already owns the (case-insensitive) handle.
func SetHandle(ctx context.Context, pool *pgxpool.Pool, agentID, handle string) error {
	if err := ValidateHandle(handle); err != nil {
		return err
	}
	normalized := strings.ToLower(handle)

	var existing string
	err := pool.QueryRow(ctx, `
		SELECT id FROM agents WHERE LOWER(handle) = $1 LIMIT 1
	`, normalized).Scan(&existing)
	if err == nil && existing != agentID {
		return ErrHandleTaken
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("handle uniqueness check: %w", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE agents SET handle=$1 WHERE id=$2`, normalized, agentID); err != nil {
		return fmt.Errorf("set handle: %w", err)
	}
	return nil
}

// ErrHandleTaken indicates the handle is already in use by another agent.
var ErrHandleTaken = errors.New("routing: handle already taken")

func lookupOwner(ctx context.Context, pool *pgxpool.Pool, userID, threadID string) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		SELECT agent_id FROM topic_agent_bindings
		WHERE binding_type='owner' AND user_id=$1 AND thread_id=$2
		LIMIT 1
	`, userID, threadID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// writeOwnerBinding inserts an owner row; on conflict reads the existing
// owner (so a concurrent winner is surfaced to callers without error).
func writeOwnerBinding(ctx context.Context, pool *pgxpool.Pool, userID, threadID string, chatID *int64, agentID string) (bool, string, error) {
	var threadNum int64
	fmt.Sscanf(threadID, "%d", &threadNum)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		INSERT INTO topic_agent_bindings
			(topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
		VALUES ($1, $2, 'owner', $3, $4, $5)
		ON CONFLICT (user_id, thread_id) WHERE binding_type='owner' DO NOTHING
	`, threadNum, agentID, userID, threadID, chatID)
	if err != nil {
		return false, "", fmt.Errorf("insert: %w", err)
	}
	wrote := tag.RowsAffected() == 1
	if !wrote {
		// Someone else won the race; read the committed row.
		var existing string
		if err := tx.QueryRow(ctx, `
			SELECT agent_id FROM topic_agent_bindings
			WHERE binding_type='owner' AND user_id=$1 AND thread_id=$2
		`, userID, threadID).Scan(&existing); err != nil {
			return false, "", fmt.Errorf("read existing: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, "", err
		}
		return false, existing, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, "", err
	}
	return true, agentID, nil
}
