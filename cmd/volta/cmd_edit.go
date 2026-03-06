package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/otaviocarvalho/volta/internal/db"
	"github.com/spf13/cobra"
)

var editCmd = &cobra.Command{
	Use:   "edit <id>",
	Short: "Open task body in $EDITOR",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := connectDB(); err != nil {
			return err
		}

		task, err := db.GetTask(pool, args[0])
		if err != nil {
			return err
		}

		// Write body to temp file.
		tmp, err := os.CreateTemp("", "volta-edit-*.md")
		if err != nil {
			return fmt.Errorf("creating temp file: %w", err)
		}
		tmpPath := tmp.Name()
		defer os.Remove(tmpPath)

		if _, err := tmp.WriteString(task.Body); err != nil {
			tmp.Close()
			return fmt.Errorf("writing temp file: %w", err)
		}
		tmp.Close()

		// Open editor.
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		c := exec.Command(editor, tmpPath)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("editor failed: %w", err)
		}

		// Read back.
		newBody, err := os.ReadFile(tmpPath)
		if err != nil {
			return fmt.Errorf("reading temp file: %w", err)
		}

		// No-op if unchanged.
		if string(newBody) == task.Body {
			fmt.Println("No changes.")
			return nil
		}

		if err := db.UpdateTask(pool, task.ID, task.Title, string(newBody)); err != nil {
			return err
		}

		fmt.Printf("✓ Updated body for %s\n", task.ID)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(editCmd)
}
