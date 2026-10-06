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
	// TeamFilter.id is a strict ID comparator (String! here 400s live);
	// top-level args elsewhere in this file are String! — the asymmetry is
	// real, verified against api.linear.app 02/10/2026.
	doc := `query($t: ID!) { issues(
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

// linearComment mirrors the GraphQL comment shape (MAQ-11). User is a
// reference that can be null (deleted users) and its email is only visible
// when the viewer has the right scope — the provider falls back to the
// display name for the author identity.
type linearComment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	Issue     struct {
		ID string `json:"id"`
	} `json:"issue"`
	User *struct {
		Email       string `json:"email"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"user"`
}

// Comments returns comments created since `since` on the given issues
// (oldest first, capped — the caller's window is minutes, volumes are tiny).
func (c *LinearClient) Comments(ctx context.Context, issueIDs []string, since time.Time) ([]linearComment, error) {
	var out struct {
		Comments struct {
			Nodes []linearComment `json:"nodes"`
		} `json:"comments"`
	}
	// Issue id is an ID comparator ([ID!] for `in`); createdAt takes
	// Linear's DateTimeOrDuration — same strictness as the query above.
	// [UUID!] and DateTime! are type errors here: Linear 400s every tick
	// (MAQ-27), verified against api.linear.app 03/10/2026.
	doc := `query($ids: [ID!], $since: DateTimeOrDuration) { comments(
	  filter: { issue: { id: { in: $ids } }, createdAt: { gte: $since } },
	  orderBy: createdAt, first: 100
	) { nodes { id body createdAt issue { id } user { email name displayName } } } }`
	if err := c.gql(ctx, doc, map[string]any{"ids": issueIDs, "since": since.Format(time.RFC3339Nano)}, &out); err != nil {
		return nil, err
	}
	return out.Comments.Nodes, nil
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
	// Top-level args are String! in Linear's schema; only filter comparators
	// (TeamFilter.id) are strict ID — proven live against api.linear.app
	// 02/10/2026 (ID! on team(id:) 400s; String! on TeamFilter.id.eq 400s).
	doc := `mutation($i: String!, $s: String!) {
	  issueUpdate(id: $i, input: { stateId: $s }) { issue { state { name } } }
	}`
	if err := c.gql(ctx, doc, map[string]any{"i": issueID, "s": stateID}, &out); err != nil {
		return "", err
	}
	return out.IssueUpdate.Issue.State.Name, nil
}

// CommentOnIssue posts a comment on the issue. Used by AddIssueLink: a
// comment is one atomic write (no read-modify-write of the description),
// which keeps the sync loop's exactly-once bookkeeping honest.
func (c *LinearClient) CommentOnIssue(ctx context.Context, issueID, body string) error {
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	doc := `mutation($i: String!, $b: String!) {
	  commentCreate(input: { issueId: $i, body: $b }) { success }
	}`
	return c.gql(ctx, doc, map[string]any{"i": issueID, "b": body}, &out)
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

// AddIssueLink implements TicketProvider: the PR link lands as a comment —
// one atomic write per call, deduped upstream by pr_url_synced.
func (p *linearProvider) AddIssueLink(ctx context.Context, issueID, url string) error {
	return p.client.CommentOnIssue(ctx, issueID, "🔗 PR: "+url)
}

// RecentComments implements CommentFetcher (MAQ-11). Author is the email
// when the API exposes it, else the display name, else the account name.
func (p *linearProvider) RecentComments(ctx context.Context, issueIDs []string, since time.Time) ([]IssueComment, error) {
	if len(issueIDs) == 0 {
		return nil, nil
	}
	nodes, err := p.client.Comments(ctx, issueIDs, since)
	if err != nil {
		return nil, err
	}
	out := make([]IssueComment, 0, len(nodes))
	for _, n := range nodes {
		ic := IssueComment{
			ID:        n.ID,
			IssueID:   n.Issue.ID,
			Body:      n.Body,
			CreatedAt: n.CreatedAt,
		}
		if n.User != nil {
			switch {
			case n.User.Email != "":
				ic.Author = n.User.Email
			case n.User.DisplayName != "":
				ic.Author = n.User.DisplayName
			default:
				ic.Author = n.User.Name
			}
		}
		out = append(out, ic)
	}
	return out, nil
}
