package platform

// Target identifies the window that was active when recording started.
// The fields are platform-agnostic strings — X11 uses numeric window IDs,
// Wayland would use different identifiers.
type Target struct {
	WindowID    string
	WindowClass string
	WindowName  string
}
