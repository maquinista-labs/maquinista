package daemonize

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// TailLogs prints the daemon's log file to w. If follow is true,
// tails the file until ctx is cancelled — new content is streamed as
// it's appended and file truncation / recreation is handled.
//
// If the log file doesn't exist yet:
//   - follow=false prints a "no log file" banner and returns nil.
//   - follow=true prints a "waiting for <path>" banner and polls
//     until the file appears (or ctx fires).
//
// Polling interval is 100 ms. The implementation deliberately avoids
// fsnotify to keep the dependency graph small; dashboard and
// orchestrator log files grow append-only, so a poll loop is plenty.
func TailLogs(ctx context.Context, spec Spec, follow bool, w io.Writer) error {
	path := spec.LogPath
	if path == "" {
		return fmt.Errorf("%s: LogPath is required", spec.Name)
	}

	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("opening %s: %w", path, err)
		}
		if !follow {
			fmt.Fprintf(w, "%s: no log file at %s (start the daemon first)\n", spec.Name, path)
			return nil
		}
		fmt.Fprintf(w, "%s: waiting for %s to appear\n", spec.Name, path)
		f, err = waitForLog(ctx, path)
		if err != nil {
			return err
		}
	}
	defer f.Close()

	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if !follow {
		return nil
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		// Detect truncation / recreation: if the file's current size
		// is less than our offset, reopen from the top.
		cur, err := f.Seek(0, 1) // SEEK_CUR
		if err != nil {
			return fmt.Errorf("seek: %w", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			// File was removed — keep polling rather than erroring.
			continue
		}
		if info.Size() < cur {
			f.Close()
			f, err = os.Open(path)
			if err != nil {
				return fmt.Errorf("reopen %s: %w", path, err)
			}
		}

		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
		}
	}
}

// waitForLog polls for path to exist, returning the opened file on
// success or ctx.Err() if the context fires first.
func waitForLog(ctx context.Context, path string) (*os.File, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("opening %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
