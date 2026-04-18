package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"vocis/internal/app"
	"vocis/internal/audio"
	"vocis/internal/config"
	"vocis/internal/platform"
	"vocis/internal/platform/gnome"
	"vocis/internal/platform/inject"
	x11 "vocis/internal/platform/x11"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the voice-to-text service",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServe()
	},
}

func runServe() error {
	session, err := sessionlog.Start()
	if err != nil {
		return err
	}
	defer session.Close()

	sessionlog.Infof("vocis %s", version)
	sessionlog.Infof("session log: %s", session.Path())

	cfg, path, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.Init(ctx, cfg.Telemetry, version)
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	defer shutdownTelemetry(context.Background())

	if cfg.Telemetry.Enabled {
		sessionlog.Infof("telemetry enabled, exporting to %s", cfg.Telemetry.Endpoint)
	}

	sessionlog.Infof("loaded config: %s", path)
	sessionlog.Infof("hotkey: %s", cfg.Hotkey)

	ov, err := x11.NewOverlay(cfg.Overlay)
	if err != nil {
		return fmt.Errorf("init overlay: %w", err)
	}

	compositor, compositorBackend := pickCompositor()
	sessionlog.Infof("compositor backend: %s", compositorBackend)

	registrar, backend := pickHotkeyRegistrar()
	return app.New(cfg, app.Deps{
		Overlay:        ov,
		Injector:       inject.New(cfg.Insertion, compositor, cfg.Hotkey),
		Ducker:         audio.NewDucker(cfg.Recording.DuckVolume),
		RegisterHotkey: registrar,
		HotkeyBackend:  backend,
	}).Run(ctx)
}

// pickCompositor returns the platform.Compositor matching the current
// session. On Wayland with the vocis-gnome extension reachable we use
// the gnome compositor (no xdotool/xclip dependency). Otherwise we use
// the X11 compositor which shells out to xdotool/xclip.
func pickCompositor() (platform.Compositor, string) {
	if isWaylandSession() {
		if c, err := gnome.NewCompositor(); err == nil {
			return c, "gnome-extension"
		} else if !gnome.IsExtensionUnreachable(err) {
			sessionlog.Warnf("gnome compositor: %v — falling back to x11", err)
		} else {
			sessionlog.Warnf("compositor: vocis-gnome extension not detected on session bus, falling back to x11/xdotool")
		}
	}
	return x11.NewCompositor(), "x11"
}

// pickHotkeyRegistrar selects a global hotkey backend based on the running
// session. On Wayland, X11 grabs do not see native Wayland keystrokes, so we
// prefer the vocis-gnome shell extension if it's installed and reachable on
// the session bus. Falls back to X11 (XGrabKey via XWayland) otherwise — that
// fallback only works for X11/XWayland focused windows on Wayland sessions.
//
// The returned label ("gnome-extension" or "x11") is passed to app.Deps so
// the root trace span records which backend a session used.
func pickHotkeyRegistrar() (app.HotkeyRegistrar, string) {
	if isWaylandSession() {
		if gnome.Available() {
			sessionlog.Infof("hotkey backend: vocis-gnome shell extension")
			return func(shortcut string) (app.HotkeySource, error) {
				return gnome.Register(shortcut)
			}, "gnome-extension"
		}
		sessionlog.Warnf("hotkey backend: vocis-gnome extension not detected on session bus, falling back to x11/XGrabKey (will not see Wayland-native keys)")
	}
	sessionlog.Infof("hotkey backend: x11 (XGrabKey)")
	return func(shortcut string) (app.HotkeySource, error) {
		return x11.Register(shortcut)
	}, "x11"
}

func isWaylandSession() bool {
	if strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") {
		return true
	}
	return os.Getenv("WAYLAND_DISPLAY") != ""
}
