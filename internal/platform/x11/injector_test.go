package x11

import (
	"context"
	"strings"
	"testing"

	"vocis/internal/config"
)

func TestTerminalDetectionIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	inj := NewInjector(config.Default().Insertion, "")
	if !inj.isTerminal("alacritty") {
		t.Fatal("expected alacritty to be treated as a terminal")
	}
}

func TestParseXPropMetadataUsesWMClassFallback(t *testing.T) {
	t.Parallel()

	className, windowName := parseXPropMetadata(
		`WM_CLASS(STRING) = "kitty", "kitty"
WM_NAME(STRING) = "backend"`,
	)

	if className != "kitty" {
		t.Fatalf("className = %q, want kitty", className)
	}
	if windowName != "backend" {
		t.Fatalf("windowName = %q, want backend", windowName)
	}
}

func TestBuildPasteArgsUsesFocusedWindow(t *testing.T) {
	t.Parallel()

	args := buildPasteArgs("ctrl+v")

	want := []string{"key", "--clearmodifiers", "ctrl+v"}
	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d; args=%v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; args=%v", i, args[i], want[i], args)
		}
	}
}

func TestBuildTypeArgsForLiveSegmentUsesFocusedWidget(t *testing.T) {
	t.Parallel()

	args := buildTypeArgs(1, Target{WindowID: "42"}, "hello world", false)

	want := []string{"type", "--clearmodifiers", "--delay", "1", "--", "hello world"}
	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d; args=%v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; args=%v", i, args[i], want[i], args)
		}
	}
}

func TestBuildModifierReleaseArgs(t *testing.T) {
	t.Parallel()

	args := buildModifierReleaseArgs()

	want := []string{
		"keyup",
		"Control_L",
		"Control_R",
		"Shift_L",
		"Shift_R",
		"Alt_L",
		"Alt_R",
		"Super_L",
		"Super_R",
	}
	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d; args=%v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; args=%v", i, args[i], want[i], args)
		}
	}
}

func TestBuildKeyReleaseArgsIncludesTriggerKey(t *testing.T) {
	t.Parallel()

	args, err := buildKeyReleaseArgs("ctrl+shift+space")
	if err != nil {
		t.Fatalf("buildKeyReleaseArgs: %v", err)
	}

	want := []string{
		"keyup",
		"Control_L",
		"Control_R",
		"Shift_L",
		"Shift_R",
		"space",
	}
	if len(args) != len(want) {
		t.Fatalf("len(args) = %d, want %d; args=%v", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q; args=%v", i, args[i], want[i], args)
		}
	}
}

func TestInsertLiveReleasesModifiersBeforeTyping(t *testing.T) {
	t.Parallel()

	var calls []string
	inj := NewInjector(config.Default().Insertion, "ctrl+shift+space")
	inj.run = func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}

	err := inj.InsertLive(
		context.Background(),
		Target{WindowID: "42", WindowClass: "kitty"},
		"hello world",
	)
	if err != nil {
		t.Fatalf("InsertLive: %v", err)
	}

	if len(calls) != 3 {
		t.Fatalf("len(calls) = %d, want 3; calls=%v", len(calls), calls)
	}
	if got, want := calls[0], "xdotool windowactivate --sync 42"; got != want {
		t.Fatalf("calls[0] = %q, want %q; calls=%v", got, want, calls)
	}
	if got, want := calls[1],
		"xdotool keyup Control_L Control_R Shift_L Shift_R space"; got != want {
		t.Fatalf("calls[1] = %q, want %q; calls=%v", got, want, calls)
	}
	if !strings.HasPrefix(calls[2], "xdotool type --clearmodifiers --delay ") ||
		!strings.HasSuffix(calls[2], " -- hello world") {
		t.Fatalf("calls[2] = %q, want xdotool type ... hello world; calls=%v", calls[2], calls)
	}
}

func TestInsertReleasesModifiersBeforePasting(t *testing.T) {
	t.Parallel()

	var calls []string
	cfg := config.Default().Insertion
	cfg.Mode = "clipboard"

	inj := NewInjector(cfg, "ctrl+shift+space")
	inj.run = func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return "", nil
	}

	err := inj.Insert(
		context.Background(),
		Target{WindowID: "42", WindowClass: "kitty"},
		"hello world",
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if len(calls) < 4 {
		t.Fatalf("len(calls) = %d, want at least 4; calls=%v", len(calls), calls)
	}
	if got, want := calls[0], "xdotool windowactivate --sync 42"; got != want {
		t.Fatalf("calls[0] = %q, want %q; calls=%v", got, want, calls)
	}
	if got, want := calls[1],
		"xdotool keyup Control_L Control_R Shift_L Shift_R space"; got != want {
		t.Fatalf("calls[1] = %q, want %q; calls=%v", got, want, calls)
	}
	if got, want := calls[2], "xdotool windowactivate --sync 42"; got != want {
		t.Fatalf("calls[2] = %q, want %q; calls=%v", got, want, calls)
	}
	if got, want := calls[3], "xdotool key --clearmodifiers ctrl+shift+v"; got != want {
		t.Fatalf("calls[3] = %q, want %q; calls=%v", got, want, calls)
	}
}
