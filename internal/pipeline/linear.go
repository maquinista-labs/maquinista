// Linear ticket provider (ADR-0006): the only file in the pipeline package
// that speaks Linear. Implements TicketProvider over the GraphQL transport
// below; the canonical Column names happen to equal this board's Linear
// column names, which keeps stored state readable and diff-compatible.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const defaultLinearAPIURL = "https://api.linear.app/graphql"

// linearIssue mirrors the GraphQL issue shape.
type linearIssue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

func (i linearIssue) toIssue() Issue {
	return Issue{ID: i.ID, Key: i.Identifier, Title: i.Title, Description: i.Description, URL: i.URL}
}

// linearColumnNames maps canonical columns to the MAQ board's Linear columns.
var linearColumnNames = map[Column]string{
	ColInProgress:       "In Progress",
	ColInReview:         "In Review",
	ColChangesRequested: "Changes Requested",
	ColNeedsHuman:       "Needs Human",
	ColReadyToMerge:     "Ready to Merge",
	ColDone:             "Done",
}

// LinearClient is a minimal Linear GraphQL client (transport only — the
// provider methods below are what the pipeline core consumes). APIURL is
// overridable for tests (httptest stubs).
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
// "pipeline" label — the intake queue for the claim loop.
func (c *LinearClient) TodoIssues(ctx context.Context, teamID string) ([]linearIssue, error) {
	var out struct {
		Issues struct {
			Nodes []linearIssue `json:"nodes"`
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

// linearProvider implements TicketProvider over the Linear API.
type linearProvider struct {
	client *LinearClient
}

// newLinearProvider resolves the credential: callers pass
// MAQUINISTA_TICKETS_API_KEY; an empty value falls back to Linear's
// documented LINEAR_API_KEY so operator setups that predate ADR-0006 keep
// working without reconfiguration.
func newLinearProvider(apiKey string) *linearProvider {
	if apiKey == "" {
		apiKey = os.Getenv("LINEAR_API_KEY")
	}
	return &linearProvider{client: NewLinearClient(apiKey)}
}

// IntakeIssues implements TicketProvider.
func (p *linearProvider) IntakeIssues(ctx context.Context, teamID string) ([]Issue, error) {
	nodes, err := p.client.TodoIssues(ctx, teamID)
	if err != nil {
		return nil, err
	}
	issues := make([]Issue, 0, len(nodes))
	for _, n := range nodes {
		issues = append(issues, n.toIssue())
	}
	return issues, nil
}

// Columns implements TicketProvider: canonical → Linear workflow-state ID.
func (p *linearProvider) Columns(ctx context.Context, teamID string) (map[Column]string, error) {
	states, err := p.client.WorkflowStates(ctx, teamID)
	if err != nil {
		return nil, err
	}
	out := make(map[Column]string, len(linearColumnNames))
	for col, name := range linearColumnNames {
		if id, ok := states[name]; ok {
			out[col] = id
		}
	}
	return out, nil
}

// SetIssueColumn implements TicketProvider.
func (p *linearProvider) SetIssueColumn(ctx context.Context, issueID, columnID string) error {
	_, err := p.client.UpdateIssueState(ctx, issueID, columnID)
	return err
}
