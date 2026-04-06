package hotkey

import (
	"sync"
	"time"
)

const AutoRepeatDelay = 80 * time.Millisecond

// KeyStateChecker returns the pressed state of tracked keys.
// Used to detect whether physical keys are still held after
// auto-repeat events.
type KeyStateChecker func() (anyDown bool)

// State implements the hotkey state machine: press/release detection,
// auto-repeat filtering, tap detection, lock/unlock, and suppression.
// It is platform-agnostic — the platform backend feeds raw events in.
type State struct {
	shortcut string
	down     chan struct{}
	up       chan struct{}
	tap      chan struct{}
	keyState KeyStateChecker

	mu                       sync.Mutex
	isDown                   bool
	wasReleased              bool
	locked                   bool
	releaseTimer             *time.Timer
	suppressUntil            time.Time
	suppressedReleasePending bool
	suppressTimer            *time.Timer
}

// NewState creates a new hotkey state machine.
func NewState(shortcut string, keyState KeyStateChecker) *State {
	return &State{
		shortcut: shortcut,
		down:     make(chan struct{}, 1),
		up:       make(chan struct{}, 1),
		tap:      make(chan struct{}, 1),
		keyState: keyState,
	}
}

func (s *State) Shortcut() string        { return s.shortcut }
func (s *State) Down() <-chan struct{}    { return s.down }
func (s *State) Up() <-chan struct{}      { return s.up }
func (s *State) Tap() <-chan struct{}     { return s.tap }

// SuppressReleasesFor tells the state machine to ignore release events
// for the given duration. Used to bracket xdotool operations that
// corrupt the keymap.
func (s *State) SuppressReleasesFor(duration time.Duration) {
	if duration <= 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	until := time.Now().Add(duration)
	if until.After(s.suppressUntil) {
		s.suppressUntil = until
	}
	s.suppressedReleasePending = false

	wait := time.Until(s.suppressUntil)
	if wait < 0 {
		wait = 0
	}
	if s.suppressTimer != nil {
		s.suppressTimer.Stop()
	}
	s.suppressTimer = time.AfterFunc(wait, s.finishSuppressedRelease)
}

// Lock makes the state machine ignore all release events until Unlock.
func (s *State) Lock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locked = true
	s.cancelReleaseTimerLocked()
}

// Unlock re-enables release detection and schedules a deferred check.
func (s *State) Unlock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locked = false
	if s.isDown {
		s.rearmReleaseCheckLocked()
	}
}

// Close stops all timers.
func (s *State) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelReleaseTimerLocked()
	if s.suppressTimer != nil {
		s.suppressTimer.Stop()
		s.suppressTimer = nil
	}
}

// HandlePress should be called when the hotkey combo is pressed.
func (s *State) HandlePress() {
	s.mu.Lock()
	s.cancelReleaseTimerLocked()
	if s.isDown {
		if s.wasReleased {
			s.wasReleased = false
			s.mu.Unlock()
			s.emit(s.tap)
		} else {
			s.mu.Unlock()
		}
		return
	}
	s.isDown = true
	s.wasReleased = false
	s.mu.Unlock()

	s.emit(s.down)
}

// HandleRelease should be called when the hotkey trigger key is released.
func (s *State) HandleRelease() {
	s.mu.Lock()
	s.wasReleased = true
	s.mu.Unlock()
	s.scheduleRelease()
}

// HandleTrackedKeyPress should be called when any tracked modifier key
// is pressed. This cancels pending release timers (auto-repeat filter).
func (s *State) HandleTrackedKeyPress() {
	s.mu.Lock()
	s.cancelReleaseTimerLocked()
	if s.suppressionActiveLocked() {
		s.suppressedReleasePending = false
	}
	s.mu.Unlock()
}

// HandleTrackedKeyRelease should be called when any tracked modifier
// key is released.
func (s *State) HandleTrackedKeyRelease() {
	s.scheduleRelease()
}

func (s *State) scheduleRelease() {
	s.mu.Lock()
	if !s.isDown || s.locked {
		s.mu.Unlock()
		return
	}
	if s.suppressionActiveLocked() {
		s.suppressedReleasePending = true
		s.mu.Unlock()
		return
	}
	if s.releaseTimer != nil {
		s.mu.Unlock()
		return
	}

	timer := time.NewTimer(AutoRepeatDelay)
	s.releaseTimer = timer
	s.mu.Unlock()

	go s.awaitRelease(timer)
}

func (s *State) cancelReleaseTimerLocked() {
	if s.releaseTimer != nil {
		s.releaseTimer.Stop()
		s.releaseTimer = nil
	}
}

func (s *State) rearmReleaseCheckLocked() {
	timer := time.NewTimer(AutoRepeatDelay)
	s.releaseTimer = timer
	go s.awaitRelease(timer)
}

func (s *State) suppressionActiveLocked() bool {
	return !s.suppressUntil.IsZero() && time.Now().Before(s.suppressUntil)
}

func (s *State) finishSuppressedRelease() {
	s.mu.Lock()
	if s.suppressTimer == nil {
		s.mu.Unlock()
		return
	}
	s.suppressTimer = nil
	s.suppressUntil = time.Time{}
	if !s.suppressedReleasePending || !s.isDown {
		s.suppressedReleasePending = false
		s.mu.Unlock()
		return
	}
	if s.keyState != nil && s.keyState() {
		s.rearmSuppressedReleaseLocked(AutoRepeatDelay)
		s.mu.Unlock()
		return
	}
	s.suppressedReleasePending = false
	s.isDown = false
	s.mu.Unlock()

	s.emit(s.up)
}

func (s *State) rearmSuppressedReleaseLocked(delay time.Duration) {
	if delay <= 0 {
		delay = AutoRepeatDelay
	}
	if s.suppressTimer != nil {
		s.suppressTimer.Stop()
	}
	s.suppressTimer = time.AfterFunc(delay, s.finishSuppressedRelease)
}

func (s *State) awaitRelease(timer *time.Timer) {
	<-timer.C

	s.mu.Lock()
	if s.releaseTimer != timer {
		s.mu.Unlock()
		return
	}
	s.releaseTimer = nil
	if !s.isDown {
		s.mu.Unlock()
		return
	}
	if s.keyState != nil && s.keyState() {
		s.rearmReleaseCheckLocked()
		s.mu.Unlock()
		return
	}
	s.isDown = false
	s.mu.Unlock()

	s.emit(s.up)
}

func (s *State) emit(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
