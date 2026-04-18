package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/maquinista-labs/maquinista/internal/daemonize"
	"github.com/spf13/cobra"
)

// Per plans/active/detached-processes.md, the top-level `logs`
// command tails orchestrator + dashboard log files. Without
// --component it interleaves both streams with `[orch]` / `[dash]`
// prefixes and a wall-clock timestamp. With --component it
// delegates to daemonize.TailLogs so operators get the exact file
// content (no prefix rewriting).
//
// The pre-D.5 `maquinista logs <agent-id>` (tmux-pane capture) moved
// to `maquinista agent logs <agent-id>` — see cmd_agent.go.

var (
	logsFollow    bool
	logsComponent string
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Tail orchestrator + dashboard log files (use --component for one)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runDaemonLogs(cmd.Context(), logsFollow, logsComponent, cmd.OutOrStdout())
	},
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "follow the log files as they grow")
	logsCmd.Flags().StringVar(&logsComponent, "component", "", "only tail one component (orchestrator|dashboard)")
	rootCmd.AddCommand(logsCmd)
}

// runDaemonLogs is the public entry point for the `maquinista logs`
// command. It resolves the production specs and delegates to
// runDaemonLogsFor so tests can inject synthetic specs without
// touching ~/.maquinista.
func runDaemonLogs(ctx context.Context, follow bool, component string, out io.Writer) error {
	return runDaemonLogsFor(ctx, follow, component, out, orchestratorSpec(), dashboardSpec())
}

func runDaemonLogsFor(ctx context.Context, follow bool, component string, out io.Writer, orchSpec, dashSpec daemonize.Spec) error {
	switch component {
	case "":
		// fall through to interleaved.
	case "orchestrator", "orch":
		return daemonize.TailLogs(ctx, orchSpec, follow, out)
	case "dashboard", "dash":
		return daemonize.TailLogs(ctx, dashSpec, follow, out)
	default:
		return fmt.Errorf("unknown component %q (want orchestrator|dashboard)", component)
	}

	// Interleaved mode: one goroutine per file, each writing line-
	// at-a-time through a prefixWriter into a shared channel. The
	// main goroutine drains the channel in the order lines arrive.
	ch := make(chan string, 256)
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	tail := func(name string, spec daemonize.Spec) {
		defer wg.Done()
		pw := &prefixLineWriter{ch: ch, tag: name}
		err := daemonize.TailLogs(ctx, spec, follow, pw)
		pw.flush()
		if err != nil && ctx.Err() == nil {
			errs <- fmt.Errorf("%s: %w", name, err)
		}
	}

	wg.Add(2)
	go tail("orch", orchSpec)
	go tail("dash", dashSpec)

	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()

	for {
		select {
		case <-doneCh:
			// Drain any remaining lines buffered before workers exited.
			for {
				select {
				case line := <-ch:
					_, _ = io.WriteString(out, line)
				default:
					close(errs)
					for e := range errs {
						return e
					}
					return nil
				}
			}
		case <-ctx.Done():
			// Workers will observe ctx.Done via TailLogs and exit.
			// Keep draining until they do.
			go func() { <-doneCh }()
		case line := <-ch:
			if _, err := io.WriteString(out, line); err != nil {
				return err
			}
		}
	}
}

// prefixLineWriter chunks incoming writes on '\n', prefixes each
// complete line with `[HH:MM:SS tag] `, and emits into ch. Partial
// writes buffer until the next newline. flush() emits a trailing
// partial line at shutdown.
type prefixLineWriter struct {
	ch  chan<- string
	tag string
	mu  sync.Mutex
	buf []byte
}

func (p *prefixLineWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		idx := bytes.IndexByte(p.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(p.buf[:idx+1])
		p.buf = p.buf[idx+1:]
		p.ch <- p.stamp() + line
	}
	return len(b), nil
}

func (p *prefixLineWriter) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) == 0 {
		return
	}
	p.ch <- p.stamp() + string(p.buf) + "\n"
	p.buf = nil
}

func (p *prefixLineWriter) stamp() string {
	return fmt.Sprintf("[%s %s] ", time.Now().Format("15:04:05"), p.tag)
}

