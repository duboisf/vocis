package x11

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/keybind"
	"github.com/BurntSushi/xgbutil/xevent"

	"vocis/internal/hotkey"
)

// Registration wraps hotkey.State with X11-specific key capture.
type Registration struct {
	*hotkey.State
	x            *xgbutil.XUtil
	trackedCodes map[xproto.Keycode]struct{}
}

// Register grabs a global hotkey via X11 and returns a Registration
// that emits Down/Up/Tap events through hotkey.State.
func Register(shortcut string) (*Registration, error) {
	sequence, err := hotkey.ParseSequence(shortcut)
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
		x:            xu,
		trackedCodes: trackedCodes,
	}

	r.State = hotkey.NewState(shortcut, r.anyTrackedKeyDown)

	err = keybind.KeyPressFun(func(_ *xgbutil.XUtil, _ xevent.KeyPressEvent) {
		r.HandlePress()
	}).Connect(xu, xu.RootWin(), sequence, true)
	if err != nil {
		return nil, err
	}

	err = keybind.KeyReleaseFun(func(_ *xgbutil.XUtil, _ xevent.KeyReleaseEvent) {
		r.HandleRelease()
	}).Connect(xu, xu.RootWin(), sequence, true)
	if err != nil {
		return nil, err
	}

	xevent.KeyPressFun(func(_ *xgbutil.XUtil, ev xevent.KeyPressEvent) {
		if r.isTrackedKey(ev.Detail) {
			r.HandleTrackedKeyPress()
		}
	}).Connect(xu, xu.RootWin())

	xevent.KeyReleaseFun(func(_ *xgbutil.XUtil, ev xevent.KeyReleaseEvent) {
		if r.isTrackedKey(ev.Detail) {
			r.HandleTrackedKeyRelease()
		}
	}).Connect(xu, xu.RootWin())

	go xevent.Main(xu)
	return r, nil
}

// Close releases the X11 grab and stops the event loop.
func (r *Registration) Close() error {
	r.State.Close()
	keybind.Detach(r.x, r.x.RootWin())
	xevent.Quit(r.x)
	r.x.Conn().Close()
	return nil
}

func (r *Registration) isTrackedKey(code xproto.Keycode) bool {
	_, ok := r.trackedCodes[code]
	return ok
}

func (r *Registration) anyTrackedKeyDown() bool {
	reply, err := xproto.QueryKeymap(r.x.Conn()).Reply()
	if err != nil {
		return false
	}
	for code := range r.trackedCodes {
		if keycodePressed(reply.Keys, code) {
			return true
		}
	}
	return false
}

func keycodePressed(keys []byte, code xproto.Keycode) bool {
	index := int(code) / 8
	if index < 0 || index >= len(keys) {
		return false
	}
	return keys[index]&byte(1<<(uint(code)%8)) != 0
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
		for _, name := range hotkey.ModifierKeyNames(part) {
			for _, code := range keybind.StrToKeycodes(xu, name) {
				codes[code] = struct{}{}
			}
		}
	}
	keyName, ok := hotkey.ParseKey(parts[len(parts)-1])
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
