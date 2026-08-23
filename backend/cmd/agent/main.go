// Command encode-agent runs the worker on a Windows Server encode node:
// heartbeat loop, job execution via PowerShell, auto-update, reboot
// enforcement. Runs as a Windows service, or -foreground for debugging.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/badskater/encode-system/backend/internal/agent"
)

// Version is stamped at build time: -ldflags "-X main.Version=0.1.0".
var Version = "dev"

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to agent.json")
	foreground := flag.Bool("foreground", false, "run in foreground instead of as a service")
	flag.Parse()

	log := newLogger(*foreground)

	if *foreground {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			log.Error("load config", "err", err)
			os.Exit(1)
		}
		a, err := agent.New(cfg, Version, log)
		if err != nil {
			log.Error("init agent", "err", err)
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := a.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("agent stopped with error", "err", err)
			os.Exit(1)
		}
		return
	}

	// Service mode is platform-specific (Windows SCM vs. plain loop).
	if err := runService(*configPath, Version, log); err != nil {
		log.Error("service error", "err", err)
		os.Exit(1)
	}
}

// defaultConfigPath picks the standard agent.json location per platform.
func defaultConfigPath() string {
	if p := os.Getenv("ENCODE_AGENT_CONFIG"); p != "" {
		return p
	}
	if isWindows() {
		return `C:\encode-agent\agent.json`
	}
	return "./agent.json"
}

// loadConfig reads and validates the agent.json configuration file.
func loadConfig(path string) (agent.Config, error) {
	var cfg agent.Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w (create it or pass -config)", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.DataDir == "" {
		cfg.DataDir = filepath.Dir(path)
	}
	return cfg, nil
}

// newLogger configures JSON logging; foreground also logs to stderr for
// interactive debugging while the service relies on its log file.
func newLogger(foreground bool) *slog.Logger {
	var w *os.File = os.Stdout
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
