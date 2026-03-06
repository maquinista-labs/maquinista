package spec

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleSpec = `---
id: p1-01
title: "Initialize Go module"
priority: 10
depends_on:
  - p0-setup
test_cmd: "go build ./..."
requires_approval: true
---
This is the body of the task.

It has multiple paragraphs.
`

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p1-01.md")
	if err := os.WriteFile(path, []byte(sampleSpec), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if spec.ID != "p1-01" {
		t.Errorf("ID = %q, want %q", spec.ID, "p1-01")
	}
	if spec.Title != "Initialize Go module" {
		t.Errorf("Title = %q, want %q", spec.Title, "Initialize Go module")
	}
	if spec.Priority != 10 {
		t.Errorf("Priority = %d, want 10", spec.Priority)
	}
	if len(spec.DependsOn) != 1 || spec.DependsOn[0] != "p0-setup" {
		t.Errorf("DependsOn = %v, want [p0-setup]", spec.DependsOn)
	}
	if spec.TestCmd != "go build ./..." {
		t.Errorf("TestCmd = %q, want %q", spec.TestCmd, "go build ./...")
	}
	if !spec.RequiresApproval {
		t.Error("RequiresApproval = false, want true")
	}
	if spec.FilePath != path {
		t.Errorf("FilePath = %q, want %q", spec.FilePath, path)
	}
	if spec.Body == "" {
		t.Error("Body is empty")
	}
}

func TestParseFile_MinimalFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minimal.md")
	content := "---\nid: t1\ntitle: Test\n---\nBody text."
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	spec, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if spec.ID != "t1" {
		t.Errorf("ID = %q, want %q", spec.ID, "t1")
	}
	if spec.Priority != 0 {
		t.Errorf("Priority = %d, want 0", spec.Priority)
	}
	if spec.Body != "Body text." {
		t.Errorf("Body = %q, want %q", spec.Body, "Body text.")
	}
}

func TestParseFile_MissingSeparator(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(path, []byte("no frontmatter here"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseFile(path)
	if err == nil {
		t.Error("expected error for missing frontmatter")
	}
}

func TestParseDir(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"a.md", "b.md"} {
		content := "---\nid: " + name[:1] + "\ntitle: " + name + "\n---\nBody."
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Non-md file should be ignored
	if err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("not a spec"), 0644); err != nil {
		t.Fatal(err)
	}

	specs, err := ParseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Errorf("got %d specs, want 2", len(specs))
	}
}

func TestParseDir_Empty(t *testing.T) {
	dir := t.TempDir()
	specs, err := ParseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 0 {
		t.Errorf("got %d specs, want 0", len(specs))
	}
}
