package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/maquinista-labs/maquinista/internal/listener"
)

func TestNotifyBridge_ReadyEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	taskEvents := make(chan listener.TaskEvent, 1)
	notifyCh := NotifyBridge(ctx, taskEvents)

	taskEvents <- listener.TaskEvent{TaskID: "t1", Status: "ready"}

	select {
	case <-notifyCh:
		// success
	case <-time.After(time.Second):
		t.Error("expected notify for ready event")
	}
}

func TestNotifyBridge_DoneEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	taskEvents := make(chan listener.TaskEvent, 1)
	notifyCh := NotifyBridge(ctx, taskEvents)

	taskEvents <- listener.TaskEvent{TaskID: "t1", Status: "done"}

	select {
	case <-notifyCh:
		// success
	case <-time.After(time.Second):
		t.Error("expected notify for done event")
	}
}

func TestNotifyBridge_IgnoresOtherStatuses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	taskEvents := make(chan listener.TaskEvent, 1)
	notifyCh := NotifyBridge(ctx, taskEvents)

	taskEvents <- listener.TaskEvent{TaskID: "t1", Status: "claimed"}

	select {
	case <-notifyCh:
		t.Error("should not notify for claimed status")
	case <-time.After(100 * time.Millisecond):
		// success
	}
}

func TestNotifyBridge_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	taskEvents := make(chan listener.TaskEvent, 1)
	_ = NotifyBridge(ctx, taskEvents)

	cancel()
	// Just verify it doesn't hang or panic.
	time.Sleep(50 * time.Millisecond)
}
