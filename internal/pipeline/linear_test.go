package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captured holds what the stub server saw from one request.
type captured struct {
	auth      string
	ctype     string
	query     string
	vars      map[string]any
	requested int
}

// stubLinear serves one canned response per request and records what arrived.
func stubLinear(t *testing.T, status int, resp string, cap *captured) *LinearClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cap != nil {
			cap.auth = r.Header.Get("Authorization")
			cap.ctype = r.Header.Get("Content-Type")
			cap.requested++
			var body struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			cap.query = body.Query
			cap.vars = body.Variables
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return &LinearClient{HTTPClient: srv.Client(), APIURL: srv.URL, APIKey: "test-key-123"}
}

func TestLinearProvider_AuthHeaders(t *testing.T) {
	var cap captured
	c := stubLinear(t, http.StatusOK, `{"data": {"issues": {"nodes": []}}}`, &cap)
	if _, err := c.TodoIssues(context.Background(), "team-1"); err != nil {
		t.Fatalf("TodoIssues: %v", err)
	}
	if cap.auth != "test-key-123" {
		t.Errorf("Authorization = %q, want raw key %q (no Bearer prefix)", cap.auth, "test-key-123")
	}
	if !strings.HasPrefix(cap.ctype, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", cap.ctype)
	}
}

func TestLinearProvider_GQLErrors(t *testing.T) {
	c := stubLinear(t, http.StatusOK, `{"errors": [{"message": "team not found"}]}`, nil)
	_, err := c.TodoIssues(context.Background(), "team-1")
	if err == nil || !strings.Contains(err.Error(), "team not found") {
		t.Errorf("err = %v, want it to contain the first GraphQL message", err)
	}
}

func TestLinearProvider_HTTPStatus(t *testing.T) {
	c := stubLinear(t, http.StatusBadGateway, `{"errors": [{"message": "nope"}]}`, nil)
	_, err := c.TodoIssues(context.Background(), "team-1")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v, want it to contain the HTTP status 502", err)
	}
}

func TestLinearProvider_FetchTodoDocument(t *testing.T) {
	var cap captured
	c := stubLinear(t, http.StatusOK, `{"data": {"issues": {"nodes": [{
		"id": "u1", "identifier": "MAQ-9", "title": "Ship it",
		"description": "the body", "url": "https://linear.app/maq/MAQ-9"
	}]}}}`, &cap)
	issues, err := c.TodoIssues(context.Background(), "team-maq")
	if err != nil {
		t.Fatalf("TodoIssues: %v", err)
	}
	for _, want := range []string{`state:`, `"Todo"`, `labels:`, `"pipeline"`, `team:`} {
		if !strings.Contains(cap.query, want) {
			t.Errorf("query missing %s in: %s", want, cap.query)
		}
	}
	if cap.vars["t"] != "team-maq" {
		t.Errorf("team variable = %v", cap.vars["t"])
	}
	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1", len(issues))
	}
	got := issues[0]
	if got.ID != "u1" || got.Identifier != "MAQ-9" || got.Title != "Ship it" ||
		got.Description != "the body" || got.URL != "https://linear.app/maq/MAQ-9" {
		t.Errorf("issue round-trip mismatch: %+v", got)
	}
}

func TestLinearProvider_SetIssueState(t *testing.T) {
	var cap captured
	c := stubLinear(t, http.StatusOK, `{"data": {"issueUpdate": {"issue": {"state": {"name": "In Review"}}}}}`, &cap)
	name, err := c.UpdateIssueState(context.Background(), "u1", "s-ir")
	if err != nil {
		t.Fatalf("UpdateIssueState: %v", err)
	}
	if name != "In Review" {
		t.Errorf("resulting state = %q, want In Review (unwrapped envelope)", name)
	}
	for _, want := range []string{"issueUpdate", "stateId"} {
		if !strings.Contains(cap.query, want) {
			t.Errorf("mutation missing %s in: %s", want, cap.query)
		}
	}
	if cap.vars["i"] != "u1" || cap.vars["s"] != "s-ir" {
		t.Errorf("mutation variables = %v", cap.vars)
	}
}

// Backoff delay sanity (C15 arithmetic): 15 s base doubling, 10 min cap.
func TestBackoffDelay(t *testing.T) {
	cases := map[int]time.Duration{
		1: 15 * time.Second,
		2: 30 * time.Second,
		3: 60 * time.Second,
		6: 480 * time.Second,
		7: 10 * time.Minute,
		9: 10 * time.Minute,
		0: 15 * time.Second,
	}
	for failures, want := range cases {
		if got := backoffDelay(failures); got != want {
			t.Errorf("backoffDelay(%d) = %s, want %s", failures, got, want)
		}
	}
}

// TestLinearProvider_KeyFallback pins the credential-resolution contract
// (AC 7): the explicit MAQUINISTA_TICKETS_API_KEY value wins; an empty value
// falls back to the legacy LINEAR_API_KEY so pre-ADR-0006 operator setups
// keep working. Core never reads LINEAR_API_KEY — this lives in-provider.
func TestLinearProvider_KeyFallback(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "legacy-key")

	p, err := NewProvider("linear", "") // empty: fallback must kick in
	if err != nil {
		t.Fatalf("NewProvider linear empty key: %v", err)
	}
	lp, ok := p.(*linearProvider)
	if !ok {
		t.Fatalf("provider type %T, want *linearProvider", p)
	}
	if lp.client.APIKey != "legacy-key" {
		t.Errorf("APIKey = %q, want legacy LINEAR_API_KEY fallback", lp.client.APIKey)
	}

	p2, _ := NewProvider("linear", "explicit-key")
	if p2.(*linearProvider).client.APIKey != "explicit-key" {
		t.Errorf("explicit key overridden, want MAQUINISTA_TICKETS_API_KEY to win")
	}
}
