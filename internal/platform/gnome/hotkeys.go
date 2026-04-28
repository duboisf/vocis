// Package gnome provides a GNOME Shell-extension-backed hotkey source.
//
// It speaks D-Bus to the companion `vocis@duboisf.github.io` shell extension (see
// `extensions/vocis-gnome/`), which uses Mutter's grab_accelerator API to
// register a global press/release-aware accelerator. This is the path that
// makes hold-to-talk dictation work on GNOME Wayland, where third-party
// processes cannot grab keys directly.
package gnome

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"vocis/internal/hotkey"
	"vocis/internal/sessionlog"
)

func dbusCallContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), probeTimeout)
}

const (
	// BusName, ObjectPath, and Interface match the names exported by the
	// vocis-gnome shell extension. Keep them in sync with extension.js.
	BusName    = "io.github.duboisf.Vocis.Hotkey"
	ObjectPath = "/io/github/duboisf/Vocis/Hotkey"
	Interface  = "io.github.duboisf.Vocis.Hotkey"

	probeTimeout = 2 * time.Second
)

// ErrExtensionNotInstalled is returned when the vocis-gnome shell extension
// cannot be reached on the session bus. The most common cause is that the
// extension is not installed or not enabled, or gnome-shell hasn't been
// reloaded since installation.
var ErrExtensionNotInstalled = errors.New("vocis-gnome shell extension not detected on session bus")

// Available probes the session bus to see whether the vocis-gnome extension
// is exporting its D-Bus interface. It opens its own short-lived connection
// so the caller doesn't pay for one if the answer is no.
func Available() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	return probe(conn) == nil
}

func probe(conn *dbus.Conn) error {
	obj := conn.Object(BusName, ObjectPath)
	ctx, cancel := dbusCallContext()
	defer cancel()

	var shortcut string
	if err := obj.CallWithContext(ctx, Interface+".GetShortcut", 0).Store(&shortcut); err != nil {
		return fmt.Errorf("%w: %v", ErrExtensionNotInstalled, err)
	}
	return nil
}

// Registration is a hotkey.State driven by D-Bus signals from the
// vocis-gnome extension. It satisfies app.HotkeySource.
type Registration struct {
	*hotkey.State

	conn      *dbus.Conn
	closeOnce sync.Once

	isDownMu  sync.Mutex
	isDownVal bool

	signalCh chan *dbus.Signal
}

// Register opens a private session bus connection, verifies the extension
// is reachable, and starts forwarding Activated/Deactivated signals into a
// hotkey.State. The shortcut argument is informational only — the actual
// key combo is owned by the extension. We log a warning if the configured
// shortcut and the extension's reported shortcut disagree.
func Register(shortcut string) (*Registration, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect to session bus: %w", err)
	}

	if err := probe(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	extShortcut, err := getExtensionShortcut(conn)
	if err == nil && extShortcut != shortcut {
		sessionlog.Warnf("hotkey: vocis config = %q but vocis-gnome extension reports %q — change one to match", shortcut, extShortcut)
	}

	r := &Registration{conn: conn}
	r.State = hotkey.NewState(shortcut, r.isDown)

	if err := r.subscribe(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	sessionlog.Infof("hotkey: bound via vocis-gnome extension (%s)", extShortcut)
	return r, nil
}

func getExtensionShortcut(conn *dbus.Conn) (string, error) {
	obj := conn.Object(BusName, ObjectPath)
	ctx, cancel := dbusCallContext()
	defer cancel()

	var shortcut string
	if err := obj.CallWithContext(ctx, Interface+".GetShortcut", 0).Store(&shortcut); err != nil {
		return "", err
	}
	return shortcut, nil
}

func (r *Registration) subscribe() error {
	if err := r.conn.AddMatchSignal(
		dbus.WithMatchInterface(Interface),
		dbus.WithMatchObjectPath(ObjectPath),
	); err != nil {
		return fmt.Errorf("add match signal: %w", err)
	}
	r.signalCh = make(chan *dbus.Signal, 16)
	r.conn.Signal(r.signalCh)
	go r.signalLoop()
	return nil
}

func (r *Registration) signalLoop() {
	for sig := range r.signalCh {
		switch sig.Name {
		case Interface + ".Activated":
			r.setDown(true)
			r.HandlePress()
		case Interface + ".Deactivated":
			r.setDown(false)
			r.HandleRelease()
		case Interface + ".Tapped":
			r.HandleTap()
		}
	}
}

func (r *Registration) setDown(v bool) {
	r.isDownMu.Lock()
	r.isDownVal = v
	r.isDownMu.Unlock()
}

func (r *Registration) isDown() bool {
	r.isDownMu.Lock()
	defer r.isDownMu.Unlock()
	return r.isDownVal
}

// Close tears down the bus subscription and the underlying connection.
func (r *Registration) Close() error {
	r.closeOnce.Do(func() {
		if r.State != nil {
			r.State.Close()
		}
		if r.conn != nil {
			if r.signalCh != nil {
				r.conn.RemoveSignal(r.signalCh)
			}
			_ = r.conn.Close()
		}
	})
	return nil
}
