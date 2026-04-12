// Package routing implements the §8.1 routing ladder: explicit @mention →
// owner binding → global default → (caller-side) picker. See
// plans/maquinista-v2.md §8.1 for the full rationale.
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
	TierNone          Tier = 0
	TierMention       Tier = 1
	TierOwnerBinding  Tier = 2
	TierGlobalDefault Tier = 3
	TierPicker        Tier = 4
)

func (t Tier) String() string {
	switch t {
	case TierMention:
		return "mention"
	case TierOwnerBinding:
		return "owner"
	case TierGlobalDefault:
		return "global_default"
	case TierPicker:
		return "picker"
	}
	return "none"
}

// Resolution describes how a message was routed.
type Resolution struct {
	AgentID     string // the resolved agent; empty iff Tier == TierPicker
	Tier        Tier
	Text        string // the message text (mention stripped when Tier == TierMention)
	BindingSet  bool   // true when a tier-3 resolve caused this call to write an owner binding
}

// ErrRequirePicker means no tier resolved — caller must prompt the user.
var ErrRequirePicker = errors.New("routing: no tier matched; show picker")

// mentionPattern matches a leading @agent-id token, allowing [A-Za-z0-9_-]+.
var mentionPattern = regexp.MustCompile(`^\s*@([A-Za-z0-9][A-Za-z0-9_-]*)\b\s*`)

// ParseMention extracts an @agent prefix; returns ("", text, false) if absent.
func ParseMention(text string) (agentID, stripped string, ok bool) {
	m := mentionPattern.FindStringSubmatchIndex(text)
	if m == nil {
		return "", text, false
	}
	id := text[m[2]:m[3]]
	return id, strings.TrimLeft(text[m[1]:], " \t"), true
}

// Resolve walks the §8.1 routing ladder in order. tiers 1 + 2 don't mutate
// state; tiers 3 + 4 write an owner binding on first use so subsequent
// messages skip the lookup. Concurrent tier-3 writers race cleanly: the
// partial unique index on (user_id, thread_id) WHERE binding_type='owner'
// picks a single winner and the loser re-reads the committed row.
func Resolve(ctx context.Context, pool *pgxpool.Pool, userID, threadID string, chatID *int64, text string) (*Resolution, error) {
	// Tier 1: explicit @mention.
	if id, stripped, ok := ParseMention(text); ok {
		return &Resolution{AgentID: id, Tier: TierMention, Text: stripped}, nil
	}

	// Tier 2: owner binding.
	ownerID, err := lookupOwner(ctx, pool, userID, threadID)
	if err != nil {
		return nil, fmt.Errorf("tier 2 owner lookup: %w", err)
	}
	if ownerID != "" {
		return &Resolution{AgentID: ownerID, Tier: TierOwnerBinding, Text: text}, nil
	}

	// Tier 3: global default; write owner binding on match.
	defaultID, err := lookupGlobalDefault(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("tier 3 default lookup: %w", err)
	}
	if defaultID != "" {
		bindingSet, resolvedID, err := writeOwnerBinding(ctx, pool, userID, threadID, chatID, defaultID)
		if err != nil {
			return nil, fmt.Errorf("tier 3 bind: %w", err)
		}
		return &Resolution{AgentID: resolvedID, Tier: TierGlobalDefault, Text: text, BindingSet: bindingSet}, nil
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

// SetUserDefault (/default slash command) is the same as writing an owner
// binding for (user, thread). Overwrites any existing owner.
func SetUserDefault(ctx context.Context, pool *pgxpool.Pool, userID, threadID string, chatID *int64, agentID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		DELETE FROM topic_agent_bindings
		WHERE binding_type='owner' AND user_id=$1 AND thread_id=$2
	`, userID, threadID); err != nil {
		return fmt.Errorf("clear owner: %w", err)
	}
	var threadNum int64
	fmt.Sscanf(threadID, "%d", &threadNum)
	if _, err := tx.Exec(ctx, `
		INSERT INTO topic_agent_bindings (topic_id, agent_id, binding_type, user_id, thread_id, chat_id)
		VALUES ($1, $2, 'owner', $3, $4, $5)
	`, threadNum, agentID, userID, threadID, chatID); err != nil {
		return fmt.Errorf("insert owner: %w", err)
	}
	return tx.Commit(ctx)
}

// SetGlobalDefault (/global-default slash command, admin-only by caller)
// flips agent_settings.is_default for agentID. The unique index enforces
// a single default, so this clears the previous one in the same TX.
func SetGlobalDefault(ctx context.Context, pool *pgxpool.Pool, agentID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE agent_settings SET is_default=FALSE WHERE is_default=TRUE
	`); err != nil {
		return fmt.Errorf("clear default: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_settings (agent_id, is_default) VALUES ($1, TRUE)
		ON CONFLICT (agent_id) DO UPDATE SET is_default=TRUE, updated_at=NOW()
	`, agentID); err != nil {
		return fmt.Errorf("set default: %w", err)
	}
	return tx.Commit(ctx)
}

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

func lookupGlobalDefault(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var id string
	err := pool.QueryRow(ctx, `
		SELECT agent_id FROM agent_settings WHERE is_default=TRUE LIMIT 1
	`).Scan(&id)
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
