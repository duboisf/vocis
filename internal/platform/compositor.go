package platform

import (
	"context"
	"errors"
)

// Compositor abstracts the small set of display-server primitives the
// dictation injector needs. Each method maps to a single user-visible
// effect; the impls (x11.Compositor, gnome.Compositor) translate to
// xdotool/xclip or D-Bus calls to the vocis-gnome extension.
//
// Splitting these out (vs. handing the injector a backend-specific
// struct) keeps the dictation logic — terminal detection, paste-key
// resolution, mode resolution, modifier release, clipboard save/restore
// — in one place under unit-test coverage with a mock Compositor.
type Compositor interface {
	// CaptureTarget returns the currently-focused window. Called when
	// dictation starts so we can later restore focus and choose the
	// right paste shortcut based on window class.
	CaptureTarget(ctx context.Context) (Target, error)

	// ActivateWindow restores focus to the window described by target.
	// Implementations are expected to wait synchronously when the
	// underlying primitive supports it (xdotool windowactivate --sync);
	// when it doesn't (Mutter window.activate is async), callers should
	// be tolerant of a brief race before the next SendKeys lands.
	ActivateWindow(ctx context.Context, target Target) error

	// SendKeys synthesizes a key combo such as "ctrl+v",
	// "ctrl+shift+v", or "Return". The shape mirrors what vocis writes
	// in config (insertion.default_paste_key etc) so config values can
	// flow straight through.
	SendKeys(ctx context.Context, combo string) error

	// ReleaseModifiers releases the named modifier keys without
	// pressing them first. Used after dictation to drop the still-held
	// hotkey modifiers so the synthesized paste isn't combined with
	// them by the focused app.
	ReleaseModifiers(ctx context.Context, keys []string) error

	// SetClipboard / GetClipboard read and write the system clipboard
	// (CLIPBOARD selection on X11, WL_DATA_DEVICE on Wayland-native
	// apps via the compositor).
	SetClipboard(ctx context.Context, text string) error
	GetClipboard(ctx context.Context) (string, error)

	// Type synthesizes the literal characters of text as keystrokes.
	// Only used by insertion.mode=type and the (currently unused)
	// InsertLive path. Backends that don't implement keysym-by-keysym
	// typing should return a wrapped ErrTypeUnsupported so the injector
	// can degrade to clipboard mode.
	Type(ctx context.Context, target Target, text string, useWindow bool) error
}

// ErrTypeUnsupported is returned by Compositor.Type when the backend
// can't synthesize per-character keystrokes. The injector treats this
// as "fall back to clipboard mode" rather than a hard failure.
var ErrTypeUnsupported = errors.New("compositor does not support typed insertion")

// ErrTargetGone is returned by Injector.Insert when the originally
// captured target window no longer exists at paste time — e.g. the
// user closed the kitty tab while still dictating. The transcript is
// still written to the clipboard before this error is returned, so
// callers should treat it as a recoverable warning ("text saved to
// clipboard, your window is gone") rather than a transcription failure.
var ErrTargetGone = errors.New("target window no longer exists; transcript copied to clipboard")
