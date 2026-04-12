// Package webhooks implements Appendix C.3: a POST /hooks/* HTTP ingress
// that looks up a matching webhook_handlers row, verifies its HMAC, renders
// the prompt template against the JSON payload, and enqueues an
// agent_inbox row with from_kind='webhook'. Replay protection is done via
// external_msg_id='hook:<handler_id>:<delivery_id>' + the existing
// UNIQUE (origin_channel, external_msg_id) index.
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/mailbox"
)

// Config bundles server knobs.
type Config struct {
	Addr       string
	MaxBody    int64 // bytes, default 1 MiB
	Now        func() time.Time
}

// DefaultConfig returns production defaults.
func DefaultConfig(addr string) Config {
	return Config{Addr: addr, MaxBody: 1 << 20, Now: time.Now}
}

// Server is stateful: it keeps per-handler token buckets keyed by handler_id.
type Server struct {
	pool    *pgxpool.Pool
	cfg     Config
	buckets sync.Map // handlerID → *bucket
}

// New constructs a Server.
func New(pool *pgxpool.Pool, cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 1 << 20
	}
	return &Server{pool: pool, cfg: cfg}
}

// ServeHTTP routes POST /hooks/* and ignores everything else.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/hooks/") {
		http.NotFound(w, r)
		return
	}
	if err := s.handle(w, r); err != nil {
		// handle writes the status already; log only.
		log.Printf("webhook %s: %v", r.URL.Path, err)
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) error {
	// Read at most MaxBody + 1 to detect oversize.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return err
		}
		http.Error(w, "read body", http.StatusBadRequest)
		return err
	}

	ctx := r.Context()
	handler, err := s.findHandler(ctx, r.URL.Path)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return err
		}
		http.Error(w, "lookup error", http.StatusInternalServerError)
		return err
	}

	// Rate limit per handler.
	if ok := s.allow(handler.id, handler.ratePerMin, s.cfg.Now()); !ok {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return errors.New("rate limited")
	}

	// Verify HMAC.
	if err := verifySignature(handler.signatureScheme, handler.secret, r, body); err != nil {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return fmt.Errorf("signature: %w", err)
	}

	// Parse JSON payload for template + event filter.
	var payload map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return fmt.Errorf("unmarshal: %w", err)
		}
	}

	// Event filter (optional): handler.event_filter is a jsonb object like
	// {"action": "opened"} that must match top-level fields.
	if !passesFilter(handler.eventFilter, payload) {
		w.WriteHeader(http.StatusNoContent)
		return nil
	}

	// Render the prompt template.
	prompt, err := renderTemplate(handler.promptTemplate, payload)
	if err != nil {
		http.Error(w, "template error", http.StatusBadRequest)
		return fmt.Errorf("render: %w", err)
	}

	// Build external_msg_id = "hook:<handler_id>:<delivery_id>".
	deliveryID := r.Header.Get("X-Delivery-Id")
	if deliveryID == "" {
		deliveryID = r.Header.Get("X-GitHub-Delivery")
	}
	if deliveryID == "" {
		deliveryID = uuid.New().String()
	}
	externalID := fmt.Sprintf("hook:%s:%s", handler.id, deliveryID)

	content, _ := json.Marshal(map[string]string{"type": "command", "text": prompt})

	channel, userID, threadID, chatID := unpackReplyChannel(handler.replyChannel)
	if channel == "" {
		channel = "webhook"
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	inboxID, _, err := mailbox.EnqueueInbox(ctx, tx, mailbox.InboxMessage{
		AgentID:        handler.agentID,
		FromKind:       "webhook",
		FromID:         handler.id,
		OriginChannel:  channel,
		OriginUserID:   userID,
		OriginThreadID: threadID,
		OriginChatID:   chatID,
		ExternalMsgID:  externalID,
		Content:        content,
	})
	if err != nil {
		http.Error(w, "enqueue error", http.StatusInternalServerError)
		return fmt.Errorf("enqueue: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "commit error", http.StatusInternalServerError)
		return fmt.Errorf("commit: %w", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"inbox_id": inboxID.String()})
	return nil
}

// Run binds to cfg.Addr and blocks until ctx is cancelled.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	s := New(pool, cfg)
	srv := &http.Server{Addr: cfg.Addr, Handler: s}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ────────────────────────────────────────────────────────────────────────────
// internals

type handlerRow struct {
	id              string
	name            string
	path            string
	secret          string
	signatureScheme string
	eventFilter     []byte
	agentID         string
	promptTemplate  string
	replyChannel    []byte
	ratePerMin      int
}

func (s *Server) findHandler(ctx context.Context, path string) (*handlerRow, error) {
	h := &handlerRow{}
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, name, path, secret, signature_scheme, event_filter,
		       agent_id, prompt_template, reply_channel, rate_limit_per_min
		FROM webhook_handlers
		WHERE path = $1 AND enabled
		LIMIT 1
	`, path).Scan(&h.id, &h.name, &h.path, &h.secret, &h.signatureScheme,
		&h.eventFilter, &h.agentID, &h.promptTemplate, &h.replyChannel,
		&h.ratePerMin)
	if err != nil {
		return nil, err
	}
	return h, nil
}

func verifySignature(scheme, secret string, r *http.Request, body []byte) error {
	switch scheme {
	case "github-hmac-sha256":
		got := r.Header.Get("X-Hub-Signature-256")
		if got == "" {
			return errors.New("missing X-Hub-Signature-256")
		}
		got = strings.TrimPrefix(got, "sha256=")
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(got), []byte(want)) {
			return errors.New("signature mismatch")
		}
		return nil
	default:
		return fmt.Errorf("unknown signature_scheme %q", scheme)
	}
}

func passesFilter(filter []byte, payload map[string]any) bool {
	if len(filter) == 0 || string(filter) == "null" {
		return true
	}
	var f map[string]any
	if err := json.Unmarshal(filter, &f); err != nil {
		return false
	}
	for k, v := range f {
		if payload[k] != v {
			return false
		}
	}
	return true
}

func renderTemplate(tmpl string, payload map[string]any) (string, error) {
	t, err := template.New("prompt").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, payload); err != nil {
		return "", err
	}
	return buf.String(), nil
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
