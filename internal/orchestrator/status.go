package orchestrator

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maquinista-labs/maquinista/internal/db"
)

// OrchestratorStatus holds a snapshot of the orchestrator's state.
type OrchestratorStatus struct {
	ActiveAgents  int
	IdleAgents    int
	WorkingAgents int
	PlannerAgents int
	ReadyTasks    int
	DoneTasks     int
	FailedTasks   int
	ClaimedTasks  int
	MergeQueue    int
}

// String returns a human-readable status summary.
func (s OrchestratorStatus) String() string {
	plannerStr := ""
	if s.PlannerAgents > 0 {
		plannerStr = fmt.Sprintf(", %d planners", s.PlannerAgents)
	}
	return fmt.Sprintf(
		"Agents: %d active (%d idle, %d working%s) | Tasks: %d ready, %d claimed, %d done, %d failed | Merge queue: %d",
		s.ActiveAgents, s.IdleAgents, s.WorkingAgents, plannerStr,
		s.ReadyTasks, s.ClaimedTasks, s.DoneTasks, s.FailedTasks,
		s.MergeQueue,
	)
}

// Status queries the current orchestrator state from the database.
func Status(pool *pgxpool.Pool, projectID *string) (*OrchestratorStatus, error) {
	s := &OrchestratorStatus{}

	// Count agents by status.
	agents, err := db.ListAgents(pool)
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}
	for _, a := range agents {
		if a.Role == "planner" {
			s.PlannerAgents++
			s.ActiveAgents++
			continue
		}
		switch a.Status {
		case "idle":
			s.IdleAgents++
			s.ActiveAgents++
		case "working":
			s.WorkingAgents++
			s.ActiveAgents++
		default:
			s.ActiveAgents++
		}
	}

	// Count tasks by status.
	tasks, err := db.ListTasks(pool, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	for _, t := range tasks {
		switch t.Status {
		case "ready":
			s.ReadyTasks++
		case "done":
			s.DoneTasks++
		case "failed":
			s.FailedTasks++
		case "claimed":
			s.ClaimedTasks++
		}
	}

	// Count merge queue entries.
	mergeEntries, err := db.ListMergeQueue(pool)
	if err != nil {
		return nil, fmt.Errorf("listing merge queue: %w", err)
	}
	s.MergeQueue = len(mergeEntries)

	return s, nil
}
