package hotkey

import (
	"testing"
	"time"
)

func TestTapEmitsOnReleaseAndRepress(t *testing.T) {
	t.Parallel()

	s := NewState("ctrl+shift+space", nil)

	s.HandlePress()
	expectEvent(t, s.Down())

	// Auto-repeat (no release): should NOT emit Tap.
	s.HandlePress()
	expectNoEvent(t, s.Tap(), 50*time.Millisecond)

	// Release then re-press: should emit Tap.
	s.HandleRelease()
	s.HandlePress()
	expectEvent(t, s.Tap())

	// Another release+press: another Tap.
	s.HandleRelease()
	s.HandlePress()
	expectEvent(t, s.Tap())

	// Down should be empty (only one Down emitted).
	expectNoEvent(t, s.Down(), 10*time.Millisecond)
}

func TestAutoRepeatDoesNotEmitTap(t *testing.T) {
	t.Parallel()

	s := NewState("ctrl+shift+space", nil)
	s.HandlePress()
	<-s.Down()

	for range 10 {
		s.HandlePress()
	}

	expectNoEvent(t, s.Tap(), 50*time.Millisecond)
}

func TestDownAndUp(t *testing.T) {
	t.Parallel()

	s := NewState("ctrl+shift+space", nil)

	s.HandlePress()
	expectEvent(t, s.Down())

	s.HandleRelease()
	expectEventWithin(t, s.Up(), AutoRepeatDelay+40*time.Millisecond)
}

func TestAutoRepeatSuppression(t *testing.T) {
	t.Parallel()

	s := NewState("ctrl+shift+space", nil)

	s.HandlePress()
	expectEvent(t, s.Down())

	s.HandleRelease()
	time.Sleep(AutoRepeatDelay / 2)
	s.HandlePress()
	expectNoEvent(t, s.Up(), AutoRepeatDelay+40*time.Millisecond)
	expectNoEvent(t, s.Down(), 40*time.Millisecond)

	s.HandleRelease()
	expectEventWithin(t, s.Up(), AutoRepeatDelay+40*time.Millisecond)
}

func TestTrackedKeyRelease(t *testing.T) {
	t.Parallel()

	s := NewState("ctrl+shift+space", nil)

	s.HandlePress()
	expectEvent(t, s.Down())

	s.HandleTrackedKeyRelease()
	expectEventWithin(t, s.Up(), AutoRepeatDelay+40*time.Millisecond)
}

func TestSuppressedReleaseEmitsUpAfterWindow(t *testing.T) {
	t.Parallel()

	s := NewState("ctrl+shift+space", nil)

	s.HandlePress()
	expectEvent(t, s.Down())

	s.SuppressReleasesFor(120 * time.Millisecond)
	s.HandleTrackedKeyRelease()

	expectEventWithin(t, s.Up(), 220*time.Millisecond)
}

func TestSuppressedReleaseCancelledByRepress(t *testing.T) {
	t.Parallel()

	s := NewState("ctrl+shift+space", nil)

	s.HandlePress()
	expectEvent(t, s.Down())

	s.SuppressReleasesFor(120 * time.Millisecond)
	s.HandleTrackedKeyRelease()
	time.Sleep(40 * time.Millisecond)
	s.HandleTrackedKeyPress()

	expectNoEvent(t, s.Up(), 180*time.Millisecond)
}

func TestSuppressedReleaseDoesNotEmitWhileKeyHeld(t *testing.T) {
	t.Parallel()

	s := NewState("ctrl+shift+space", func() bool { return true })

	s.HandlePress()
	expectEvent(t, s.Down())

	s.SuppressReleasesFor(120 * time.Millisecond)
	s.HandleTrackedKeyRelease()

	expectNoEvent(t, s.Up(), 220*time.Millisecond)
}

func TestReleaseTimerDoesNotEmitWhileKeyHeld(t *testing.T) {
	t.Parallel()

	down := true
	s := NewState("ctrl+shift+space", func() bool { return down })

	s.HandlePress()
	expectEvent(t, s.Down())

	s.HandleTrackedKeyRelease()
	expectNoEvent(t, s.Up(), AutoRepeatDelay+80*time.Millisecond)

	down = false
	expectEventWithin(t, s.Up(), AutoRepeatDelay+120*time.Millisecond)
}

func TestLockedReleaseIsIgnored(t *testing.T) {
	t.Parallel()

	s := NewState("ctrl+shift+space", nil)

	s.HandlePress()
	expectEvent(t, s.Down())

	s.Lock()
	s.HandleTrackedKeyRelease()
	s.HandleRelease()
	expectNoEvent(t, s.Up(), 200*time.Millisecond)
}

func TestUnlockEmitsUpWhenKeyReleased(t *testing.T) {
	t.Parallel()

	s := NewState("ctrl+shift+space", nil)

	s.HandlePress()
	expectEvent(t, s.Down())

	s.Lock()
	s.HandleTrackedKeyRelease()

	s.Unlock()
	expectEventWithin(t, s.Up(), AutoRepeatDelay+40*time.Millisecond)
}

func TestUnlockDoesNotEmitUpWhileKeyHeld(t *testing.T) {
	t.Parallel()

	down := true
	s := NewState("ctrl+shift+space", func() bool { return down })

	s.HandlePress()
	expectEvent(t, s.Down())

	s.Lock()
	s.HandleTrackedKeyRelease()

	s.Unlock()
	expectNoEvent(t, s.Up(), AutoRepeatDelay+80*time.Millisecond)

	down = false
	expectEventWithin(t, s.Up(), AutoRepeatDelay+120*time.Millisecond)
}

func expectEvent(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	expectEventWithin(t, ch, time.Second)
}

func expectEventWithin(t *testing.T, ch <-chan struct{}, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		t.Fatalf("expected event within %s", timeout)
	}
}

func expectNoEvent(t *testing.T, ch <-chan struct{}, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		t.Fatal("expected no event")
	case <-timer.C:
	}
}
