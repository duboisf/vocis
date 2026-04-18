package inject

import (
	"context"
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
