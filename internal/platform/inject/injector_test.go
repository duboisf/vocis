package inject

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"vocis/internal/config"
	"vocis/internal/platform"
)

// fakeCompositor records every call as a single string for ordering
// assertions. It returns the canned clipboard value when GetClipboard is
// called so RestoreClipboard timing can be exercised without touching a
// real selection.
type fakeCompositor struct {
	mu          sync.Mutex
	calls       []string
	clipboard   string
	typeReturns error
}

func (f *fakeCompositor) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeCompositor) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeCompositor) CaptureTarget(_ context.Context) (platform.Target, error) {
	f.record("CaptureTarget")
	return platform.Target{WindowID: "1", WindowClass: "test"}, nil
}

func (f *fakeCompositor) ActivateWindow(_ context.Context, target platform.Target) error {
	f.record("ActivateWindow " + target.WindowID)
	return nil
}

func (f *fakeCompositor) SendKeys(_ context.Context, combo string) error {
	f.record("SendKeys " + combo)
	return nil
}

func (f *fakeCompositor) ReleaseModifiers(_ context.Context, keys []string) error {
	f.record("ReleaseModifiers " + strings.Join(keys, ","))
	return nil
}

func (f *fakeCompositor) SetClipboard(_ context.Context, text string) error {
	f.record("SetClipboard " + text)
	f.clipboard = text
	return nil
}

func (f *fakeCompositor) GetClipboard(_ context.Context) (string, error) {
	f.record("GetClipboard")
	return f.clipboard, nil
}

func (f *fakeCompositor) Type(_ context.Context, _ platform.Target, text string, useWindow bool) error {
	if f.typeReturns != nil {
		return f.typeReturns
	}
	f.record("Type " + text)
	return nil
}

func TestTerminalDetectionIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	inj := New(config.Default().Insertion, &fakeCompositor{}, "")
	if !inj.isTerminal("alacritty") {
		t.Fatal("expected alacritty to be treated as a terminal (case-insensitive match against TerminalClasses)")
	}
}

// TestInsertReleasesModifiersBeforePasting locks down the order of
// operations on the paste path:
//   1. activate target window
//   2. release the still-held hotkey modifiers (otherwise the paste
//      shortcut combines with them)
//   3. snapshot clipboard, write transcript, send paste shortcut
//
// Two ActivateWindow calls are expected — once in focusAndReleaseModifiers
// for the focus pre-roll, and the paste path doesn't re-activate (we
// trust the first focus to stick). The pre-paste GetClipboard / SetClipboard
// pair must straddle SendKeys.
func TestInsertReleasesModifiersBeforePasting(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Insertion
	cfg.Mode = "clipboard"
	fc := &fakeCompositor{clipboard: "previous"}
	inj := New(cfg, fc, "ctrl+shift+space")

	err := inj.Insert(
		context.Background(),
		platform.Target{WindowID: "42", WindowClass: "kitty"},
		"hello world",
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	calls := fc.snapshot()
	want := []string{
		"ActivateWindow 42",
		"ReleaseModifiers Control_L,Control_R,Shift_L,Shift_R,space",
		"GetClipboard",
		"SetClipboard hello world",
		"SendKeys ctrl+shift+v", // kitty is in TerminalClasses → terminal_paste_key
	}
	if len(calls) != len(want) {
		t.Fatalf("len(calls)=%d want %d; calls=%v", len(calls), len(want), calls)
	}
	for i, w := range want {
		if calls[i] != w {
			t.Fatalf("calls[%d]=%q want %q; calls=%v", i, calls[i], w, calls)
		}
	}
}

func TestInsertNonTerminalUsesDefaultPasteKey(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Insertion
	cfg.Mode = "clipboard"
	fc := &fakeCompositor{}
	inj := New(cfg, fc, "ctrl+shift+space")

	if err := inj.Insert(
		context.Background(),
		platform.Target{WindowID: "9", WindowClass: "Firefox"},
		"hi",
	); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	calls := fc.snapshot()
	last := calls[len(calls)-1]
	if last != "SendKeys ctrl+v" {
		t.Fatalf("last call = %q, want SendKeys ctrl+v; calls=%v", last, calls)
	}
}

func TestInsertTypeMode(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Insertion
	cfg.Mode = "type"
	fc := &fakeCompositor{}
	inj := New(cfg, fc, "ctrl+shift+space")

	if err := inj.Insert(
		context.Background(),
		platform.Target{WindowID: "5", WindowClass: "Firefox"},
		"hi there",
	); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	calls := fc.snapshot()
	if calls[len(calls)-1] != "Type hi there" {
		t.Fatalf("last call = %q, want Type hi there; calls=%v", calls[len(calls)-1], calls)
	}
}

func TestPressEnterEmitsReturn(t *testing.T) {
	t.Parallel()

	fc := &fakeCompositor{}
	inj := New(config.Default().Insertion, fc, "")
	if err := inj.PressEnter(context.Background(), platform.Target{}); err != nil {
		t.Fatalf("PressEnter: %v", err)
	}
	calls := fc.snapshot()
	if len(calls) != 1 || calls[0] != "SendKeys Return" {
		t.Fatalf("calls=%v, want [SendKeys Return]", calls)
	}
}

func TestReleaseKeysFromShortcut(t *testing.T) {
	t.Parallel()

	fc := &fakeCompositor{}
	cfg := config.Default().Insertion
	cfg.Mode = "clipboard"
	inj := New(cfg, fc, "ctrl+shift+space")

	_ = inj.Insert(context.Background(), platform.Target{WindowID: "1", WindowClass: "Firefox"}, "x")
	for _, c := range fc.snapshot() {
		if strings.HasPrefix(c, "ReleaseModifiers ") {
			if !strings.Contains(c, "space") {
				t.Fatalf("expected release keys derived from shortcut to include trigger key; got %q", c)
			}
			return
		}
	}
	t.Fatal("ReleaseModifiers call never observed")
}

func TestEmptyTextSkipsCompositor(t *testing.T) {
	t.Parallel()

	fc := &fakeCompositor{}
	inj := New(config.Default().Insertion, fc, "")
	if err := inj.Insert(context.Background(), platform.Target{WindowID: "1"}, "   "); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if len(fc.snapshot()) != 0 {
		t.Fatalf("expected no compositor calls for whitespace text; calls=%v", fc.snapshot())
	}
}

// TestCaptureTargetEnrichesKittyWindow checks that when the captured
// window is a kitty terminal and remote control is enabled, the
// injector calls our kitty hook and stores the returned id on the
// target.
func TestCaptureTargetEnrichesKittyWindow(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Insertion
	cfg.KittyRemoteControl = true
	fc := &fakeKittyCompositor{class: "kitty"}
	inj := New(cfg, fc, "")
	inj.SetKittyHooks(KittyHooks{
		FocusedID: func(ctx context.Context) (string, error) { return "91", nil },
	})

	target, err := inj.CaptureTarget(context.Background())
	if err != nil {
		t.Fatalf("CaptureTarget: %v", err)
	}
	if target.KittyWindowID != "91" {
		t.Fatalf("KittyWindowID = %q, want \"91\"", target.KittyWindowID)
	}
}

func TestCaptureTargetSkipsKittyWhenDisabled(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Insertion
	cfg.KittyRemoteControl = false
	fc := &fakeKittyCompositor{class: "kitty"}
	inj := New(cfg, fc, "")
	called := false
	inj.SetKittyHooks(KittyHooks{
		FocusedID: func(ctx context.Context) (string, error) { called = true; return "91", nil },
	})

	target, err := inj.CaptureTarget(context.Background())
	if err != nil {
		t.Fatalf("CaptureTarget: %v", err)
	}
	if called {
		t.Fatal("kitty hook was called even though KittyRemoteControl is false")
	}
	if target.KittyWindowID != "" {
		t.Fatalf("KittyWindowID = %q, want empty", target.KittyWindowID)
	}
}

func TestCaptureTargetSkipsKittyForNonKittyClass(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Insertion
	cfg.KittyRemoteControl = true
	fc := &fakeKittyCompositor{class: "Firefox"}
	inj := New(cfg, fc, "")
	called := false
	inj.SetKittyHooks(KittyHooks{
		FocusedID: func(ctx context.Context) (string, error) { called = true; return "91", nil },
	})

	if _, err := inj.CaptureTarget(context.Background()); err != nil {
		t.Fatalf("CaptureTarget: %v", err)
	}
	if called {
		t.Fatal("kitty hook was called for a non-kitty target")
	}
}

// TestInsertUsesKittySendTextWithoutFocusChange locks down the core
// "no focus theft" contract: when the target carries a kitty window
// id and the window is reachable, the injector MUST send the text via
// kitty remote control and MUST NOT touch the compositor's
// ActivateWindow / SendKeys / clipboard.
func TestInsertUsesKittySendTextWithoutFocusChange(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Insertion
	cfg.Mode = "clipboard"
	fc := &fakeCompositor{}
	inj := New(cfg, fc, "ctrl+shift+space")
	sentTo, sentText := "", ""
	inj.SetKittyHooks(KittyHooks{
		Exists: func(ctx context.Context, id string) (bool, error) { return true, nil },
		SendText: func(ctx context.Context, id, text string) error {
			sentTo, sentText = id, text
			return nil
		},
	})

	target := platform.Target{WindowID: "42", WindowClass: "kitty", KittyWindowID: "91"}
	if err := inj.Insert(context.Background(), target, "hello kitty"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if sentTo != "91" || sentText != "hello kitty" {
		t.Fatalf("kitty send-text called with id=%q text=%q; want id=\"91\" text=\"hello kitty\"", sentTo, sentText)
	}
	for _, c := range fc.snapshot() {
		if strings.HasPrefix(c, "ActivateWindow ") {
			t.Fatalf("compositor.ActivateWindow MUST NOT be called for a reachable kitty target; calls=%v", fc.snapshot())
		}
		if strings.HasPrefix(c, "SendKeys ") {
			t.Fatalf("compositor paste keys MUST NOT be sent for a reachable kitty target; calls=%v", fc.snapshot())
		}
		if strings.HasPrefix(c, "SetClipboard ") {
			t.Fatalf("clipboard MUST NOT be touched for a reachable kitty target; calls=%v", fc.snapshot())
		}
	}
}

// TestInsertReturnsErrTargetGoneWhenKittyWindowDisappeared exercises
// the recovery path: kitty reports the window is gone, the injector
// must write the transcript to the clipboard and surface ErrTargetGone
// (so the app layer can show a "saved to clipboard" warning rather
// than a failure). send-text MUST NOT be called.
func TestInsertReturnsErrTargetGoneWhenKittyWindowDisappeared(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Insertion
	cfg.Mode = "clipboard"
	fc := &fakeCompositor{}
	inj := New(cfg, fc, "")
	sendTextCalled := false
	inj.SetKittyHooks(KittyHooks{
		Exists: func(ctx context.Context, id string) (bool, error) { return false, nil },
		SendText: func(ctx context.Context, id, text string) error {
			sendTextCalled = true
			return nil
		},
	})

	target := platform.Target{WindowID: "42", WindowClass: "kitty", KittyWindowID: "91"}
	err := inj.Insert(context.Background(), target, "lost transcript")
	if !errors.Is(err, platform.ErrTargetGone) {
		t.Fatalf("Insert err = %v, want ErrTargetGone", err)
	}
	if sendTextCalled {
		t.Fatal("send-text MUST NOT be called when the target window is gone")
	}

	calls := fc.snapshot()
	foundClipboard := false
	for _, c := range calls {
		if c == "SetClipboard lost transcript" {
			foundClipboard = true
		}
		if strings.HasPrefix(c, "SendKeys ") {
			t.Fatalf("paste keys should not have been sent when target is gone; calls=%v", calls)
		}
	}
	if !foundClipboard {
		t.Fatalf("expected SetClipboard write before ErrTargetGone; calls=%v", calls)
	}
}

// TestInsertFallsBackToFocusPasteWhenKittyExistsErrors covers the
// "kitty isn't reachable" degrade path: the Exists hook returns a
// real error (not just "no match"), and the injector should fall
// back to compositor focus + paste so dictation still completes.
func TestInsertFallsBackToFocusPasteWhenKittyExistsErrors(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Insertion
	cfg.Mode = "clipboard"
	fc := &fakeCompositor{}
	inj := New(cfg, fc, "")
	inj.SetKittyHooks(KittyHooks{
		Exists: func(ctx context.Context, id string) (bool, error) {
			return false, errors.New("connection refused")
		},
	})

	target := platform.Target{WindowID: "42", WindowClass: "kitty", KittyWindowID: "91"}
	if err := inj.Insert(context.Background(), target, "hi"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	calls := fc.snapshot()
	foundActivate, foundPaste := false, false
	for _, c := range calls {
		if c == "ActivateWindow 42" {
			foundActivate = true
		}
		if strings.HasPrefix(c, "SendKeys ctrl+") {
			foundPaste = true
		}
	}
	if !foundActivate || !foundPaste {
		t.Fatalf("expected fallback to compositor focus + paste when kitty exists-check errors; calls=%v", calls)
	}
}

// TestInsertFallsBackWhenKittySendTextErrors covers the second-stage
// degrade: Exists succeeded, but the actual SendText shell-out failed
// (e.g. kitty crashed between the two calls). We should still focus +
// paste so dictation completes — losing the no-focus-change benefit
// but not losing the transcript.
func TestInsertFallsBackWhenKittySendTextErrors(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Insertion
	cfg.Mode = "clipboard"
	fc := &fakeCompositor{}
	inj := New(cfg, fc, "")
	inj.SetKittyHooks(KittyHooks{
		Exists:   func(ctx context.Context, id string) (bool, error) { return true, nil },
		SendText: func(ctx context.Context, id, text string) error { return errors.New("socket gone") },
	})

	target := platform.Target{WindowID: "42", WindowClass: "kitty", KittyWindowID: "91"}
	if err := inj.Insert(context.Background(), target, "hi"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	calls := fc.snapshot()
	foundActivate := false
	for _, c := range calls {
		if c == "ActivateWindow 42" {
			foundActivate = true
		}
	}
	if !foundActivate {
		t.Fatalf("expected fallback to compositor.ActivateWindow when kitty send-text fails; calls=%v", calls)
	}
}

// TestPressEnterUsesKittyForKittyTarget locks down the submit-mode
// contract: PressEnter on a kitty target must go through the kitty
// remote-control SendEnter hook, NOT through compositor.SendKeys
// (which would send Return to whatever has focus right now).
func TestPressEnterUsesKittyForKittyTarget(t *testing.T) {
	t.Parallel()

	fc := &fakeCompositor{}
	inj := New(config.Default().Insertion, fc, "")
	called := ""
	inj.SetKittyHooks(KittyHooks{
		SendEnter: func(ctx context.Context, id string) error { called = id; return nil },
	})

	target := platform.Target{KittyWindowID: "91"}
	if err := inj.PressEnter(context.Background(), target); err != nil {
		t.Fatalf("PressEnter: %v", err)
	}
	if called != "91" {
		t.Fatalf("kitty SendEnter called with id=%q, want \"91\"", called)
	}
	for _, c := range fc.snapshot() {
		if strings.HasPrefix(c, "SendKeys ") {
			t.Fatalf("compositor.SendKeys MUST NOT be called for a kitty target; calls=%v", fc.snapshot())
		}
	}
}

// fakeKittyCompositor is a shrunk fakeCompositor that only implements
// CaptureTarget with a configurable window class — the kitty
// enrichment tests don't exercise the rest of the surface.
type fakeKittyCompositor struct{ class string }

func (f *fakeKittyCompositor) CaptureTarget(_ context.Context) (platform.Target, error) {
	return platform.Target{WindowID: "1", WindowClass: f.class}, nil
}
func (f *fakeKittyCompositor) ActivateWindow(_ context.Context, _ platform.Target) error { return nil }
func (f *fakeKittyCompositor) SendKeys(_ context.Context, _ string) error                 { return nil }
func (f *fakeKittyCompositor) ReleaseModifiers(_ context.Context, _ []string) error       { return nil }
func (f *fakeKittyCompositor) SetClipboard(_ context.Context, _ string) error             { return nil }
func (f *fakeKittyCompositor) GetClipboard(_ context.Context) (string, error)             { return "", nil }
func (f *fakeKittyCompositor) Type(_ context.Context, _ platform.Target, _ string, _ bool) error {
	return nil
}
