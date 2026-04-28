// Vocis Hotkey Bridge — GNOME Shell extension.
//
// Registers a global accelerator via Mutter and exposes Activated/Deactivated
// D-Bus signals so the vocis daemon (running as a normal user process) can
// implement hold-to-talk dictation on GNOME Wayland, where global hotkeys
// from third-party processes are otherwise unreachable.
//
// Press detection: Mutter's accelerator-activated signal fires when the user
// presses ctrl+shift+space. Release detection: poll global modifier state
// every POLL_INTERVAL_MS until ctrl OR shift is no longer held, then emit
// Deactivated. Polling is gated to only the time between Activated and
// Deactivated, so steady-state cost is zero.
//
// The extension also exposes window/keyboard/clipboard primitives so the
// daemon can paste text, restore focus, and synthesize Enter without
// shelling out to xdotool / xclip — both of which are unreliable on
// Wayland and which are missing entirely on GNOME-only setups.
//
// D-Bus surface (well-known name `io.github.duboisf.Vocis.Hotkey`,
// path `/io/github/duboisf/Vocis/Hotkey`,
// interface `io.github.duboisf.Vocis.Hotkey`):
//   signal Activated(s shortcut)
//   signal Deactivated(s shortcut)
//   signal Tapped(s shortcut)
//   method GetShortcut() -> s
//   method GetFocusedWindow() -> (s wm_class, s title, s id)
//   method ActivateWindow(s id) -> b ok
//   method SendKeys(s combo) -> b ok
//   method ReleaseModifiers(as keys) -> b ok
//   method SetClipboard(s text)
//   method GetClipboard() -> s text
//
// Tapped fires when the user releases and re-presses the trigger key
// without releasing the modifiers. On the daemon side this toggles
// per-session submit mode (Enter-after-paste), matching the behavior
// of the X11 backend's auto-repeat-filtered press detection.

import GLib from 'gi://GLib';
import Gio from 'gi://Gio';
import Meta from 'gi://Meta';
import Shell from 'gi://Shell';
import Clutter from 'gi://Clutter';
import St from 'gi://St';

import {Extension} from 'resource:///org/gnome/shell/extensions/extension.js';
import * as Main from 'resource:///org/gnome/shell/ui/main.js';
import * as PanelMenu from 'resource:///org/gnome/shell/ui/panelMenu.js';
import * as PopupMenu from 'resource:///org/gnome/shell/ui/popupMenu.js';

const ACCELERATOR = '<Ctrl><Shift>space';
const SHORTCUT_LABEL = 'ctrl+shift+space';
const POLL_INTERVAL_MS = 30;

const BUS_NAME = 'io.github.duboisf.Vocis.Hotkey';
const OBJECT_PATH = '/io/github/duboisf/Vocis/Hotkey';
const INTERFACE_NAME = 'io.github.duboisf.Vocis.Hotkey';

const INTERFACE_XML = `
<node>
  <interface name="${INTERFACE_NAME}">
    <signal name="Activated">
      <arg type="s" name="shortcut"/>
    </signal>
    <signal name="Deactivated">
      <arg type="s" name="shortcut"/>
    </signal>
    <signal name="Tapped">
      <arg type="s" name="shortcut"/>
    </signal>
    <method name="GetShortcut">
      <arg direction="out" type="s" name="shortcut"/>
    </method>
    <method name="GetFocusedWindow">
      <arg direction="out" type="s" name="wm_class"/>
      <arg direction="out" type="s" name="title"/>
      <arg direction="out" type="s" name="id"/>
    </method>
    <method name="ActivateWindow">
      <arg direction="in" type="s" name="id"/>
      <arg direction="out" type="b" name="ok"/>
    </method>
    <method name="SendKeys">
      <arg direction="in" type="s" name="combo"/>
      <arg direction="out" type="b" name="ok"/>
    </method>
    <method name="ReleaseModifiers">
      <arg direction="in" type="as" name="keys"/>
      <arg direction="out" type="b" name="ok"/>
    </method>
    <method name="SetClipboard">
      <arg direction="in" type="s" name="text"/>
    </method>
    <method name="GetClipboard">
      <arg direction="out" type="s" name="text"/>
    </method>
  </interface>
</node>`;

// Maps a vocis-config key name (case-insensitive) to a Clutter keysym. Only
// keys actually used by vocis insertion paths are listed — extending later
// is cheap. Single ASCII characters (a-z, A-Z, 0-9, punctuation) are
// resolved separately via Clutter.unicode_to_keysym at call time.
const KEY_ALIASES = {
    'ctrl': Clutter.KEY_Control_L,
    'control': Clutter.KEY_Control_L,
    'shift': Clutter.KEY_Shift_L,
    'alt': Clutter.KEY_Alt_L,
    'meta': Clutter.KEY_Super_L,
    'super': Clutter.KEY_Super_L,
    'win': Clutter.KEY_Super_L,
    'return': Clutter.KEY_Return,
    'enter': Clutter.KEY_Return,
    'tab': Clutter.KEY_Tab,
    'space': Clutter.KEY_space,
    'escape': Clutter.KEY_Escape,
    'esc': Clutter.KEY_Escape,
    'backspace': Clutter.KEY_BackSpace,
    'delete': Clutter.KEY_Delete,
    'home': Clutter.KEY_Home,
    'end': Clutter.KEY_End,
    'pageup': Clutter.KEY_Page_Up,
    'pagedown': Clutter.KEY_Page_Down,
    'up': Clutter.KEY_Up,
    'down': Clutter.KEY_Down,
    'left': Clutter.KEY_Left,
    'right': Clutter.KEY_Right,
};

export default class VocisHotkeyExtension extends Extension {
    enable() {
        this._actionId = 0;
        this._activatedHandler = 0;
        this._pollSourceId = 0;
        this._busOwnerId = 0;
        this._dbusImpl = null;
        this._isHeld = false;
        this._keyboardDevice = null;
        this._panelButton = null;

        this._exportDbus();
        this._registerAccelerator();
        this._addPanelButton();
    }

    disable() {
        this._stopPolling();
        this._unregisterAccelerator();
        this._unexportDbus();
        this._removePanelButton();
        this._keyboardDevice = null;
    }

    // -- D-Bus -------------------------------------------------------------

    _exportDbus() {
        this._dbusImpl = Gio.DBusExportedObject.wrapJSObject(INTERFACE_XML, {
            GetShortcut: () => SHORTCUT_LABEL,
            GetFocusedWindow: () => this._getFocusedWindow(),
            ActivateWindow: (id) => this._activateWindow(id),
            SendKeys: (combo) => this._sendKeys(combo),
            ReleaseModifiers: (keys) => this._releaseModifiers(keys),
            SetClipboard: (text) => this._setClipboard(text),
            GetClipboardAsync: (params, invocation) => this._getClipboardAsync(invocation),
        });
        this._dbusImpl.export(Gio.DBus.session, OBJECT_PATH);

        this._busOwnerId = Gio.bus_own_name(
            Gio.BusType.SESSION,
            BUS_NAME,
            Gio.BusNameOwnerFlags.NONE,
            null,
            null,
            () => {
                console.warn(`[vocis] failed to acquire bus name ${BUS_NAME} — another instance running?`);
            },
        );
    }

    _unexportDbus() {
        if (this._busOwnerId !== 0) {
            Gio.bus_unown_name(this._busOwnerId);
            this._busOwnerId = 0;
        }
        if (this._dbusImpl) {
            this._dbusImpl.unexport();
            this._dbusImpl = null;
        }
    }

    _emitSignal(name) {
        if (!this._dbusImpl) return;
        this._dbusImpl.emit_signal(name, GLib.Variant.new('(s)', [SHORTCUT_LABEL]));
    }

    // Returns [wm_class, title, id] for the currently focused Mutter window.
    // wm_class follows X11 WM_CLASS for XWayland windows and the Wayland
    // app_id for native Wayland clients. id is Mutter's stable uint64 window
    // ID stringified — it is NOT an X11 window ID and cannot be passed to
    // xdotool. Returns ['', '', ''] when no window has focus.
    _getFocusedWindow() {
        const win = global.display.get_focus_window();
        if (!win) {
            return ['', '', ''];
        }
        const wmClass = win.get_wm_class() || '';
        const title = win.get_title() || '';
        const id = String(win.get_id());
        return [wmClass, title, id];
    }

    // Re-focuses the Mutter window with the given stringified id (as
    // returned by GetFocusedWindow). Returns false if no such window is
    // currently mapped — the daemon should treat that as "user moved on,
    // skip insertion" rather than retry.
    _activateWindow(id) {
        if (!id) return false;
        const target = String(id);
        for (const win of global.get_window_actors().map(a => a.meta_window)) {
            if (!win) continue;
            if (String(win.get_id()) === target) {
                win.activate(global.get_current_time());
                return true;
            }
        }
        console.warn(`[vocis] ActivateWindow: no window with id=${target}`);
        return false;
    }

    // Synthesizes a key combo such as "ctrl+v", "ctrl+shift+v", or
    // "Return" via Mutter's virtual keyboard. All non-final tokens are
    // treated as modifiers; the final token is the action key. Returns
    // false on parse error (unknown key) so the daemon can log and
    // surface the failure rather than silently dropping the paste.
    _sendKeys(combo) {
        const parts = combo.split('+').map(p => p.trim()).filter(Boolean);
        if (parts.length === 0) {
            console.warn('[vocis] SendKeys: empty combo');
            return false;
        }

        const keyvals = [];
        for (const part of parts) {
            const keyval = this._resolveKeyval(part);
            if (keyval === 0) {
                console.warn(`[vocis] SendKeys: unknown key "${part}" in combo "${combo}"`);
                return false;
            }
            keyvals.push(keyval);
        }

        const device = this._virtualKeyboard();
        const time = global.get_current_time();

        // Press in left-to-right order, release in reverse — same shape as
        // xdotool's `key` so apps see the same event sequence they would
        // from a real keyboard.
        for (const k of keyvals) {
            device.notify_keyval(time, k, Clutter.KeyState.PRESSED);
        }
        for (let i = keyvals.length - 1; i >= 0; i--) {
            device.notify_keyval(time, keyvals[i], Clutter.KeyState.RELEASED);
        }
        return true;
    }

    // Releases each named modifier without pressing it first. Mirrors
    // `xdotool keyup` and is used after dictation to drop the still-held
    // hotkey modifiers so the synthesized paste shortcut isn't combined
    // with them by the focused app.
    _releaseModifiers(keys) {
        if (!keys || keys.length === 0) return true;
        const device = this._virtualKeyboard();
        const time = global.get_current_time();
        for (const key of keys) {
            const keyval = this._resolveKeyval(key);
            if (keyval === 0) {
                console.warn(`[vocis] ReleaseModifiers: unknown key "${key}"`);
                continue;
            }
            device.notify_keyval(time, keyval, Clutter.KeyState.RELEASED);
        }
        return true;
    }

    _setClipboard(text) {
        St.Clipboard.get_default().set_text(St.ClipboardType.CLIPBOARD, text || '');
    }

    // GetClipboard is async because St.Clipboard.get_text uses a callback
    // — GJS lets us complete the D-Bus invocation manually so the caller
    // sees a normal sync method.
    _getClipboardAsync(invocation) {
        St.Clipboard.get_default().get_text(St.ClipboardType.CLIPBOARD, (_clipboard, text) => {
            invocation.return_value(GLib.Variant.new('(s)', [text || '']));
        });
    }

    // -- Panel button ------------------------------------------------------

    // Adds a microphone icon to the top bar with a single popup item to
    // disable the extension. The "Disable" action shells out to
    // `gnome-extensions disable` rather than calling Extension.disable()
    // directly — disabling from inside a click handler unwinds the very
    // object handling the click, which gjs warns about.
    _addPanelButton() {
        this._panelButton = new PanelMenu.Button(0.0, 'Vocis', false);

        const icon = new St.Icon({
            icon_name: 'audio-input-microphone-symbolic',
            style_class: 'system-status-icon',
        });
        this._panelButton.add_child(icon);

        const item = new PopupMenu.PopupMenuItem('Disable vocis-gnome');
        item.connect('activate', () => {
            const uuid = this.metadata?.uuid || 'vocis@duboisf.github.io';
            GLib.spawn_command_line_async(`gnome-extensions disable ${uuid}`);
        });
        this._panelButton.menu.addMenuItem(item);

        Main.panel.addToStatusArea('vocis', this._panelButton);
    }

    _removePanelButton() {
        if (this._panelButton) {
            this._panelButton.destroy();
            this._panelButton = null;
        }
    }

    _virtualKeyboard() {
        if (!this._keyboardDevice) {
            const seat = Clutter.get_default_backend().get_default_seat();
            this._keyboardDevice = seat.create_virtual_device(Clutter.InputDeviceType.KEYBOARD_DEVICE);
        }
        return this._keyboardDevice;
    }

    // Resolves a vocis-config-style key name to a Clutter keysym. Returns
    // 0 on failure so callers can detect parse errors. Aliases are looked
    // up case-insensitively; single Unicode chars fall through to
    // Clutter.unicode_to_keysym.
    _resolveKeyval(key) {
        const lower = key.toLowerCase();
        if (KEY_ALIASES[lower] !== undefined) return KEY_ALIASES[lower];
        if (key.length === 1) {
            const ksym = Clutter.unicode_to_keysym(key.charCodeAt(0));
            return ksym || 0;
        }
        return 0;
    }

    // -- Accelerator -------------------------------------------------------

    _registerAccelerator() {
        // IGNORE_AUTOREPEAT tells Mutter to deliver accelerator-activated
        // only on the initial press of each combo, not during the OS
        // key-repeat stream. With it set, any subsequent activation
        // while we still believe the combo is held can only mean the
        // user released and re-pressed the trigger key — a tap. Without
        // it, autorepeat at ~30 Hz is indistinguishable from a tap.
        const flags = Meta.KeyBindingFlags.IGNORE_AUTOREPEAT
            ?? Meta.KeyBindingFlags.NONE;
        const modes = Shell.ActionMode.NORMAL
            | Shell.ActionMode.OVERVIEW
            | Shell.ActionMode.POPUP;

        this._actionId = global.display.grab_accelerator(ACCELERATOR, flags);
        if (this._actionId === Meta.KeyBindingAction.NONE || this._actionId === 0) {
            console.warn(`[vocis] grab_accelerator(${ACCELERATOR}) failed — another app may own it`);
            return;
        }

        // The accelerator is registered with Mutter, but Main.wm gates which
        // shell action modes are allowed to fire it. Opt into the normal
        // foreground modes so the daemon receives the press.
        const name = Meta.external_binding_name_for_action(this._actionId);
        Main.wm.allowKeybinding(name, modes);

        this._activatedHandler = global.display.connect(
            'accelerator-activated',
            (_display, action, _deviceId, _timestamp) => {
                if (action !== this._actionId) return;
                this._onActivated();
            },
        );
    }

    _unregisterAccelerator() {
        if (this._activatedHandler !== 0) {
            global.display.disconnect(this._activatedHandler);
            this._activatedHandler = 0;
        }
        if (this._actionId !== 0) {
            global.display.ungrab_accelerator(this._actionId);
            this._actionId = 0;
        }
    }

    // -- Press / release ---------------------------------------------------

    _onActivated() {
        // With IGNORE_AUTOREPEAT set on the grab, Mutter fires
        // accelerator-activated only on a real press transition. So a
        // re-firing while we still hold the combo means the user
        // released and re-pressed the trigger key without releasing
        // the modifiers — a tap. We surface that as a Tapped signal so
        // the daemon can toggle submit mode without breaking out of the
        // active dictation.
        if (this._isHeld) {
            this._emitSignal('Tapped');
            return;
        }
        this._isHeld = true;
        this._emitSignal('Activated');
        this._startPolling();
    }

    _onDeactivated() {
        if (!this._isHeld) return;
        this._isHeld = false;
        this._stopPolling();
        this._emitSignal('Deactivated');
    }

    _startPolling() {
        if (this._pollSourceId !== 0) return;
        this._pollSourceId = GLib.timeout_add(
            GLib.PRIORITY_DEFAULT,
            POLL_INTERVAL_MS,
            () => this._pollOnce(),
        );
    }

    _stopPolling() {
        if (this._pollSourceId !== 0) {
            GLib.source_remove(this._pollSourceId);
            this._pollSourceId = 0;
        }
    }

    _pollOnce() {
        const [, , mods] = global.get_pointer();
        const ctrlHeld = (mods & Clutter.ModifierType.CONTROL_MASK) !== 0;
        const shiftHeld = (mods & Clutter.ModifierType.SHIFT_MASK) !== 0;
        if (ctrlHeld && shiftHeld) {
            return GLib.SOURCE_CONTINUE;
        }
        // Either modifier was released — treat the gesture as ended.
        this._pollSourceId = 0;
        this._onDeactivated();
        return GLib.SOURCE_REMOVE;
    }
}

