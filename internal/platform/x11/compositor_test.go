package x11

import (
	"context"
	"strings"
	"testing"
)

// TestCaptureTargetUsesXpropFallbackWhenClassEmpty exercises the
// "xdotool returned no class" case (real on JetBrains/Steam windows)
// where Compositor falls back to xprop WM_CLASS.
func TestCaptureTargetUsesXpropFallbackWhenClassEmpty(t *testing.T) {
	t.Parallel()

	c := NewCompositor()
	c.SetCommandRunner(func(_ context.Context, name string, args ...string) (string, error) {
		switch {
		case name == "xdotool" && args[0] == "getactivewindow":
			return "42", nil
		case name == "xdotool" && args[0] == "getwindowclassname":
			return "", nil // empty → triggers xprop fallback
		case name == "xdotool" && args[0] == "getwindowname":
			return "Project — IDEA", nil
		case name == "xprop":
			return `WM_CLASS(STRING) = "jetbrains-idea", "jetbrains-idea"`, nil
		}
		return "", nil
	})

	target, err := c.CaptureTarget(context.Background())
	if err != nil {
		t.Fatalf("CaptureTarget: %v", err)
	}
	if target.WindowClass != "jetbrains-idea" {
		t.Fatalf("WindowClass = %q, want jetbrains-idea (xprop fallback)", target.WindowClass)
	}
	if target.WindowID != "42" {
		t.Fatalf("WindowID = %q, want 42", target.WindowID)
	}
}

// TestSendKeysShellsOutWithLowercaseCombo locks down the xdotool
// argument shape so config like "Ctrl+V" reaches xdotool as "ctrl+v"
// (the only form xdotool reliably parses on every locale).
func TestSendKeysShellsOutWithLowercaseCombo(t *testing.T) {
	t.Parallel()

	var captured string
	c := NewCompositor()
	c.SetCommandRunner(func(_ context.Context, name string, args ...string) (string, error) {
		captured = name + " " + strings.Join(args, " ")
		return "", nil
	})
	if err := c.SendKeys(context.Background(), "Ctrl+Shift+V"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	want := "xdotool key --clearmodifiers ctrl+shift+v"
	if captured != want {
		t.Fatalf("captured=%q want %q", captured, want)
	}
}

// TestReleaseModifiersMapsToXdotoolKeyup confirms the keyup invocation
// shape — xdotool needs the keysym names (Control_L etc), not friendly
// aliases.
func TestReleaseModifiersMapsToXdotoolKeyup(t *testing.T) {
	t.Parallel()

	var captured string
	c := NewCompositor()
	c.SetCommandRunner(func(_ context.Context, name string, args ...string) (string, error) {
		captured = name + " " + strings.Join(args, " ")
		return "", nil
	})
	if err := c.ReleaseModifiers(context.Background(), []string{"Control_L", "Shift_L"}); err != nil {
		t.Fatalf("ReleaseModifiers: %v", err)
	}
	want := "xdotool keyup Control_L Shift_L"
	if captured != want {
		t.Fatalf("captured=%q want %q", captured, want)
	}
}
