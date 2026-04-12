package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/spf13/cobra"
)

var (
	addAfter            []string
	addPriority         int
	addTestCmd          string
	addProject          string
	addBody             string
	addStatus           string
	addRequiresApproval bool
)

var addCmd = &cobra.Command{
	Use:   "add <title>",
	Short: "Create a task",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		// Validate --status flag.
		if addStatus != "ready" && addStatus != "draft" {
			return fmt.Errorf("invalid --status %q: must be 'ready' or 'draft'", addStatus)
		}

		title := strings.Join(args, " ")
		id := generateID(title)

		projectID := addProject
		if projectID == "" {
			projectID = os.Getenv("MAQUINISTA_PROJECT")
		}
		var projPtr *string
		if projectID != "" {
			projPtr = &projectID
		}

		var metadata json.RawMessage
		if addTestCmd != "" {
			m := map[string]string{"test_cmd": addTestCmd}
			metadata, _ = json.Marshal(m)
		}

		if err := db.CreateTask(pool, id, title, addBody, addPriority, projPtr, metadata, addRequiresApproval); err != nil {
			return err
		}

		// Add dependencies.
		for _, dep := range addAfter {
			resolvedDep, err := db.ResolvePartialID(pool, dep)
			if err != nil {
				return fmt.Errorf("resolving dependency %q: %w", dep, err)
			}
			if err := db.AddDependency(pool, id, resolvedDep); err != nil {
				return err
			}
		}

		// Set status based on --status flag and deps.
		if addStatus == "draft" {
			// Draft tasks stay draft regardless of deps.
			if err := db.SetTaskStatus(pool, id, "draft"); err != nil {
				return err
			}
		} else if len(addAfter) > 0 {
			hasUnmet, err := db.HasUnmetDeps(pool, id)
			if err != nil {
				return err
			}
			if !hasUnmet {
				if err := db.SetTaskStatus(pool, id, "ready"); err != nil {
					return err
				}
			}
			// Otherwise stays 'pending' (default).
		} else {
			if err := db.SetTaskStatus(pool, id, "ready"); err != nil {
				return err
			}
		}

		fmt.Printf("Created: %s  %q\n", id, title)
		return nil
	},
}

func init() {
	addCmd.Flags().StringSliceVar(&addAfter, "after", nil, "dependency task ID (partial ok, repeatable)")
	addCmd.Flags().IntVar(&addPriority, "priority", 5, "priority 0-10")
	addCmd.Flags().StringVar(&addTestCmd, "test-cmd", "", "test command override")
	addCmd.Flags().StringVar(&addProject, "project", "", "project ID (or MAQUINISTA_PROJECT env)")
	addCmd.Flags().StringVar(&addBody, "body", "", "task body/specification")
	addCmd.Flags().StringVar(&addStatus, "status", "ready", "initial task status: ready, draft")
	addCmd.Flags().BoolVar(&addRequiresApproval, "requires-approval", false, "require human approval before execution")
	rootCmd.AddCommand(addCmd)
}

// generateID creates a slug from the title plus a random suffix.
func generateID(title string) string {
	slug := slugify(title)
	if len(slug) > 15 {
		slug = slug[:15]
	}
	suffix := randomHex(3)
	return slug + "-" + suffix
}

func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)[:n+2]
}
