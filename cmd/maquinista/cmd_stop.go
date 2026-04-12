package main

import (
	"fmt"
	"log"
	"os"
	"syscall"
	"time"

	"github.com/maquinista-labs/maquinista/internal/agent"
	"github.com/maquinista-labs/maquinista/internal/config"
	"github.com/maquinista-labs/maquinista/internal/db"
	"github.com/maquinista-labs/maquinista/internal/tmux"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running Maquinista daemon and clean up resources",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStop()
	},
}

func runStop() error {
	pid, err := readPIDFile()
	if err != nil && !os.IsNotExist(err) {
		log.Printf("Warning: reading PID file: %v", err)
	}

	// Best-effort cleanup: resolve config for tmux session name.
	cfg, cfgErr := config.Load()

	sessionName := "maquinista"
	if cfgErr == nil {
		sessionName = cfg.TmuxSessionName
	}

	hasSession := tmux.SessionExists(sessionName)

	// If there's no PID, no tmux session — Maquinista is already stopped.
	if pid == 0 && !hasSession {
		removePIDFile() // clean up stale file if any
		log.Println("Maquinista is not running.")
		return nil
	}

	if pid != 0 && processAlive(pid) {
		log.Printf("Sending SIGTERM to maquinista (PID %d)...", pid)
		proc, err := os.FindProcess(pid)
		if err != nil {
			return fmt.Errorf("finding process %d: %w", pid, err)
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			log.Printf("Warning: sending SIGTERM: %v", err)
		} else {
			// Wait up to 10s for process to exit.
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if !processAlive(pid) {
					log.Println("Maquinista process exited.")
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if processAlive(pid) {
				log.Printf("Process %d still alive after 10s, sending SIGKILL...", pid)
				_ = proc.Signal(syscall.SIGKILL)
			}
		}
	} else if pid != 0 {
		log.Printf("PID %d is not running (stale PID file).", pid)
	}

	// Clean up PID file.
	removePIDFile()

	if hasSession {
		log.Printf("Killing tmux session %q...", sessionName)
		if err := tmux.KillSession(sessionName); err != nil {
			log.Printf("Warning: killing tmux session: %v", err)
		}
	}

	// Kill DB agents if DATABASE_URL is available.
	dbURL := os.Getenv("DATABASE_URL")
	if cfgErr == nil && cfg.DatabaseURL != "" {
		dbURL = cfg.DatabaseURL
	}
	if dbURL != "" {
		cleanPool, err := db.Connect(dbURL)
		if err != nil {
			log.Printf("Warning: connecting to DB for agent cleanup: %v", err)
		} else {
			if err := agent.KillAll(cleanPool, sessionName); err != nil {
				log.Printf("Warning: killing DB agents: %v", err)
			}
			cleanPool.Close()
		}
	}

	log.Println("Maquinista stopped.")
	return nil
}
