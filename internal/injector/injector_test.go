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
