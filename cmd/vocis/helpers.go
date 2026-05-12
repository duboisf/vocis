package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

func findExecutable(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}

// bootCLI is the standard preamble for vocis subcommands that own a
// session log: opens the log, loads (+validates, via config.Load) the
// config, and writes the startup `"vocis <ver> <cmdName> (config=<path>)"`
// line. Caller must `defer session.Close()` once it has the session.
//
// `cmdName` is the human-readable label used in the startup log
// (e.g. "recall start", "transcribe", "speak"). Telemetry and signal
// handling stay in the caller — speak does neither, recall/transcribe
// use bootCLIWithTelemetry below.
func bootCLI(cmdName string) (config.Config, *sessionlog.Session, error) {
	session, err := sessionlog.Start()
	if err != nil {
		return config.Config{}, nil, err
	}
	cfg, path, err := config.Load()
	if err != nil {
		session.Close()
		return config.Config{}, nil, err
	}
	sessionlog.Infof("vocis %s %s (config=%s)", version, cmdName, path)
	return cfg, session, nil
}

// bootCLIWithTelemetry extends bootCLI with the rest of the daemon
// preamble shared by `recall start` and `transcribe`: a SIGINT/SIGTERM-
// aware ctx and OTel initialization. Returns the cfg, the ctx, and a
// cleanup func the caller must defer — order matches the original
// inline defers (telemetry shutdown first, then signal stop, then
// session close).
//
// `serve` doesn't use this helper because its telemetry shutdown is
// wrapped in a 3-second-bounded handler that logs latency, which is
// idiosyncratic to long-running interactive commands.
func bootCLIWithTelemetry(cmdName string) (config.Config, context.Context, func(), error) {
	cfg, session, err := bootCLI(cmdName)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	shutdownTelemetry, err := telemetry.Init(ctx, cfg.Telemetry, version)
	if err != nil {
		stop()
		session.Close()
		return config.Config{}, nil, nil, fmt.Errorf("init telemetry: %w", err)
	}
	cleanup := func() {
		shutdownTelemetry(context.Background())
		stop()
		session.Close()
	}
	return cfg, ctx, cleanup, nil
}
