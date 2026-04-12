package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/dbtest"
)

func setup(t *testing.T) (*pgxpool.Pool, *httptest.Server) {
	t.Helper()
	pool, _ := dbtest.PgContainer(t)
	if _, err := db.RunMigrations(pool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agents (id, tmux_session, tmux_window) VALUES ('alpha','s','w')`); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	srv := httptest.NewServer(New(pool, DefaultConfig("")))
	t.Cleanup(srv.Close)
	return pool, srv
}

func seedHandler(t *testing.T, pool *pgxpool.Pool, path, secret, tmpl string, filter []byte, rate int) {
	t.Helper()
	filterArg := any(nil)
	if len(filter) > 0 {
		filterArg = filter
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO webhook_handlers
		  (name, path, secret, agent_id, prompt_template, event_filter, rate_limit_per_min)
		VALUES ('h-'||$1, $1, $2, 'alpha', $3, $4::jsonb, $5)
	`, path, secret, tmpl, filterArg, rate)
	if err != nil {
		t.Fatalf("seed handler: %v", err)
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func post(t *testing.T, srv *httptest.Server, path string, headers map[string]string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestWebhook_SignedRequestSucceeds(t *testing.T) {
	pool, srv := setup(t)
	seedHandler(t, pool, "/hooks/github/pr", "s3cr3t", "/review-pr {{.number}}", nil, 60)

	body := []byte(`{"number":42,"action":"opened"}`)
	resp := post(t, srv, "/hooks/github/pr", map[string]string{
		"X-Hub-Signature-256": sign("s3cr3t", body),
		"X-GitHub-Delivery":   "del-1",
		"Content-Type":        "application/json",
	}, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status=%d, want 202", resp.StatusCode)
	}

	var out struct{ InboxID string `json:"inbox_id"` }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.InboxID == "" {
		t.Error("no inbox_id in response")
	}

	// Inbox row exists with rendered prompt.
	var content []byte
	pool.QueryRow(context.Background(),
		`SELECT content FROM agent_inbox WHERE external_msg_id='hook:'||(SELECT id FROM webhook_handlers WHERE path='/hooks/github/pr')::text||':del-1'`).
		Scan(&content)
	if !strings.Contains(string(content), "/review-pr 42") {
		t.Errorf("content=%s, want /review-pr 42", content)
	}
}

func TestWebhook_WrongSignatureRejects(t *testing.T) {
	pool, srv := setup(t)
	seedHandler(t, pool, "/hooks/gh", "secret", "x", nil, 60)

	body := []byte(`{}`)
	resp := post(t, srv, "/hooks/gh", map[string]string{
		"X-Hub-Signature-256": "sha256=deadbeef",
	}, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", resp.StatusCode)
	}
}

func TestWebhook_EventFilter_Drops(t *testing.T) {
	pool, srv := setup(t)
	seedHandler(t, pool, "/hooks/gh", "s", "x", []byte(`{"action":"opened"}`), 60)

	for _, action := range []string{"opened", "closed"} {
		body := []byte(fmt.Sprintf(`{"action":%q}`, action))
		resp := post(t, srv, "/hooks/gh", map[string]string{
			"X-Hub-Signature-256": sign("s", body),
			"X-GitHub-Delivery":   "del-" + action,
		}, body)
		resp.Body.Close()
		wantStatus := http.StatusAccepted
		if action != "opened" {
			wantStatus = http.StatusNoContent
		}
		if resp.StatusCode != wantStatus {
			t.Errorf("action=%s status=%d, want %d", action, resp.StatusCode, wantStatus)
		}
	}
}

func TestWebhook_ReplayProtection(t *testing.T) {
	pool, srv := setup(t)
	seedHandler(t, pool, "/hooks/gh", "s", "x", nil, 60)

	body := []byte(`{"n":1}`)
	for i := 0; i < 2; i++ {
		resp := post(t, srv, "/hooks/gh", map[string]string{
			"X-Hub-Signature-256": sign("s", body),
			"X-GitHub-Delivery":   "same-id",
		}, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("iter %d: status=%d", i, resp.StatusCode)
		}
	}

	var count int
	pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM agent_inbox WHERE from_kind='webhook'`).Scan(&count)
	if count != 1 {
		t.Errorf("inbox rows=%d, want 1 (idempotent replay)", count)
	}
}

func TestWebhook_RateLimit(t *testing.T) {
	pool, srv := setup(t)
	seedHandler(t, pool, "/hooks/gh", "s", "x", nil, 2)

	body := []byte(`{}`)
	var codes []int
	for i := 0; i < 4; i++ {
		resp := post(t, srv, "/hooks/gh", map[string]string{
			"X-Hub-Signature-256": sign("s", body),
			"X-GitHub-Delivery":   fmt.Sprintf("del-%d", i),
		}, body)
		resp.Body.Close()
		codes = append(codes, resp.StatusCode)
	}
	// First 2 accepted, then 429.
	if codes[0] != 202 || codes[1] != 202 {
		t.Errorf("first two not accepted: %v", codes)
	}
	if codes[2] != 429 && codes[3] != 429 {
		t.Errorf("no rate-limit seen in %v", codes)
	}
}

func TestWebhook_SizeCapReturns413(t *testing.T) {
	pool, srv := setup(t)
	seedHandler(t, pool, "/hooks/gh", "s", "x", nil, 60)

	huge := bytes.Repeat([]byte{'a'}, 2<<20) // 2 MiB
	resp := post(t, srv, "/hooks/gh", map[string]string{
		"X-Hub-Signature-256": sign("s", huge),
	}, huge)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d, want 413", resp.StatusCode)
	}
	_ = pool
}

func TestWebhook_UnknownPath404(t *testing.T) {
	_, srv := setup(t)
	resp := post(t, srv, "/hooks/nope", map[string]string{"X-Hub-Signature-256": "sha256=x"}, []byte(`{}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
}
