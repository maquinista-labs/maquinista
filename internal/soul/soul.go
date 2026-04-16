// Package soul owns the agent-identity DB surface: templates, per-agent
// rows, and the prompt-rendering pipeline. See
// plans/active/agent-soul-db-state.md for design.
//
// The package is deliberately DB-focused. Spawn-time prompt composition
// (layering tool guidance, memory, env hints) lives in cmd/maquinista; this
// package exposes the raw Render() and the template/agent CRUD that the
// renderer and CLI consume.
package soul

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Soul carries the structured identity of one agent. Shape matches the
// agent_souls row plus a handful of derived rendering helpers.
type Soul struct {
	AgentID         string
	TemplateID      string
	Name            string
	Tagline         string
	Role            string
	Goal            string
	CoreTruths      string
	Boundaries      string
	Vibe            string
	Continuity      string
	Extras          map[string]string
	AllowDelegation bool
	MaxIter         int
	RespectContext  bool
	Version         int
}

// Template mirrors a row in soul_templates.
type Template struct {
	ID              string
	Name            string
	Tagline         string
	Role            string
	Goal            string
	CoreTruths      string
	Boundaries      string
	Vibe            string
	Continuity      string
	Extras          map[string]string
	AllowDelegation bool
	MaxIter         int
	IsDefault       bool
}

// Overrides capture CLI / spawn flags that replace individual sections
// when cloning a template into a fresh agent_souls row.
type Overrides struct {
	Name       *string
	Tagline    *string
	Role       *string
	Goal       *string
	CoreTruths *string
	Boundaries *string
	Vibe       *string
	Continuity *string
	Extras     map[string]string
}

// Querier is the minimal pgx interface this package uses. Allows both
// *pgxpool.Pool and pgx.Tx to be passed in the same signature.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ErrNotFound is returned when a lookup misses.
var ErrNotFound = errors.New("soul: not found")

// LoadTemplate reads a soul_templates row.
func LoadTemplate(ctx context.Context, q Querier, id string) (*Template, error) {
	t := &Template{}
	var extras []byte
	err := q.QueryRow(ctx, `
		SELECT id, name, COALESCE(tagline,''), role, goal,
		       core_truths, boundaries, vibe, continuity,
		       extras, allow_delegation, max_iter, is_default
		FROM soul_templates WHERE id = $1
	`, id).Scan(&t.ID, &t.Name, &t.Tagline, &t.Role, &t.Goal,
		&t.CoreTruths, &t.Boundaries, &t.Vibe, &t.Continuity,
		&extras, &t.AllowDelegation, &t.MaxIter, &t.IsDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load template %s: %w", id, err)
	}
	t.Extras, err = decodeExtras(extras)
	if err != nil {
		return nil, fmt.Errorf("decode extras for %s: %w", id, err)
	}
	return t, nil
}

// LoadDefaultTemplate returns the template with is_default=TRUE. Seed
// migration 016 installs `default` on fresh installs; operators who
// delete it will get ErrNotFound.
func LoadDefaultTemplate(ctx context.Context, q Querier) (*Template, error) {
	return loadDefaultOrByID(ctx, q)
}

func loadDefaultOrByID(ctx context.Context, q Querier) (*Template, error) {
	t := &Template{}
	var extras []byte
	err := q.QueryRow(ctx, `
		SELECT id, name, COALESCE(tagline,''), role, goal,
		       core_truths, boundaries, vibe, continuity,
		       extras, allow_delegation, max_iter, is_default
		FROM soul_templates WHERE is_default LIMIT 1
	`).Scan(&t.ID, &t.Name, &t.Tagline, &t.Role, &t.Goal,
		&t.CoreTruths, &t.Boundaries, &t.Vibe, &t.Continuity,
		&extras, &t.AllowDelegation, &t.MaxIter, &t.IsDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load default template: %w", err)
	}
	t.Extras, err = decodeExtras(extras)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// CreateFromTemplate clones the named template into a new agent_souls row
// for agentID, applying overrides. templateID="" picks the default
// template. On missing default, falls back to an empty-string soul with a
// sane Name/Role/Goal derived from agentID so the row is always created.
// Idempotent — upsert by agent_id.
func CreateFromTemplate(ctx context.Context, q Querier, agentID, templateID string, ov Overrides) error {
	var tpl *Template
	var tplErr error
	if templateID != "" {
		tpl, tplErr = LoadTemplate(ctx, q, templateID)
	} else {
		tpl, tplErr = LoadDefaultTemplate(ctx, q)
	}
	if tplErr != nil && !errors.Is(tplErr, ErrNotFound) {
		return tplErr
	}

	s := Soul{
		AgentID:        agentID,
		TemplateID:     "",
		Name:           agentID,
		Role:           "Agent",
		Goal:           "Follow operator instructions.",
		Extras:         map[string]string{},
		MaxIter:        25,
		RespectContext: true,
		Version:        1,
	}
	if tpl != nil {
		s.TemplateID = tpl.ID
		s.Name = tpl.Name
		s.Tagline = tpl.Tagline
		s.Role = tpl.Role
		s.Goal = tpl.Goal
		s.CoreTruths = tpl.CoreTruths
		s.Boundaries = tpl.Boundaries
		s.Vibe = tpl.Vibe
		s.Continuity = tpl.Continuity
		s.Extras = tpl.Extras
		s.AllowDelegation = tpl.AllowDelegation
		s.MaxIter = tpl.MaxIter
	}
	applyOverrides(&s, ov)
	return Upsert(ctx, q, s)
}

// Upsert inserts or updates an agent_souls row.
func Upsert(ctx context.Context, q Querier, s Soul) error {
	extrasJSON, err := encodeExtras(s.Extras)
	if err != nil {
		return err
	}
	var templateID any
	if s.TemplateID != "" {
		templateID = s.TemplateID
	}
	_, err = q.Exec(ctx, `
		INSERT INTO agent_souls
			(agent_id, template_id, name, tagline, role, goal,
			 core_truths, boundaries, vibe, continuity, extras,
			 allow_delegation, max_iter, respect_context, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12, $13, $14, $15)
		ON CONFLICT (agent_id) DO UPDATE SET
			template_id      = EXCLUDED.template_id,
			name             = EXCLUDED.name,
			tagline          = EXCLUDED.tagline,
			role             = EXCLUDED.role,
			goal             = EXCLUDED.goal,
			core_truths      = EXCLUDED.core_truths,
			boundaries       = EXCLUDED.boundaries,
			vibe             = EXCLUDED.vibe,
			continuity       = EXCLUDED.continuity,
			extras           = EXCLUDED.extras,
			allow_delegation = EXCLUDED.allow_delegation,
			max_iter         = EXCLUDED.max_iter,
			respect_context  = EXCLUDED.respect_context,
			version          = agent_souls.version + 1,
			updated_at       = NOW()
	`, s.AgentID, templateID, s.Name, nullIfEmpty(s.Tagline), s.Role, s.Goal,
		s.CoreTruths, s.Boundaries, s.Vibe, s.Continuity, extrasJSON,
		s.AllowDelegation, s.MaxIter, s.RespectContext, s.Version)
	if err != nil {
		return fmt.Errorf("upsert agent_souls: %w", err)
	}
	return nil
}

// Load reads an agent_souls row.
func Load(ctx context.Context, q Querier, agentID string) (*Soul, error) {
	s := &Soul{AgentID: agentID}
	var templateID any
	var tagline, extras any
	err := q.QueryRow(ctx, `
		SELECT template_id, name, tagline, role, goal,
		       core_truths, boundaries, vibe, continuity,
		       extras, allow_delegation, max_iter, respect_context, version
		FROM agent_souls WHERE agent_id = $1
	`, agentID).Scan(&templateID, &s.Name, &tagline, &s.Role, &s.Goal,
		&s.CoreTruths, &s.Boundaries, &s.Vibe, &s.Continuity,
		&extras, &s.AllowDelegation, &s.MaxIter, &s.RespectContext, &s.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load agent_souls: %w", err)
	}
	if templateID != nil {
		s.TemplateID, _ = templateID.(string)
	}
	if tagline != nil {
		s.Tagline, _ = tagline.(string)
	}
	if b, ok := extras.([]byte); ok {
		s.Extras, err = decodeExtras(b)
		if err != nil {
			return nil, err
		}
	} else {
		s.Extras = map[string]string{}
	}
	return s, nil
}

// Render produces the system-prompt text for a soul. Opens with CrewAI's
// proven role-playing anchor ("You are <name>, a <role>. Your goal is
// …"), then renders the Markdown sections. Empty sections are skipped.
// maxChars truncates the final string with a head/tail-preserving marker
// — 0 means no truncation.
func Render(s Soul, maxChars int) string {
	var out bytes.Buffer
	must := func(err error) {
		if err != nil {
			out.WriteString(fmt.Sprintf("\n[soul render error: %v]\n", err))
		}
	}
	tpl, err := template.New("soul").Parse(soulTemplate)
	must(err)
	if tpl != nil {
		must(tpl.Execute(&out, s))
	}

	rendered := strings.TrimSpace(out.String())
	if maxChars > 0 && len(rendered) > maxChars {
		return truncateHeadTail(rendered, maxChars)
	}
	return rendered
}

const soulTemplate = `# You are {{ .Name }}, a {{ .Role }}.
{{- if .Tagline }}
> {{ .Tagline }}
{{- end }}

**Your goal:** {{ .Goal }}
{{ if .CoreTruths }}
## Core truths
{{ .CoreTruths }}
{{ end -}}
{{ if .Boundaries }}
## Boundaries
{{ .Boundaries }}
{{ end -}}
{{ if .Vibe }}
## Vibe
{{ .Vibe }}
{{ end -}}
{{ if .Continuity }}
## Continuity
{{ .Continuity }}
{{ end -}}
{{ range $k, $v := .Extras }}
## {{ $k }}
{{ $v }}
{{ end -}}
`

func applyOverrides(s *Soul, ov Overrides) {
	if ov.Name != nil {
		s.Name = *ov.Name
	}
	if ov.Tagline != nil {
		s.Tagline = *ov.Tagline
	}
	if ov.Role != nil {
		s.Role = *ov.Role
	}
	if ov.Goal != nil {
		s.Goal = *ov.Goal
	}
	if ov.CoreTruths != nil {
		s.CoreTruths = *ov.CoreTruths
	}
	if ov.Boundaries != nil {
		s.Boundaries = *ov.Boundaries
	}
	if ov.Vibe != nil {
		s.Vibe = *ov.Vibe
	}
	if ov.Continuity != nil {
		s.Continuity = *ov.Continuity
	}
	for k, v := range ov.Extras {
		if s.Extras == nil {
			s.Extras = map[string]string{}
		}
		s.Extras[k] = v
	}
}

func encodeExtras(e map[string]string) ([]byte, error) {
	if e == nil {
		return []byte(`{}`), nil
	}
	return json.Marshal(e)
}

func decodeExtras(raw []byte) (map[string]string, error) {
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	out := map[string]string{}
	// Decode flexibly — extras values may be strings or richer JSON.
	var flex map[string]any
	if err := json.Unmarshal(raw, &flex); err != nil {
		return nil, err
	}
	for k, v := range flex {
		switch typed := v.(type) {
		case string:
			out[k] = typed
		default:
			b, err := json.Marshal(typed)
			if err != nil {
				return nil, err
			}
			out[k] = string(b)
		}
	}
	return out, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// truncateHeadTail preserves the first 70% and last 20% of s with a
// truncation marker in between. Openclaw's bootstrap.ts proven shape.
func truncateHeadTail(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	head := (max * 70) / 100
	tail := (max * 20) / 100
	markerFmt := "\n\n…[truncated %d chars]…\n\n"
	marker := fmt.Sprintf(markerFmt, len(s)-head-tail)
	if head+tail+len(marker) > max {
		// Shrink to fit.
		head = (max - len(marker)) * 7 / 9
		tail = max - len(marker) - head
		if head < 0 {
			head = 0
		}
		if tail < 0 {
			tail = 0
		}
	}
	if head+tail >= len(s) {
		return s[:max]
	}
	return s[:head] + marker + s[len(s)-tail:]
}
