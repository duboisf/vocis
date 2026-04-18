package gnome

import (
	"context"
	"fmt"

	"github.com/godbus/dbus/v5"

	"vocis/internal/platform"
)

// Compositor implements platform.Compositor by calling into the
// vocis-gnome shell extension over D-Bus. No xdotool, no xclip — every
// primitive (focus, key synthesis, clipboard) is handled inside Mutter
// where Wayland actually permits it.
//
// The connection is owned per-Compositor so the daemon can keep one
// long-lived bus connection rather than opening one per call.
type Compositor struct {
	conn *dbus.Conn
	obj  dbus.BusObject
}

// NewCompositor opens a session-bus connection and verifies the
// extension is reachable. Returns ErrExtensionNotInstalled (wrapped) if
// the extension didn't respond — caller is expected to either fall back
// to x11.Compositor or surface a setup hint to the user.
func NewCompositor() (*Compositor, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect to session bus: %w", err)
	}

	c := &Compositor{
		conn: conn,
		obj:  conn.Object(BusName, ObjectPath),
	}

	// Liveness probe — GetShortcut is cheap and present from day-one of
	// the extension, so it doubles as "is anything answering?".
	var shortcut string
	if err := c.obj.Call(Interface+".GetShortcut", 0).Store(&shortcut); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: %v", ErrExtensionNotInstalled, err)
	}
	return c, nil
}

func (c *Compositor) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// CaptureTarget asks the extension for the currently-focused window.
// Returns an empty Target (no window class, no id) when nothing has
// focus — the injector treats that as "user moved on, skip insertion"
// rather than retrying.
func (c *Compositor) CaptureTarget(ctx context.Context) (platform.Target, error) {
	var wmClass, title, id string
	if err := c.obj.CallWithContext(ctx, Interface+".GetFocusedWindow", 0).Store(&wmClass, &title, &id); err != nil {
		return platform.Target{}, fmt.Errorf("GetFocusedWindow: %w", err)
	}
	return platform.Target{
		WindowID:    id,
		WindowClass: wmClass,
		WindowName:  title,
	}, nil
}

func (c *Compositor) ActivateWindow(ctx context.Context, target platform.Target) error {
	var ok bool
	if err := c.obj.CallWithContext(ctx, Interface+".ActivateWindow", 0, target.WindowID).Store(&ok); err != nil {
		return fmt.Errorf("ActivateWindow(%s): %w", target.WindowID, err)
	}
	if !ok {
		// Window vanished between capture and insertion — common when
		// the user closed the app mid-recording. Surface as an error so
		// the injector can decide whether to retry or give up.
		return fmt.Errorf("ActivateWindow(%s): window not found", target.WindowID)
	}
	return nil
}

func (c *Compositor) SendKeys(ctx context.Context, combo string) error {
	var ok bool
	if err := c.obj.CallWithContext(ctx, Interface+".SendKeys", 0, combo).Store(&ok); err != nil {
		return fmt.Errorf("SendKeys(%s): %w", combo, err)
	}
	if !ok {
		return fmt.Errorf("SendKeys(%s): extension rejected combo (unknown key?)", combo)
	}
	return nil
}

func (c *Compositor) ReleaseModifiers(ctx context.Context, keys []string) error {
	var ok bool
	if err := c.obj.CallWithContext(ctx, Interface+".ReleaseModifiers", 0, keys).Store(&ok); err != nil {
		return fmt.Errorf("ReleaseModifiers(%v): %w", keys, err)
	}
	if !ok {
		return fmt.Errorf("ReleaseModifiers(%v): extension rejected", keys)
	}
	return nil
}

func (c *Compositor) SetClipboard(ctx context.Context, text string) error {
	if err := c.obj.CallWithContext(ctx, Interface+".SetClipboard", 0, text).Err; err != nil {
		return fmt.Errorf("SetClipboard: %w", err)
	}
	return nil
}

func (c *Compositor) GetClipboard(ctx context.Context) (string, error) {
	var text string
	if err := c.obj.CallWithContext(ctx, Interface+".GetClipboard", 0).Store(&text); err != nil {
		return "", fmt.Errorf("GetClipboard: %w", err)
	}
	return text, nil
}

// Type currently returns ErrTypeUnsupported on the gnome path —
// per-keysym typing through Clutter would work but is unused (the
// active insertion mode is auto→clipboard, and InsertLive is dead
// code today). Wire it up if/when insertion.mode=type lands.
func (c *Compositor) Type(ctx context.Context, target platform.Target, text string, useWindow bool) error {
	return platform.ErrTypeUnsupported
}
