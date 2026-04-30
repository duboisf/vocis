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

// TestFindWindow_ReturnsFlattenedSnapshot verifies that findWindow
// walks the kitty @ ls JSON tree and returns the matching window's
// title + foreground process info. This is the diagnostic data the
// inject layer logs at capture and post-send to triage "transcript
// disappeared" reports — if the parsing breaks, those logs go silent.
func TestFindWindow_ReturnsFlattenedSnapshot(t *testing.T) {
	t.Parallel()
	oss := []osWin{
		{
			ID: 1, IsFocused: true, IsActive: true,
			Tabs: []kittyTab{
				{
					ID: 1, IsFocused: true, IsActive: true,
					Windows: []kittyWin{
						{
							ID: 4, IsFocused: true, IsActive: true,
							Title: "claude — vocis",
							ForegroundProcesses: []kittyProcess{
								{Cmdline: []string{"claude", "--resume"}, PID: 12345, Cwd: "/home/fred/git/vocis"},
							},
						},
					},
				},
			},
		},
	}
	got := findWindow(oss, "4")
	if got == nil {
		t.Fatal("findWindow(_, \"4\") = nil, want a snapshot")
	}
	if got.ID != 4 {
		t.Errorf("ID = %d, want 4", got.ID)
	}
	if got.Title != "claude — vocis" {
		t.Errorf("Title = %q, want %q", got.Title, "claude — vocis")
	}
	if got.ForegroundCmd != "claude --resume" {
		t.Errorf("ForegroundCmd = %q, want %q", got.ForegroundCmd, "claude --resume")
	}
	if got.ForegroundPID != 12345 {
		t.Errorf("ForegroundPID = %d, want 12345", got.ForegroundPID)
	}
	if !got.IsFocused {
		t.Errorf("IsFocused = false, want true")
	}
}

func TestFindWindow_NoMatchReturnsNil(t *testing.T) {
	t.Parallel()
	oss := []osWin{
		{
			Tabs: []kittyTab{
				{Windows: []kittyWin{{ID: 4, Title: "shell"}}},
			},
		},
	}
	if got := findWindow(oss, "99"); got != nil {
		t.Fatalf("findWindow(_, \"99\") = %+v, want nil for no matching id", got)
	}
}

// TestFindWindow_EmptyForegroundProcesses ensures the snapshot stays
// well-formed (empty cmd, zero pid) when kitty reports a window with
// no foreground processes — e.g. a freshly-detached pty. We must not
// panic indexing into an empty slice.
func TestFindWindow_EmptyForegroundProcesses(t *testing.T) {
	t.Parallel()
	oss := []osWin{
		{
			Tabs: []kittyTab{
				{Windows: []kittyWin{{ID: 7, Title: "scratch"}}},
			},
		},
	}
	got := findWindow(oss, "7")
	if got == nil {
		t.Fatal("findWindow returned nil for matching id")
	}
	if got.ForegroundCmd != "" || got.ForegroundPID != 0 {
		t.Fatalf("expected zero-valued foreground fields, got cmd=%q pid=%d", got.ForegroundCmd, got.ForegroundPID)
	}
}
