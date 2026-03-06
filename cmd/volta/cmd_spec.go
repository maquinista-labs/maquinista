package main

import (
	"fmt"
	"os"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/otaviocarvalho/volta/internal/spec"
	"github.com/spf13/cobra"
)

var specCmd = &cobra.Command{
	Use:   "spec",
	Short: "Manage task spec files",
}

var (
	specSyncDir     string
	specSyncProject string
	specSyncDryRun  bool
	specSyncRelease bool
)

var specSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync spec files to database tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		proj := specSyncProject
		if proj == "" {
			proj = os.Getenv("VOLTA_PROJECT")
		}
		if proj == "" {
			return fmt.Errorf("--project is required")
		}

		specs, err := spec.ParseDir(specSyncDir)
		if err != nil {
			return fmt.Errorf("parsing specs: %w", err)
		}

		if len(specs) == 0 {
			fmt.Println("No spec files found.")
			return nil
		}

		result, err := spec.Sync(pool, specs, proj, specSyncDryRun)
		if err != nil {
			return fmt.Errorf("syncing specs: %w", err)
		}

		prefix := ""
		if specSyncDryRun {
			prefix = "[DRY RUN] "
		}

		fmt.Printf("%sCreated: %d tasks\n", prefix, len(result.Created))
		for _, id := range result.Created {
			fmt.Printf("  + %s\n", id)
		}

		fmt.Printf("%sUpdated: %d tasks\n", prefix, len(result.Updated))
		for _, id := range result.Updated {
			fmt.Printf("  ~ %s\n", id)
		}

		if len(result.Orphaned) > 0 {
			fmt.Printf("%sOrphaned: %d tasks (in DB but not in specs)\n", prefix, len(result.Orphaned))
			for _, id := range result.Orphaned {
				fmt.Printf("  ? %s\n", id)
			}
		}

		fmt.Printf("%sDependencies set: %d\n", prefix, result.DepsSet)

		if specSyncRelease && !specSyncDryRun {
			count, err := db.DraftReleaseAll(pool, proj)
			if err != nil {
				return fmt.Errorf("releasing tasks: %w", err)
			}
			fmt.Printf("Released %d draft tasks to ready\n", count)
		}

		return nil
	},
}

var specValidateDir string

var specValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate spec files without syncing",
	RunE: func(cmd *cobra.Command, args []string) error {
		specs, err := spec.ParseDir(specValidateDir)
		if err != nil {
			return fmt.Errorf("validating specs: %w", err)
		}

		fmt.Printf("Validated %d spec files\n", len(specs))
		for _, s := range specs {
			deps := ""
			if len(s.DependsOn) > 0 {
				deps = fmt.Sprintf(" (deps: %v)", s.DependsOn)
			}
			fmt.Printf("  ✓ %s: %s%s\n", s.ID, s.Title, deps)
		}
		return nil
	},
}

func init() {
	specSyncCmd.Flags().StringVar(&specSyncDir, "dir", ".specs", "directory of spec files")
	specSyncCmd.Flags().StringVar(&specSyncProject, "project", "", "project ID")
	specSyncCmd.Flags().BoolVar(&specSyncDryRun, "dry-run", false, "show changes without applying")
	specSyncCmd.Flags().BoolVar(&specSyncRelease, "release", false, "release draft tasks after sync")

	specValidateCmd.Flags().StringVar(&specValidateDir, "dir", ".specs", "directory of spec files")

	specCmd.AddCommand(specSyncCmd, specValidateCmd)
	rootCmd.AddCommand(specCmd)
}
