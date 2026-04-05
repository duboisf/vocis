package x11

import (
	"testing"
	"time"
)

func newTestRegistration() *Registration {
	return &Registration{
		down: make(chan struct{}, 1),
		up:   make(chan struct{}, 1),
		tap:  make(chan struct{}, 1),
	}
}

func TestTapEmitsOnReleaseAndRepress(t *testing.T) {
	t.Parallel()

	r := newTestRegistration()

	// First press: sets isDown, emits Down.
	r.handlePress()
	select {
	case <-r.Down():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected Down event from first press")
	}

	// Auto-repeat press (no release in between): should NOT emit Tap.
	r.handlePress()
	select {
	case <-r.Tap():
		t.Fatal("unexpected Tap from auto-repeat (no release)")
	case <-time.After(50 * time.Millisecond):
	}

	// Release then re-press: should emit Tap.
	r.handleRelease()
	r.handlePress()
	select {
	case <-r.Tap():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected Tap after release+press")
	}

	// Another release+press: should emit another Tap.
	r.handleRelease()
	r.handlePress()
	select {
	case <-r.Tap():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected Tap after second release+press")
	}

	// Down channel should be empty.
	select {
	case <-r.Down():
		t.Fatal("unexpected extra Down event")
	default:
	}
}

func TestAutoRepeatDoesNotEmitTap(t *testing.T) {
	t.Parallel()

	r := newTestRegistration()

	r.handlePress()
	<-r.Down()

	// Rapid auto-repeat presses without any release.
	for i := 0; i < 10; i++ {
		r.handlePress()
	}

	select {
	case <-r.Tap():
		t.Fatal("unexpected Tap from auto-repeat")
	case <-time.After(50 * time.Millisecond):
	}
}
