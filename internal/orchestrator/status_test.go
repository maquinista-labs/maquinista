package orchestrator

import (
	"strings"
	"testing"
)

func TestOrchestratorStatus_String(t *testing.T) {
	s := OrchestratorStatus{
		ActiveAgents:  3,
		IdleAgents:    1,
		WorkingAgents: 2,
		ReadyTasks:    5,
		DoneTasks:     10,
		FailedTasks:   1,
		ClaimedTasks:  2,
		MergeQueue:    3,
	}

	str := s.String()

	if !strings.Contains(str, "3 active") {
		t.Error("missing active agents")
	}
	if !strings.Contains(str, "5 ready") {
		t.Error("missing ready tasks")
	}
	if !strings.Contains(str, "10 done") {
		t.Error("missing done tasks")
	}
	if !strings.Contains(str, "Merge queue: 3") {
		t.Error("missing merge queue")
	}
}

func TestOrchestratorStatus_Empty(t *testing.T) {
	s := OrchestratorStatus{}
	str := s.String()
	if !strings.Contains(str, "0 active") {
		t.Error("expected zeros in empty status")
	}
}
