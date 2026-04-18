// Package inject implements the dictation insertion logic — focus
// restore, modifier release, paste vs type mode resolution, terminal
// detection, clipboard save/restore — on top of a Compositor primitive
// set. Both the X11 (xdotool/xclip) and GNOME-Wayland (D-Bus extension)
// backends share this code; only the four-method Compositor surface
// differs.
package inject

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"vocis/internal/config"
	"vocis/internal/hotkey"
	"vocis/internal/platform"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

// Injector implements app.InjectorClient. It is backend-agnostic — all
// platform calls go through the Compositor passed in at construction.
type Injector struct {
	cfg         config.InsertionConfig
	compositor  platform.Compositor
	releaseKeys []string

	mu           sync.Mutex
	restoreTimer *time.Timer
}

// New returns an injector parameterised on a compositor. shortcut is
// the configured global hotkey (e.g. "ctrl+shift+space"); the
// modifier portion is captured so we can release just those keys after
// dictation rather than a generic full-modifier sweep.
func New(cfg config.InsertionConfig, compositor platform.Compositor, shortcut string) *Injector {
	inj := &Injector{
		cfg:         cfg,
		compositor:  compositor,
		releaseKeys: defaultReleaseKeys(),
	}
	if strings.TrimSpace(shortcut) != "" {
		if names, err := hotkey.ReleaseKeyNames(shortcut); err == nil {
			inj.releaseKeys = names
		} else {
			sessionlog.Warnf("resolve hotkey release keys: %v", err)
		}
	}
	return inj
}

// CaptureTarget proxies to the compositor and tags the span with where
// the answer came from (xdotool vs gnome-extension) — useful when
// triaging "wrong window detected" complaints.
func (i *Injector) CaptureTarget(ctx context.Context) (platform.Target, error) {
	return i.compositor.CaptureTarget(ctx)
}

func (i *Injector) Insert(ctx context.Context, target platform.Target, text string) error {
	if !hasVisibleText(text) {
		return nil
	}

	if err := i.focusAndReleaseModifiers(ctx, target); err != nil {
		return err
	}

	mode := i.resolveMode()
	switch mode {
	case "type":
		ctx, typeSpan := telemetry.StartSpan(ctx, "vocis.inject.type",
			attribute.Int("text.length", len(text)),
		)
		err := i.compositor.Type(ctx, target, text, true)
		telemetry.EndSpan(typeSpan, err)
		if err != nil {
			return fmt.Errorf("type text: %w", err)
		}
		return nil
	default:
		return i.paste(ctx, target, text)
	}
}

// InsertLive types a partial segment without doing any clipboard work.
// Currently dead in production but kept on the interface for backward
// compatibility — falls back gracefully if the compositor doesn't
// support typing.
func (i *Injector) InsertLive(ctx context.Context, target platform.Target, text string) error {
	if !hasVisibleText(text) {
		return nil
	}
	if err := i.focusAndReleaseModifiers(ctx, target); err != nil {
		return err
	}
	sessionlog.Infof("typing live segment into window=%s class=%q", target.WindowID, target.WindowClass)
	if err := i.compositor.Type(ctx, target, text, false); err != nil {
		return fmt.Errorf("type live segment: %w", err)
	}
	return nil
}

func (i *Injector) PressEnter(ctx context.Context, target platform.Target) error {
	// 100ms gives the focused app a moment to settle after the paste
	// completes — without it some terminals pick up Enter on the
	// stale focus.
	time.Sleep(100 * time.Millisecond)
	if err := i.compositor.SendKeys(ctx, "Return"); err != nil {
		return fmt.Errorf("press enter: %w", err)
	}
	return nil
}

func (i *Injector) focusAndReleaseModifiers(ctx context.Context, target platform.Target) error {
	ctx, focusSpan := telemetry.StartSpan(ctx, "vocis.inject.focus",
		attribute.String("window.id", target.WindowID),
	)
	if err := i.compositor.ActivateWindow(ctx, target); err != nil {
		telemetry.EndSpan(focusSpan, err)
		return fmt.Errorf("restore focus: %w", err)
	}
	// Brief pause after focus so the next synthesized keypress lands
	// in the newly-focused window — both xdotool --sync and Mutter
	// activate are best-effort about ordering vs the X server / Wayland
	// compositor.
	time.Sleep(120 * time.Millisecond)
	if err := i.compositor.ReleaseModifiers(ctx, i.releaseKeys); err != nil {
		sessionlog.Warnf("release held modifiers: %v", err)
	} else {
		time.Sleep(25 * time.Millisecond)
	}
	telemetry.EndSpan(focusSpan, nil)
	return nil
}

func (i *Injector) paste(ctx context.Context, target platform.Target, text string) error {
	ctx, span := telemetry.StartSpan(ctx, "vocis.inject.paste",
		attribute.Int("text.length", len(text)),
		attribute.Bool("terminal", i.isTerminal(target.WindowClass)),
	)
	defer func() { telemetry.EndSpan(span, nil) }()

	originalClipboard := ""
	if i.cfg.RestoreClipboard {
		// Failure to read the previous clipboard is non-fatal — we just
		// won't restore it. Common when no app currently owns the
		// selection.
		if clip, err := i.compositor.GetClipboard(ctx); err == nil {
			originalClipboard = clip
		}
	}

	if err := i.compositor.SetClipboard(ctx, text); err != nil {
		telemetry.EndSpan(span, err)
		return fmt.Errorf("clipboard write: %w", err)
	}

	pasteKey := i.cfg.DefaultPasteKey
	isTerminal := i.isTerminal(target.WindowClass)
	if isTerminal {
		pasteKey = i.cfg.TerminalPasteKey
	}
	span.SetAttributes(attribute.String("paste.key", pasteKey))
	sessionlog.Infof(
		"pasting transcript into window=%s class=%q terminal=%t key=%s",
		target.WindowID, target.WindowClass, isTerminal, pasteKey,
	)

	if err := i.compositor.SendKeys(ctx, pasteKey); err != nil {
		telemetry.EndSpan(span, err)
		return fmt.Errorf("paste text: %w", err)
	}

	if i.cfg.RestoreClipboard {
		i.scheduleClipboardRestore(originalClipboard)
	}
	return nil
}

// scheduleClipboardRestore writes the saved clipboard back ~250ms
// later, after the focused app has had time to consume the paste. The
// timer is cancellable so back-to-back dictations don't stack restore
// callbacks racing each other.
func (i *Injector) scheduleClipboardRestore(text string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.restoreTimer != nil {
		i.restoreTimer.Stop()
	}
	if text == "" {
		return
	}
	i.restoreTimer = time.AfterFunc(250*time.Millisecond, func() {
		_ = i.compositor.SetClipboard(context.Background(), text)
	})
}

func (i *Injector) resolveMode() string {
	if i.cfg.Mode != "auto" {
		return i.cfg.Mode
	}
	return "clipboard"
}

func (i *Injector) isTerminal(windowClass string) bool {
	for _, candidate := range i.cfg.TerminalClasses {
		if strings.EqualFold(candidate, windowClass) {
			return true
		}
	}
	return false
}

// defaultReleaseKeys covers every modifier vocis recognizes in a
// hotkey shortcut. Used as a safety net when the configured shortcut
// can't be parsed.
func defaultReleaseKeys() []string {
	return []string{
		"Control_L", "Control_R",
		"Shift_L", "Shift_R",
		"Alt_L", "Alt_R",
		"Super_L", "Super_R",
	}
}

func hasVisibleText(text string) bool {
	return strings.TrimSpace(text) != ""
}
