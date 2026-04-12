package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start Docker postgres container",
	RunE: func(cmd *cobra.Command, args []string) error {
		composePath, err := findComposePath()
		if err != nil {
			return err
		}

		compose, composeArgs := composeCommand(composePath, "up", "-d")
		c := exec.Command(compose, composeArgs...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("docker compose up failed: %w", err)
		}

		// Wait for healthy.
		fmt.Print("Waiting for postgres to be healthy...")
		for i := 0; i < 30; i++ {
			compose, composeArgs := composeCommand(composePath, "ps")
			out, err := exec.Command(compose, composeArgs...).CombinedOutput()
			if err == nil && containsHealthy(string(out)) {
				fmt.Println(" ready")
				fmt.Println("✓ maquinista-postgres started (postgres://maquinista:maquinista@localhost:5433/maquinistadb)")
				return nil
			}
			time.Sleep(time.Second)
			fmt.Print(".")
		}

		fmt.Println(" timeout")
		return fmt.Errorf("postgres did not become healthy within 30s")
	},
}

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop Docker postgres container",
	RunE: func(cmd *cobra.Command, args []string) error {
		composePath, err := findComposePath()
		if err != nil {
			return err
		}

		compose, composeArgs := composeCommand(composePath, "down")
		c := exec.Command(compose, composeArgs...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("docker compose down failed: %w", err)
		}

		fmt.Println("✓ maquinista-postgres stopped")
		return nil
	},
}

func init() {
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(downCmd)
}

// findComposePath locates docker-compose.yml relative to the executable or working dir.
func findComposePath() (string, error) {
	// Try relative to working directory first.
	candidates := []string{
		"docker/docker-compose.yml",
	}

	// Also try relative to the executable.
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "docker", "docker-compose.yml"))
	}

	for _, p := range candidates {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs, nil
			}
		}
	}

	return "", fmt.Errorf("docker/docker-compose.yml not found (run from project root)")
}

// composeCommand returns the command and args for docker compose, trying v2 plugin first then v1.
func composeCommand(composePath string, subArgs ...string) (string, []string) {
	// Try docker compose (v2 plugin).
	if _, err := exec.LookPath("docker"); err == nil {
		args := append([]string{"compose", "-f", composePath}, subArgs...)
		// Test if docker compose works.
		check := exec.Command("docker", "compose", "version")
		if check.Run() == nil {
			return "docker", args
		}
	}

	// Fall back to docker-compose (v1).
	args := append([]string{"-f", composePath}, subArgs...)
	return "docker-compose", args
}

func containsHealthy(s string) bool {
	return strings.Contains(s, "healthy") || strings.Contains(s, "Up")
}
