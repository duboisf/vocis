package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"vocis/internal/app"
	"vocis/internal/config"
	"vocis/internal/recorder"
	"vocis/internal/securestore"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		args = []string{"serve"}
	}

	switch args[0] {
	case "serve":
		if err := runServe(); err != nil {
			sessionlog.Errorf("serve: %v", err)
			return 1
		}
		return 0
	case "init":
		if err := runInit(); err != nil {
			sessionlog.Errorf("init: %v", err)
			return 1
		}
		return 0
	case "doctor":
		if err := runDoctor(); err != nil {
			sessionlog.Errorf("doctor: %v", err)
			return 1
		}
		return 0
	case "key":
		if err := runKey(args[1:]); err != nil {
			sessionlog.Errorf("key: %v", err)
			return 1
		}
		return 0
	case "help", "-h", "--help":
		printUsage()
		return 0
	default:
		printUsage()
		return 1
	}
}

func runServe() error {
	session, err := sessionlog.Start()
	if err != nil {
		return err
	}
	defer session.Close()

	sessionlog.Infof("session log: %s", session.Path())

	cfg, path, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.Init(ctx, cfg.Telemetry)
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	defer shutdownTelemetry(context.Background())

	if cfg.Telemetry.Enabled {
		sessionlog.Infof("telemetry enabled, exporting to %s", cfg.Telemetry.Endpoint)
	}

	sessionlog.Infof("loaded config: %s", path)
	sessionlog.Infof("hotkey: %s", cfg.Hotkey)

	return app.New(cfg).Run(ctx)
}

func runInit() error {
	path, err := config.InitDefault()
	if err != nil {
		return err
	}

	fmt.Printf("wrote %s\n", path)
	return nil
}

func runDoctor() error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	logDir, err := sessionlog.Dir()
	if err != nil {
		return err
	}

	checks := []struct {
		label string
		value string
		ok    bool
	}{
		{label: "display", value: os.Getenv("DISPLAY"), ok: os.Getenv("DISPLAY") != ""},
	}

	for _, check := range checks {
		status := "ok"
		if !check.ok {
			status = "missing"
		}
		fmt.Printf("%-14s %s (%s)\n", check.label, status, check.value)
	}

	for _, cmd := range []string{"xdotool", "xclip"} {
		path, ok := findExecutable(cmd)
		status := "ok"
		if !ok {
			status = "missing"
		}
		fmt.Printf("%-14s %s (%s)\n", cmd, status, path)
	}
	audioStatus := "ok"
	audioValue := "pulse server"
	if err := recorder.Check(); err != nil {
		audioStatus = "missing"
		audioValue = err.Error()
	}
	fmt.Printf("%-14s %s (%s)\n", "audio", audioStatus, audioValue)

	if _, err := os.Stat(cfgPath); err == nil {
		fmt.Printf("%-14s ok (%s)\n", "config", cfgPath)
	} else {
		fmt.Printf("%-14s missing (%s)\n", "config", cfgPath)
	}
	fmt.Printf("%-14s ok (%s)\n", "log-dir", logDir)

	store := securestore.New()
	if _, err := store.APIKey(); err == nil {
		fmt.Printf("%-14s ok (keyring or env)\n", "openai-key")
	} else {
		fmt.Printf("%-14s missing (%v)\n", "openai-key", err)
	}

	return nil
}

func runKey(args []string) error {
	if len(args) == 0 {
		return errors.New("expected subcommand: set, clear, show-source")
	}

	store := securestore.New()

	switch args[0] {
	case "set":
		key, err := readSecret()
		if err != nil {
			return err
		}
		if err := store.SetAPIKey(key); err != nil {
			return err
		}
		fmt.Println("stored OpenAI API key in the system keyring")
		return nil
	case "clear":
		if err := store.ClearAPIKey(); err != nil {
			return err
		}
		fmt.Println("removed OpenAI API key from the system keyring")
		return nil
	case "show-source":
		source, err := store.Source()
		if err != nil {
			return err
		}
		fmt.Println(source)
		return nil
	default:
		return fmt.Errorf("unknown key subcommand %q", args[0])
	}
}

func readSecret() (string, error) {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return "", err
	}

	if (stat.Mode() & os.ModeCharDevice) == 0 {
		const maxKeySize = 1024
		buf, err := io.ReadAll(io.LimitReader(os.Stdin, maxKeySize))
		if err != nil {
			return "", err
		}
		defer clearBytes(buf)
		return strings.TrimSpace(string(buf)), nil
	}

	fmt.Fprint(os.Stderr, "OpenAI API key: ")
	buf, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	defer clearBytes(buf)
	return strings.TrimSpace(string(buf)), nil
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func printUsage() {
	fmt.Println(`vocis — Latin: "of voice" (pronounced WOH-kiss)

Usage:
  vocis serve
  vocis init
  vocis doctor
  vocis key set
  vocis key clear
  vocis key show-source`)
}

func findExecutable(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}
