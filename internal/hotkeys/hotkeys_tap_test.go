package hotkeys

import (
	"os"
	"testing"
	"time"
)

func TestTapEmitsOnRepeatedPress(t *testing.T) {
	if os.Getenv("DISPLAY") == "" {
		t.Skip("no DISPLAY set")
	}

	r, err := Register("ctrl+shift+space")
	if err != nil {
		t.Skipf("could not register hotkey: %v", err)
	}
	defer r.Close()

	// Simulate the scenario: isDown is true (hotkey pressed),
	// then handlePress fires again (space tapped while held).
	// This bypasses X11 — we call handlePress directly.

	// First press: sets isDown, emits down.
	r.handlePress()
	select {
	case <-r.Down():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected Down event from first press")
	}

	// Second press while still down: should emit tap.
	r.handlePress()
	select {
	case <-r.Tap():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected Tap event from second press while isDown=true")
	}

	// Third press: should emit another tap.
	r.handlePress()
	select {
	case <-r.Tap():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected Tap event from third press")
	}

	// Down channel should be empty (only one Down was emitted).
	select {
	case <-r.Down():
		t.Fatal("unexpected extra Down event")
	default:
	}
}
