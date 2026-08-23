//go:build windows

package main

import (
	"context"
	"log/slog"

	"golang.org/x/sys/windows/svc"

	"github.com/badskater/encode-system/backend/internal/agent"
)

const serviceName = "encode-agent"

func isWindows() bool { return true }

// runService registers with the Windows Service Control Manager and runs the
// agent loop, stopping cleanly on service stop requests.
func runService(configPath, version string, log *slog.Logger) error {
	handler := &agentSvc{configPath: configPath, version: version, log: log}
	return svc.Run(serviceName, handler)
}

type agentSvc struct {
	configPath string
	version    string
	log        *slog.Logger
}

// Execute implements svc.Handler. It starts the agent, then bridges SCM
// control requests to agent lifecycle until stop is requested.
func (s *agentSvc) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	cfg, err := loadConfig(s.configPath)
	if err != nil {
		s.log.Error("load config", "err", err)
		changes <- svc.Status{State: svc.StopPending}
		return false, 1
	}
	a, err := agent.New(cfg, s.version, s.log)
	if err != nil {
		s.log.Error("init agent", "err", err)
		changes <- svc.Status{State: svc.StopPending}
		return false, 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}
	for {
		select {
		case req := <-r:
			switch req.Cmd {
			case svc.Interrogate:
				changes <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				a.Stop()
				cancel()
				<-done
				return false, 0
			}
		case err := <-done:
			if err != nil && ctx.Err() == nil {
				s.log.Error("agent loop exited", "err", err)
				return false, 1
			}
			changes <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
}
