package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/otaviocarvalho/volta/internal/db"
)

func TestPrintTreeNode(t *testing.T) {
	// Capture stdout.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	root := &db.TreeNode{
		Task: &db.Task{ID: "root-task", Title: "Root Task", Status: "done"},
		Children: []*db.TreeNode{
			{
				Task:     &db.Task{ID: "child-a", Title: "Child A", Status: "ready"},
				Children: nil,
			},
			{
				Task:     &db.Task{ID: "child-b", Title: "Child B", Status: "pending"},
				Children: nil,
			},
		},
	}

	printTreeNode(root, "", true)

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	os.Stdout = old

	output := buf.String()

	// Root should appear.
	if !strings.Contains(output, "root-task") {
		t.Errorf("expected root-task in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Root Task") {
		t.Errorf("expected 'Root Task' in output, got:\n%s", output)
	}
	// Children should appear.
	if !strings.Contains(output, "child-a") {
		t.Errorf("expected child-a in output, got:\n%s", output)
	}
	if !strings.Contains(output, "child-b") {
		t.Errorf("expected child-b in output, got:\n%s", output)
	}
	// Status symbols should appear.
	if !strings.Contains(output, "✓") {
		t.Errorf("expected done symbol ✓ in output, got:\n%s", output)
	}
	if !strings.Contains(output, "◎") {
		t.Errorf("expected ready symbol ◎ in output, got:\n%s", output)
	}
	// Tree connectors should appear.
	if !strings.Contains(output, "├") && !strings.Contains(output, "└") {
		t.Errorf("expected tree connectors in output, got:\n%s", output)
	}
}

func TestTreeCommandRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Use == "tree" {
			return
		}
	}
	t.Error("expected 'tree' command to be registered")
}

func TestTreeCommandFlags(t *testing.T) {
	if treeCmd.Flags().Lookup("project") == nil {
		t.Error("expected --project flag on tree command")
	}
}
