package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/otaviocarvalho/volta/internal/db"
)

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	workingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	idleStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	doneStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	failedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	readyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	pendingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

type tickMsg time.Time

type model struct {
	pool    *pgxpool.Pool
	agents  []*db.Agent
	tasks   []*db.Task
	err     error
	width   int
	height  int
}

// NewModel creates a new TUI model.
func NewModel(pool *pgxpool.Pool) model {
	return model{pool: pool}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), tea.WindowSize())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		m.agents, m.err = db.ListAgents(m.pool)
		if m.err == nil {
			m.tasks, m.err = db.ListTasks(m.pool, nil)
		}
		return m, tickCmd()
	}
	return m, nil
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v\n\nPress q to quit.", m.err)
	}

	var b strings.Builder

	// Header.
	b.WriteString(headerStyle.Render("Minuano — Agent Watch"))
	b.WriteString("\n\n")

	// Agents table.
	b.WriteString(headerStyle.Render("Agents"))
	b.WriteString("\n")

	if len(m.agents) == 0 {
		b.WriteString(idleStyle.Render("  No agents running."))
		b.WriteString("\n")
	} else {
		b.WriteString(fmt.Sprintf("  %-20s %-10s %-25s %s\n",
			idleStyle.Render("AGENT"),
			idleStyle.Render("STATUS"),
			idleStyle.Render("TASK"),
			idleStyle.Render("LAST SEEN")))

		for _, a := range m.agents {
			sym := "○"
			style := idleStyle
			if a.Status == "working" {
				sym = "●"
				style = workingStyle
			}

			taskID := "—"
			if a.TaskID != nil {
				taskID = truncate(*a.TaskID, 25)
			}

			lastSeen := "—"
			if a.LastSeen != nil {
				lastSeen = relativeTime(*a.LastSeen)
			}

			b.WriteString(fmt.Sprintf("  %s %-19s %-10s %-25s %s\n",
				style.Render(sym),
				style.Render(truncate(a.ID, 19)),
				style.Render(a.Status),
				style.Render(taskID),
				idleStyle.Render(lastSeen)))
		}
	}

	b.WriteString("\n")

	// Task status summary.
	b.WriteString(headerStyle.Render("Tasks"))
	b.WriteString("\n")

	counts := map[string]int{}
	for _, t := range m.tasks {
		counts[t.Status]++
	}

	total := len(m.tasks)
	b.WriteString(fmt.Sprintf("  Total: %d", total))
	if c := counts["done"]; c > 0 {
		b.WriteString(fmt.Sprintf("  %s %d", doneStyle.Render("✓"), c))
	}
	if c := counts["claimed"]; c > 0 {
		b.WriteString(fmt.Sprintf("  %s %d", workingStyle.Render("●"), c))
	}
	if c := counts["ready"]; c > 0 {
		b.WriteString(fmt.Sprintf("  %s %d", readyStyle.Render("◎"), c))
	}
	if c := counts["pending"]; c > 0 {
		b.WriteString(fmt.Sprintf("  %s %d", pendingStyle.Render("○"), c))
	}
	if c := counts["failed"]; c > 0 {
		b.WriteString(fmt.Sprintf("  %s %d", failedStyle.Render("✗"), c))
	}
	b.WriteString("\n")

	b.WriteString("\n")
	b.WriteString(idleStyle.Render("Press q to quit."))
	b.WriteString("\n")

	return b.String()
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

// Run starts the TUI.
func Run(pool *pgxpool.Pool) error {
	p := tea.NewProgram(NewModel(pool), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
