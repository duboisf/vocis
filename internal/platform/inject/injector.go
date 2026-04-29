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
	"vocis/internal/platform/kitty"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

// Injector implements app.InjectorClient. It is backend-agnostic — all
// platform calls go through the Compositor passed in at construction.
type Injector struct {
	cfg         config.InsertionConfig
	compositor  platform.Compositor
	releaseKeys []string

	// kittyFocusedID / kittyExists / kittySendText / kittySendEnter are
	// seams for tests so we don't have to shell out to a real kitty
	// during unit tests. The default implementations call the kitty
	// package directly.
	kittyFocusedID func(ctx context.Context) (string, error)
	kittyExists    func(ctx context.Context, id string) (bool, error)
	kittySendText  func(ctx context.Context, id, text string) error
	kittySendEnter func(ctx context.Context, id string) error

	mu           sync.Mutex
	restoreTimer *time.Timer
}

// New returns an injector parameterised on a compositor. shortcut is
// the configured global hotkey (e.g. "ctrl+shift+space"); the
// modifier portion is captured so we can release just those keys after
// dictation rather than a generic full-modifier sweep.
func New(cfg config.InsertionConfig, compositor platform.Compositor, shortcut string) *Injector {
	inj := &Injector{
		cfg:            cfg,
		compositor:     compositor,
		releaseKeys:    defaultReleaseKeys(),
		kittyFocusedID: kitty.FocusedWindowID,
		kittyExists:    kitty.Exists,
		kittySendText:  kitty.SendText,
		kittySendEnter: kitty.SendEnter,
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

// KittyHooks groups the four kitty CLI shell-outs the injector uses,
// so tests can swap them all at once via SetKittyHooks. Any field
// left nil keeps the Injector's existing implementation for that hook.
type KittyHooks struct {
	FocusedID func(ctx context.Context) (string, error)
	Exists    func(ctx context.Context, id string) (bool, error)
	SendText  func(ctx context.Context, id, text string) error
	SendEnter func(ctx context.Context, id string) error
}

// SetKittyHooks swaps the kitty CLI shell-outs for fakes — used by
// tests to assert tab-aware delivery behavior without invoking real
// kitty subprocesses.
func (i *Injector) SetKittyHooks(h KittyHooks) {
	if h.FocusedID != nil {
		i.kittyFocusedID = h.FocusedID
	}
	if h.Exists != nil {
		i.kittyExists = h.Exists
	}
	if h.SendText != nil {
		i.kittySendText = h.SendText
	}
	if h.SendEnter != nil {
		i.kittySendEnter = h.SendEnter
	}
}

// CaptureTarget proxies to the compositor and tags the span with where
// the answer came from (xdotool vs gnome-extension) — useful when
// triaging "wrong window detected" complaints. When the captured target
// is a kitty terminal and kitty remote control is enabled in config,
// the result is enriched with the focused kitty internal window id so
// paste-time focus can drill down to a specific tab/pane.
func (i *Injector) CaptureTarget(ctx context.Context) (platform.Target, error) {
	target, err := i.compositor.CaptureTarget(ctx)
	if err != nil {
		return target, err
	}
	if !i.cfg.KittyRemoteControl || !kitty.IsKitty(target.WindowClass) {
		return target, nil
	}
	id, kerr := i.kittyFocusedID(ctx)
	if kerr != nil {
		// Common when kitty isn't on PATH, allow_remote_control is off,
		// or no socket is reachable. Fall through to OS-window focus —
		// the user just doesn't get tab-aware paste.
		sessionlog.Warnf(
			"kitty target detected but `kitty @ ls` failed: %v "+
				"(falling back to OS-window focus; enable allow_remote_control / listen_on in kitty.conf for tab-aware paste)",
			kerr,
		)
		return target, nil
	}
	if id == "" {
		sessionlog.Infof("kitty target window=%s but no kitty @ ls focus match — using OS-window focus only",
			target.WindowID)
		return target, nil
	}
	target.KittyWindowID = id
	sessionlog.Infof("captured kitty window id=%s for OS window=%s class=%q",
		id, target.WindowID, target.WindowClass)
	return target, nil
}

func (i *Injector) Insert(ctx context.Context, target platform.Target, text string) error {
	if !hasVisibleText(text) {
		return nil
	}

	// Kitty fast path: deliver the transcript via `kitty @ send-text`
	// directly into the originally-targeted tab/pane WITHOUT changing
	// focus. This is strictly better than focus + paste:
	//   - it doesn't disrupt the user's current keyboard focus (they
	//     may have moved on to another window mid-recording),
	//   - it doesn't pollute the clipboard, and
	//   - it always lands in the original tab/pane, not whichever
	//     tab/pane happens to be active right now.
	// On a "window gone" reply we fall back to clipboard delivery and
	// surface ErrTargetGone; on an unexpected CLI error we degrade to
	// the OS-window focus + paste flow so dictation still completes.
	if target.KittyWindowID != "" {
		handled, err := i.deliverViaKittyDirect(ctx, target, text)
		if err != nil {
			return err
		}
		if handled {
			return nil
		}
		// fall through to focus + paste fallback
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

// deliverViaKittyDirect tries to push text to the captured kitty
// window without changing keyboard focus. Returns:
//
//	(true,  nil)               — text delivered. No further work needed.
//	(false, ErrTargetGone)     — kitty window is gone; transcript was
//	                             written to the clipboard for the user
//	                             to recover manually.
//	(false, nil)               — kitty CLI hit an unexpected error; the
//	                             caller should fall back to focus + paste.
func (i *Injector) deliverViaKittyDirect(ctx context.Context, target platform.Target, text string) (bool, error) {
	ctx, span := telemetry.StartSpan(ctx, "vocis.inject.kitty_direct",
		attribute.String("kitty.window_id", target.KittyWindowID),
		attribute.Int("text.length", len(text)),
	)
	defer func() { telemetry.EndSpan(span, nil) }()

	exists, err := i.kittyExists(ctx, target.KittyWindowID)
	if err != nil {
		span.SetAttributes(attribute.String("kitty.exists.error", err.Error()))
		sessionlog.Warnf("kitty exists-check id=%s failed (%v) — falling back to OS-window focus + paste",
			target.KittyWindowID, err)
		return false, nil
	}
	if !exists {
		span.SetAttributes(attribute.Bool("kitty.target_gone", true))
		if cerr := i.compositor.SetClipboard(ctx, text); cerr != nil {
			sessionlog.Warnf("kitty target gone and clipboard write failed: %v", cerr)
		} else {
			sessionlog.Infof("kitty window id=%s gone — wrote %d-char transcript to clipboard",
				target.KittyWindowID, len(text))
		}
		return false, platform.ErrTargetGone
	}
	if err := i.kittySendText(ctx, target.KittyWindowID, text); err != nil {
		span.SetAttributes(attribute.String("kitty.send_text.error", err.Error()))
		sessionlog.Warnf("kitty send-text id=%s failed (%v) — falling back to OS-window focus + paste",
			target.KittyWindowID, err)
		return false, nil
	}
	sessionlog.Infof("delivered %d-char transcript to kitty window id=%s via send-text (no focus change)",
		len(text), target.KittyWindowID)
	span.SetAttributes(attribute.Bool("kitty.delivered", true))
	return true, nil
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
	// Kitty path: route the submit Enter through the same focus-free
	// remote-control channel as the text payload. Otherwise sending
	// Enter via the compositor (xdotool / Mutter) would land in
	// whichever window currently has keyboard focus — which is exactly
	// what we just went out of our way to avoid by using send-text.
	if target.KittyWindowID != "" {
		ctx, span := telemetry.StartSpan(ctx, "vocis.inject.press_enter",
			attribute.String("kitty.window_id", target.KittyWindowID),
			attribute.String("path", "kitty_direct"),
		)
		err := i.kittySendEnter(ctx, target.KittyWindowID)
		telemetry.EndSpan(span, err)
		if err == nil {
			sessionlog.Infof("submit: pressed Enter on kitty window id=%s via send-text \\r (no focus change)",
				target.KittyWindowID)
			return nil
		}
		sessionlog.Warnf("kitty send-enter id=%s failed (%v) — falling back to compositor Return",
			target.KittyWindowID, err)
	}
	// 100ms gives the focused app a moment to settle after the paste
	// completes — without it some terminals pick up Enter on the
	// stale focus.
	time.Sleep(100 * time.Millisecond)
	ctx, span := telemetry.StartSpan(ctx, "vocis.inject.press_enter",
		attribute.String("path", "compositor"),
	)
	err := i.compositor.SendKeys(ctx, "Return")
	telemetry.EndSpan(span, err)
	if err != nil {
		return fmt.Errorf("press enter: %w", err)
	}
	sessionlog.Infof("submit: pressed Enter via compositor (target window=%s)", target.WindowID)
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
