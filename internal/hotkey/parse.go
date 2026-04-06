package hotkey

import (
	"fmt"
	"strconv"
	"strings"
)

// ReleaseKeyNames returns the X key names that should be released
// when the given shortcut is released (e.g., both Control_L and
// Control_R for "ctrl").
func ReleaseKeyNames(shortcut string) ([]string, error) {
	parts := strings.FieldsFunc(strings.ToLower(shortcut), func(r rune) bool {
		return r == '+' || r == '-'
	})
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid hotkey %q", shortcut)
	}

	names := make([]string, 0, len(parts)+4)
	for _, part := range parts[:len(parts)-1] {
		modNames := ModifierKeyNames(part)
		if len(modNames) == 0 {
			return nil, fmt.Errorf("unsupported modifier %q", part)
		}
		names = append(names, modNames...)
	}

	keyName, ok := ParseKey(parts[len(parts)-1])
	if !ok {
		return nil, fmt.Errorf("unsupported key %q", parts[len(parts)-1])
	}
	names = append(names, keyName)
	return names, nil
}

// ParseSequence converts a user-facing shortcut string (e.g., "ctrl+shift+space")
// into the xgbutil keybind format (e.g., "control-shift-space").
func ParseSequence(shortcut string) (string, error) {
	parts := strings.FieldsFunc(strings.ToLower(shortcut), func(r rune) bool {
		return r == '+' || r == '-'
	})
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid hotkey %q", shortcut)
	}

	mods := make([]string, 0, len(parts)-1)
	for _, part := range parts[:len(parts)-1] {
		mod, ok := ParseModifier(part)
		if !ok {
			return "", fmt.Errorf("unsupported modifier %q", part)
		}
		mods = append(mods, mod)
	}

	key, ok := ParseKey(parts[len(parts)-1])
	if !ok {
		return "", fmt.Errorf("unsupported key %q", parts[len(parts)-1])
	}
	return strings.Join(append(mods, key), "-"), nil
}

// ModifierKeyNames returns the X key names for a modifier string.
func ModifierKeyNames(part string) []string {
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

// ParseModifier converts a user-facing modifier name to xgbutil format.
func ParseModifier(part string) (string, bool) {
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

// ParseKey converts a user-facing key name to X key name.
func ParseKey(part string) (string, bool) {
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
		if err == nil && n >= 1 && n <= 12 {
			return fmt.Sprintf("F%d", n), true
		}
	}

	return "", false
}
