//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/badskater/encode-system/backend/internal/agent"
)

func isWindows() bool { return false }

// runService on non-Windows hosts runs a plain foreground loop (used for
// development and testing of the agent on Linux).
func runService(configPath, version string, log *slog.Logger) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	a, err := agent.New(cfg, version, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}
