package orchestrator

import (
	"context"

	"github.com/maquinista-labs/maquinista/internal/listener"
)

// NotifyBridge bridges listener.TaskEvent to a simple signal channel
// suitable for the orchestrator's NotifyCh. It filters for "ready" and
// "done" status changes.
func NotifyBridge(ctx context.Context, taskEvents <-chan listener.TaskEvent) <-chan struct{} {
	ch := make(chan struct{}, 1)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-taskEvents:
				if !ok {
					return
				}
				if ev.Status == "ready" || ev.Status == "done" {
					// Non-blocking send to wake orchestrator.
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	return ch
}
