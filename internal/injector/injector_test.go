package injector

import (
	"testing"

	"vtt/internal/config"
)

func TestTerminalDetectionIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	inj := New(config.Default().Insertion)
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
