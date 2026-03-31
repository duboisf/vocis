package hotkeys

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/keybind"
	"github.com/BurntSushi/xgbutil/xevent"
)

const autoRepeatReleaseDelay = 80 * time.Millisecond

type Registration struct {
	shortcut string
	x        *xgbutil.XUtil
	down     chan struct{}
	up       chan struct{}

	trackedCodes map[xproto.Keycode]struct{}

	mu                       sync.Mutex
	isDown                   bool
	releaseTimer             *time.Timer
	suppressUntil            time.Time
	suppressedReleasePending bool
	suppressTimer            *time.Timer
}

func Register(shortcut string) (*Registration, error) {
	sequence, err := parse(shortcut)
	if err != nil {
		return nil, err
	}

	xu, err := xgbutil.NewConn()
	if err != nil {
		return nil, err
	}
	keybind.Initialize(xu)

	trackedCodes, err := trackedKeycodes(xu, shortcut)
	if err != nil {
		return nil, err
	}

	r := &Registration{
		shortcut:     shortcut,
		x:            xu,
		down:         make(chan struct{}, 1),
		up:           make(chan struct{}, 1),
		trackedCodes: trackedCodes,
	}

	err = keybind.KeyPressFun(func(_ *xgbutil.XUtil, _ xevent.KeyPressEvent) {
		r.handlePress()
	}).Connect(xu, xu.RootWin(), sequence, true)
	if err != nil {
		return nil, err
	}

	err = keybind.KeyReleaseFun(func(_ *xgbutil.XUtil, _ xevent.KeyReleaseEvent) {
		r.handleRelease()
	}).Connect(xu, xu.RootWin(), sequence, true)
	if err != nil {
		return nil, err
	}

	xevent.KeyPressFun(func(_ *xgbutil.XUtil, ev xevent.KeyPressEvent) {
		r.handleTrackedPress(ev.Detail)
	}).Connect(xu, xu.RootWin())

	xevent.KeyReleaseFun(func(_ *xgbutil.XUtil, ev xevent.KeyReleaseEvent) {
		r.handleTrackedRelease(ev.Detail)
	}).Connect(xu, xu.RootWin())

	go xevent.Main(xu)
	return r, nil
}

func (r *Registration) Shortcut() string {
	return r.shortcut
}

func (r *Registration) Down() <-chan struct{} {
	return r.down
}

func (r *Registration) Up() <-chan struct{} {
	return r.up
}

func (r *Registration) SuppressReleasesFor(duration time.Duration) {
	if duration <= 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	until := time.Now().Add(duration)
	if until.After(r.suppressUntil) {
		r.suppressUntil = until
	}
	r.suppressedReleasePending = false

	wait := time.Until(r.suppressUntil)
	if wait < 0 {
		wait = 0
	}
	if r.suppressTimer != nil {
		r.suppressTimer.Stop()
	}
	r.suppressTimer = time.AfterFunc(wait, r.finishSuppressedRelease)
}

func (r *Registration) Close() error {
	r.mu.Lock()
	if r.releaseTimer != nil {
		r.releaseTimer.Stop()
		r.releaseTimer = nil
	}
	if r.suppressTimer != nil {
		r.suppressTimer.Stop()
		r.suppressTimer = nil
	}
	r.mu.Unlock()

	keybind.Detach(r.x, r.x.RootWin())
	xevent.Quit(r.x)
	r.x.Conn().Close()
	return nil
}

func (r *Registration) handlePress() {
	r.mu.Lock()
	r.cancelReleaseTimerLocked()
	if r.isDown {
		r.mu.Unlock()
		return
	}
	r.isDown = true
	r.mu.Unlock()

	r.emit(r.down)
}

func (r *Registration) handleRelease() {
	r.scheduleRelease()
}

func (r *Registration) handleTrackedPress(code xproto.Keycode) {
	if !r.isTrackedKey(code) {
		return
	}

	r.mu.Lock()
	r.cancelReleaseTimerLocked()
	if r.suppressionActiveLocked() {
		r.suppressedReleasePending = false
	}
	r.mu.Unlock()
}

func (r *Registration) handleTrackedRelease(code xproto.Keycode) {
	if !r.isTrackedKey(code) {
		return
	}

	r.scheduleRelease()
}

func (r *Registration) scheduleRelease() {
	r.mu.Lock()
	if !r.isDown {
		r.mu.Unlock()
		return
	}
	if r.suppressionActiveLocked() {
		r.suppressedReleasePending = true
		r.mu.Unlock()
		return
	}
	if r.releaseTimer != nil {
		r.mu.Unlock()
		return
	}

	timer := time.NewTimer(autoRepeatReleaseDelay)
	r.releaseTimer = timer
	r.mu.Unlock()

	go r.awaitRelease(timer)
}

func (r *Registration) cancelReleaseTimerLocked() {
	if r.releaseTimer != nil {
		r.releaseTimer.Stop()
		r.releaseTimer = nil
	}
}

func (r *Registration) suppressionActiveLocked() bool {
	return !r.suppressUntil.IsZero() && time.Now().Before(r.suppressUntil)
}

func (r *Registration) finishSuppressedRelease() {
	r.mu.Lock()
	if r.suppressTimer == nil {
		r.mu.Unlock()
		return
	}
	r.suppressTimer = nil
	r.suppressUntil = time.Time{}
	if !r.suppressedReleasePending || !r.isDown {
		r.suppressedReleasePending = false
		r.mu.Unlock()
		return
	}
	r.suppressedReleasePending = false
	r.isDown = false
	r.mu.Unlock()

	r.emit(r.up)
}

func (r *Registration) awaitRelease(timer *time.Timer) {
	<-timer.C

	r.mu.Lock()
	if r.releaseTimer != timer {
		r.mu.Unlock()
		return
	}
	r.releaseTimer = nil
	if !r.isDown {
		r.mu.Unlock()
		return
	}
	r.isDown = false
	r.mu.Unlock()

	r.emit(r.up)
}

func (r *Registration) emit(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (r *Registration) isTrackedKey(code xproto.Keycode) bool {
	_, ok := r.trackedCodes[code]
	return ok
}

func trackedKeycodes(xu *xgbutil.XUtil, shortcut string) (map[xproto.Keycode]struct{}, error) {
	parts := strings.FieldsFunc(strings.ToLower(shortcut), func(r rune) bool {
		return r == '+' || r == '-'
	})
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid hotkey %q", shortcut)
	}

	codes := make(map[xproto.Keycode]struct{})
	for _, part := range parts[:len(parts)-1] {
		for _, name := range modifierKeyNames(part) {
			for _, code := range keybind.StrToKeycodes(xu, name) {
				codes[code] = struct{}{}
			}
		}
	}
	keyName, ok := parseKey(parts[len(parts)-1])
	if !ok {
		return nil, fmt.Errorf("unsupported key %q", parts[len(parts)-1])
	}
	for _, code := range keybind.StrToKeycodes(xu, keyName) {
		codes[code] = struct{}{}
	}
	if len(codes) == 0 {
		return nil, fmt.Errorf("no keycodes found for hotkey %q", shortcut)
	}
	return codes, nil
}

func modifierKeyNames(part string) []string {
	switch part {
	case "ctrl", "control":
		return []string{"Control_L", "Control_R"}
	case "alt", "option":
		return []string{"Alt_L", "Alt_R", "Meta_L", "Meta_R"}
	case "shift":
		return []string{"Shift_L", "Shift_R"}
	case "cmd", "super", "meta", "win":
		return []string{"Super_L", "Super_R"}
	default:
		return nil
	}
}

func parse(shortcut string) (string, error) {
	parts := strings.FieldsFunc(strings.ToLower(shortcut), func(r rune) bool {
		return r == '+' || r == '-'
	})
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid hotkey %q", shortcut)
	}

	mods := make([]string, 0, len(parts)-1)
	for _, part := range parts[:len(parts)-1] {
		mod, ok := parseModifier(part)
		if !ok {
			return "", fmt.Errorf("unsupported modifier %q", part)
		}
		mods = append(mods, mod)
	}

	key, ok := parseKey(parts[len(parts)-1])
	if !ok {
		return "", fmt.Errorf("unsupported key %q", parts[len(parts)-1])
	}
	return strings.Join(append(mods, key), "-"), nil
}

func parseModifier(part string) (string, bool) {
	switch part {
	case "ctrl", "control":
		return "control", true
	case "alt", "option":
		return "mod1", true
	case "shift":
		return "shift", true
	case "cmd", "super", "meta", "win":
		return "mod4", true
	default:
		return "", false
	}
}

func parseKey(part string) (string, bool) {
	if len(part) == 1 {
		r := rune(part[0])
		if r >= 'a' && r <= 'z' {
			return part, true
		}
		if r >= '0' && r <= '9' {
			return part, true
		}
	}

	switch part {
	case "space":
		return "space", true
	case "enter", "return":
		return "Return", true
	case "tab":
		return "Tab", true
	case "escape", "esc":
		return "Escape", true
	case "comma":
		return "comma", true
	case "period", "dot":
		return "period", true
	case "slash":
		return "slash", true
	case "semicolon":
		return "semicolon", true
	case "apostrophe", "quote":
		return "apostrophe", true
	case "minus":
		return "minus", true
	case "equal", "equals":
		return "equal", true
	case "leftbracket":
		return "bracketleft", true
	case "rightbracket":
		return "bracketright", true
	case "backslash":
		return "backslash", true
	case "grave":
		return "grave", true
	}

	if strings.HasPrefix(part, "f") {
		n, err := strconv.Atoi(strings.TrimPrefix(part, "f"))
		if err == nil {
			switch {
			case n >= 1 && n <= 12:
				return fmt.Sprintf("F%d", n), true
			}
		}
	}

	return "", false
}
