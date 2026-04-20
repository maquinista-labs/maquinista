package bot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/pbkdf2"
)

const (
	dashUserPbkdf2Iter    = 600_000
	dashUserPbkdf2KeyLen  = 32
	dashUserPbkdf2SaltLen = 16
)

// handleDashboardUser routes /dashboard_user subcommands.
//
// Usage:
//
//	/dashboard_user add <username> <password>
func (b *Bot) handleDashboardUser(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	threadID := getThreadID(msg)

	parts := strings.Fields(strings.TrimSpace(msg.CommandArguments()))
	if len(parts) == 0 {
		b.reply(chatID, threadID, "Usage: /dashboard_user add <username> <password>")
		return
	}

	switch parts[0] {
	case "add":
		if len(parts) < 3 {
			b.reply(chatID, threadID, "Usage: /dashboard_user add <username> <password>")
			return
		}
		username := parts[1]
		password := parts[2]

		pool := b.getPool()
		if pool == nil {
			b.reply(chatID, threadID, "Database not available — cannot create user.")
			return
		}

		id, err := createOperatorCredential(context.Background(), pool, username, password)
		if err != nil {
			log.Printf("dashboard_user: create operator: %v", err)
			b.reply(chatID, threadID, fmt.Sprintf("Failed to create user: %v", err))
			return
		}

		log.Printf("dashboard_user: created operator %q (id=%s)", username, id)
		b.reply(chatID, threadID, fmt.Sprintf(
			"✅ User %q created.\nThey can log in at the dashboard with that password.", username,
		))

	default:
		b.reply(chatID, threadID, fmt.Sprintf(
			"Unknown subcommand %q.\nUsage: /dashboard_user add <username> <password>", parts[0],
		))
	}
}

// createOperatorCredential inserts a new row into operator_credentials and
// returns the new operator UUID. Returns an error if the username already
// exists or the DB write fails.
func createOperatorCredential(ctx context.Context, pool *pgxpool.Pool, username, password string) (string, error) {
	hash, salt, err := hashDashboardPassword(password)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}

	id := uuid.New().String()
	now := time.Now().UTC()

	_, err = pool.Exec(ctx, `
		INSERT INTO operator_credentials
			(id, username, pbkdf2_hash, salt, iter, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, username, hash, salt, dashUserPbkdf2Iter, now)
	if err != nil {
		return "", fmt.Errorf("inserting operator: %w", err)
	}

	return id, nil
}

// hashDashboardPassword produces a (pbkdf2_hash, salt) pair compatible with
// auth.ts hashPassword() / verifyPassword():
//
//   - salt is a random 16-byte value hex-encoded (32 hex chars).
//   - pbkdf2_hash is the hex-encoded 32-byte PBKDF2-SHA256 key derived using
//     the hex-encoded salt string as the raw salt bytes — matching the Node
//     pbkdf2Sync call in auth.ts which passes the hex string directly.
//
// The two values are stored separately in operator_credentials (pbkdf2_hash,
// salt columns) so verifyPassword can reconstruct the key without parsing a
// combined format string.
func hashDashboardPassword(password string) (hash, salt string, err error) {
	saltBytes := make([]byte, dashUserPbkdf2SaltLen)
	if _, err = rand.Read(saltBytes); err != nil {
		return "", "", fmt.Errorf("generating salt: %w", err)
	}
	// Encode salt to hex. The hex string itself is used as the PBKDF2 salt
	// (as UTF-8 bytes), matching Node's pbkdf2Sync which receives the hex
	// string and treats it as raw bytes.
	salt = hex.EncodeToString(saltBytes)
	key := pbkdf2.Key([]byte(password), []byte(salt), dashUserPbkdf2Iter, dashUserPbkdf2KeyLen, sha256.New)
	hash = hex.EncodeToString(key)
	return hash, salt, nil
}
