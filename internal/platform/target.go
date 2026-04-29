package platform

// Target identifies the window that was active when recording started.
// The fields are platform-agnostic strings — X11 uses numeric window IDs,
// Wayland would use different identifiers.
type Target struct {
	WindowID    string
	WindowClass string
	WindowName  string
	// KittyWindowID is the stable kitty internal window id (a tab/pane
	// inside a kitty OS window) captured at recording start when the
	// target is a kitty terminal and `kitty @ ls` is reachable. The
	// inject layer prefers this over WindowID so paste lands in the
	// originally-targeted tab/pane even after the user switches kitty
	// tabs mid-recording. Empty for non-kitty windows or when kitty
	// remote control isn't available.
	KittyWindowID string
}
