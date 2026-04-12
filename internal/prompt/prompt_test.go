package prompt

import (
	"strings"
	"testing"

	"github.com/maquinista-labs/maquinista/internal/db"
)

func TestBuildSinglePrompt(t *testing.T) {
	task := &db.Task{
		ID:       "t1",
		Title:    "Test Task",
		Body:     "Do the thing.",
		Priority: 5,
	}

	result := BuildSinglePrompt(task, nil)

	if !strings.Contains(result, "Test Task") {
		t.Error("missing title")
	}
	if !strings.Contains(result, "t1") {
		t.Error("missing task ID")
	}
	if !strings.Contains(result, "Do the thing.") {
		t.Error("missing body")
	}
	if !strings.Contains(result, "maquinista-done") {
		t.Error("missing maquinista-done instruction")
	}
}

func TestBuildAutoPrompt(t *testing.T) {
	result := BuildAutoPrompt("myproject")

	if !strings.Contains(result, "myproject") {
		t.Error("missing project name")
	}
	if !strings.Contains(result, "maquinista-claim") {
		t.Error("missing maquinista-claim instruction")
	}
	if !strings.Contains(result, "Auto Mode") {
		t.Error("missing Auto Mode header")
	}
}

func TestBuildBatchPrompt(t *testing.T) {
	entries := []TaskWithContext{
		{Task: &db.Task{ID: "t1", Title: "First", Priority: 5}},
		{Task: &db.Task{ID: "t2", Title: "Second", Priority: 3}},
	}

	result := BuildBatchPrompt(entries)

	if !strings.Contains(result, "Batch Mode") {
		t.Error("missing Batch Mode header")
	}
	if !strings.Contains(result, "First") {
		t.Error("missing first task")
	}
	if !strings.Contains(result, "Second") {
		t.Error("missing second task")
	}
	if !strings.Contains(result, "2 task(s)") {
		t.Error("missing task count")
	}
}

func TestBuildSinglePrompt_WithContext(t *testing.T) {
	task := &db.Task{ID: "t1", Title: "Test", Priority: 1}
	agent := "agent-1"
	ctxs := []*db.TaskContext{
		{Kind: "inherited", AgentID: &agent, Content: "found a bug"},
	}

	result := BuildSinglePrompt(task, ctxs)

	if !strings.Contains(result, "INHERITED") {
		t.Error("missing context kind")
	}
	if !strings.Contains(result, "found a bug") {
		t.Error("missing context content")
	}
}
