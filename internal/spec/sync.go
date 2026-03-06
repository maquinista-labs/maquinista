package spec

import (
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/otaviocarvalho/volta/internal/db"
)

// SyncResult summarizes the sync operation.
type SyncResult struct {
	Created  []string
	Updated  []string
	Orphaned []string
	DepsSet  int
}

// Sync synchronizes spec files with database tasks for a project.
// New tasks are created with status "draft". Existing tasks have their
// title and body updated. Orphaned DB tasks (not in specs) are reported
// but not deleted. Dependencies are resolved within the spec set.
func Sync(pool *pgxpool.Pool, specs []*SpecFile, projectID string, dryRun bool) (*SyncResult, error) {
	// Build lookup of specs by ID.
	specMap := make(map[string]*SpecFile, len(specs))
	for _, s := range specs {
		specMap[s.ID] = s
	}

	// Load existing tasks for this project.
	existing, err := db.ListTasks(pool, &projectID)
	if err != nil {
		return nil, fmt.Errorf("listing existing tasks: %w", err)
	}
	existingMap := make(map[string]*db.Task, len(existing))
	for _, t := range existing {
		existingMap[t.ID] = t
	}

	result := &SyncResult{}

	// Create or update tasks.
	for _, s := range specs {
		if _, exists := existingMap[s.ID]; exists {
			result.Updated = append(result.Updated, s.ID)
			if !dryRun {
				if err := db.UpdateTask(pool, s.ID, s.Title, s.Body); err != nil {
					return nil, fmt.Errorf("updating task %s: %w", s.ID, err)
				}
			}
		} else {
			result.Created = append(result.Created, s.ID)
			if !dryRun {
				var metadata json.RawMessage
				if s.TestCmd != "" {
					metadata, _ = json.Marshal(map[string]string{"test_cmd": s.TestCmd})
				}
				if err := db.CreateTask(pool, s.ID, s.Title, s.Body, s.Priority, &projectID, metadata, s.RequiresApproval); err != nil {
					return nil, fmt.Errorf("creating task %s: %w", s.ID, err)
				}
				// Set status to draft.
				if err := db.SetTaskStatus(pool, s.ID, "draft"); err != nil {
					return nil, fmt.Errorf("setting draft status for %s: %w", s.ID, err)
				}
			}
		}
	}

	// Detect orphaned tasks.
	for id := range existingMap {
		if _, inSpecs := specMap[id]; !inSpecs {
			result.Orphaned = append(result.Orphaned, id)
		}
	}

	// Set dependencies.
	if !dryRun {
		for _, s := range specs {
			for _, dep := range s.DependsOn {
				if err := db.AddDependency(pool, s.ID, dep); err != nil {
					return nil, fmt.Errorf("adding dependency %s -> %s: %w", s.ID, dep, err)
				}
				result.DepsSet++
			}
		}
	}

	return result, nil
}
