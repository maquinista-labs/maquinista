// Package pipeline implements the Linear ↔ maquinista bridge (ADR-0005):
// intake (Linear Todo issues → task rows) and linearSync (Postgres task
// state transitions → Linear board, with retry). Agents never call Linear
// directly — the determinism boundary is the tasks state machine.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultLinearAPIURL = "https://api.linear.app/graphql"

// Issue is the slice of a Linear issue the bridge needs.
type Issue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

// LinearAPI is the surface the bridge and sync loops need; *LinearClient
// implements it against the real API, tests substitute fakes.
type LinearAPI interface {
	TodoIssues(ctx context.Context, teamID string) ([]Issue, error)
	WorkflowStates(ctx context.Context, teamID string) (map[string]string, error)
	UpdateIssueState(ctx context.Context, issueID, stateID string) (string, error)
}

// LinearClient is a minimal Linear GraphQL client. APIURL is overridable for
// tests (httptest stubs).
type LinearClient struct {
	HTTPClient *http.Client
	APIURL     string
	APIKey     string
}

// NewLinearClient builds a client for the public Linear API.
func NewLinearClient(apiKey string) *LinearClient {
	return &LinearClient{
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		APIURL:     defaultLinearAPIURL,
		APIKey:     apiKey,
	}
}

// gql executes a GraphQL document and unwraps the data envelope into out.
func (c *LinearClient) gql(ctx context.Context, doc string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": doc, "variables": vars})
	if err != nil {
		return fmt.Errorf("pipeline: encoding query: %w", err)
	}
	api := c.APIURL
	if api == "" {
		api = defaultLinearAPIURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pipeline: building request: %w", err)
	}
	// Linear documents raw-key Authorization (no Bearer prefix).
	req.Header.Set("Authorization", c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pipeline: linear api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("pipeline: linear api status %d: %s", resp.StatusCode, string(b))
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("pipeline: decoding linear response: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("pipeline: linear api error: %s", env.Errors[0].Message)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("pipeline: decoding linear data: %w", err)
		}
	}
	return nil
}

// TodoIssues returns the team's issues in workflow state "Todo" carrying the
// "pipeline" label — the intake set for the claim loop.
func (c *LinearClient) TodoIssues(ctx context.Context, teamID string) ([]Issue, error) {
	var out struct {
		Issues struct {
			Nodes []Issue `json:"nodes"`
		} `json:"issues"`
	}
	doc := `query($t: String!) { issues(
	  filter: {
	    team:   { id: { eq: $t } },
	    state:  { name: { eq: "Todo" } },
	    labels: { name: { eq: "pipeline" } }
	  }
	) { nodes { id identifier title description url } } }`
	if err := c.gql(ctx, doc, map[string]any{"t": teamID}, &out); err != nil {
		return nil, err
	}
	return out.Issues.Nodes, nil
}

// WorkflowStates returns the team's workflow states as name → id.
func (c *LinearClient) WorkflowStates(ctx context.Context, teamID string) (map[string]string, error) {
	var out struct {
		Team struct {
			States struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"states"`
		} `json:"team"`
	}
	doc := `query($t: String!) { team(id: $t) { states { nodes { id name } } } }`
	if err := c.gql(ctx, doc, map[string]any{"t": teamID}, &out); err != nil {
		return nil, err
	}
	m := make(map[string]string, len(out.Team.States.Nodes))
	for _, n := range out.Team.States.Nodes {
		m[n.Name] = n.ID
	}
	return m, nil
}

// UpdateIssueState moves an issue to the state with the given id and returns
// the issue's resulting state name.
func (c *LinearClient) UpdateIssueState(ctx context.Context, issueID, stateID string) (string, error) {
	var out struct {
		IssueUpdate struct {
			Issue struct {
				State struct {
					Name string `json:"name"`
				} `json:"state"`
			} `json:"issue"`
		} `json:"issueUpdate"`
	}
	doc := `mutation($i: String!, $s: String!) {
	  issueUpdate(id: $i, input: { stateId: $s }) { issue { state { name } } }
	}`
	if err := c.gql(ctx, doc, map[string]any{"i": issueID, "s": stateID}, &out); err != nil {
		return "", err
	}
	return out.IssueUpdate.Issue.State.Name, nil
}
