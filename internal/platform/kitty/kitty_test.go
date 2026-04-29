package kitty

import "testing"

func TestIsKitty(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"kitty":                 true,
		"Kitty":                 true,
		"  kitty  ":             true,
		"xterm-kitty":           true,
		"org.kovidgoyal.kitty":  true,
		"alacritty":             false,
		"":                      false,
		"gnome-terminal-server": false,
	}
	for in, want := range cases {
		if got := IsKitty(in); got != want {
			t.Errorf("IsKitty(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPickFocused_PrefersFocusedTriple(t *testing.T) {
	t.Parallel()
	oss := []osWin{
		{
			ID:        1,
			IsFocused: true,
			IsActive:  true,
			Tabs: []kittyTab{
				{
					ID:        1,
					IsFocused: false,
					IsActive:  true,
					Windows: []kittyWin{
						{ID: 10, IsActive: true},
					},
				},
				{
					ID:        2,
					IsFocused: true,
					IsActive:  false,
					Windows: []kittyWin{
						{ID: 20, IsFocused: false, IsActive: true},
						{ID: 21, IsFocused: true},
					},
				},
			},
		},
	}
	got, ok := pickFocused(oss)
	if !ok || got != 21 {
		t.Fatalf("pickFocused = (%d, %v), want (21, true)", got, ok)
	}
}

func TestPickFocused_FallsBackToActiveTriple(t *testing.T) {
	t.Parallel()
	// Nothing is_focused — kitty CLI reports the OS window as inactive,
	// e.g. when the kitty socket is per-OS-window and the kitty OS
	// window doesn't currently own keyboard focus. Should still
	// return the active triple so we have something useful to paste
	// into.
	oss := []osWin{
		{
			ID:       1,
			IsActive: true,
			Tabs: []kittyTab{
				{ID: 1, IsActive: false, Windows: []kittyWin{{ID: 5, IsActive: true}}},
				{
					ID:       7,
					IsActive: true,
					Windows: []kittyWin{
						{ID: 70, IsActive: false},
						{ID: 71, IsActive: true},
					},
				},
			},
		},
	}
	got, ok := pickFocused(oss)
	if !ok || got != 71 {
		t.Fatalf("pickFocused = (%d, %v), want (71, true)", got, ok)
	}
}

func TestPickFocused_EmptyJSONReturnsNotOK(t *testing.T) {
	t.Parallel()
	if _, ok := pickFocused(nil); ok {
		t.Fatal("expected pickFocused on empty input to return ok=false")
	}
}

func TestIsNoMatchError(t *testing.T) {
	t.Parallel()
	if !isNoMatchError(errFromString("kitty @ focus-window: No matching windows")) {
		t.Fatal("expected match for canonical kitty 'No matching windows' message")
	}
	if !isNoMatchError(errFromString("Some prefix: no matching window")) {
		t.Fatal("expected match for singular form too")
	}
	if isNoMatchError(errFromString("connection refused")) {
		t.Fatal("expected non-match for unrelated error")
	}
	if isNoMatchError(nil) {
		t.Fatal("nil should not be a no-match error")
	}
}

type stringErr string

func (e stringErr) Error() string { return string(e) }

func errFromString(s string) error { return stringErr(s) }
