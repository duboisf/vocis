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
// auto-repeat filtering, and tap detection. It is platform-agnostic —
// the platform backend feeds raw events in.
type State struct {
	shortcut string
	down     chan struct{}
	up       chan struct{}
	tap      chan struct{}
	keyState KeyStateChecker

	mu           sync.Mutex
	isDown       bool
	wasReleased  bool
	releaseTimer *time.Timer
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

func (s *State) Shortcut() string      { return s.shortcut }
func (s *State) Down() <-chan struct{} { return s.down }
func (s *State) Up() <-chan struct{}   { return s.up }
func (s *State) Tap() <-chan struct{}  { return s.tap }

// Close stops all timers.
func (s *State) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelReleaseTimerLocked()
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

// HandleTap should be called when the platform backend has detected a
// tap-while-held (the trigger key was released and re-pressed without
// the modifiers being released). On X11 the state machine derives this
// from explicit press/release events, but on the GNOME extension
// backend the trigger-key release isn't observable — Mutter only
// reports the combo activating — so the extension does the detection
// itself and signals it through this method.
func (s *State) HandleTap() {
	s.mu.Lock()
	if !s.isDown {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.emit(s.tap)
}

// HandleTrackedKeyPress should be called when any tracked modifier key
// is pressed. This cancels pending release timers (auto-repeat filter).
func (s *State) HandleTrackedKeyPress() {
	s.mu.Lock()
	s.cancelReleaseTimerLocked()
	s.mu.Unlock()
}

// HandleTrackedKeyRelease should be called when any tracked modifier
// key is released.
func (s *State) HandleTrackedKeyRelease() {
	s.scheduleRelease()
}

func (s *State) scheduleRelease() {
	s.mu.Lock()
	if !s.isDown {
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
