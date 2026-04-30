// Package kitty wraps the small subset of kitty's remote-control CLI
// (`kitty @ ls`, `kitty @ focus-window`) that vocis needs to address a
// specific kitty tab/pane rather than just "whichever kitty window has
// focus right now." This makes paste-after-dictation land in the
// originally-targeted tab even if the user switched tabs while dictating.
//
// Discovery relies on the kitty CLI's own KITTY_LISTEN_ON / `--to` /
// kitty.conf-`listen_on` resolution — vocis just shells out and parses
// the JSON. If kitty isn't installed, isn't running with remote control
// enabled, or vocis didn't inherit a usable socket address, the helpers
// fail loudly and the inject layer falls back to OS-window focus.
package kitty

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"vocis/internal/telemetry"
)

// IsKitty reports whether wmClass is one of the known kitty WM_CLASS /
// app_id values. Kitty defaults to "kitty" on X11 and on Wayland;
// "xterm-kitty" turns up via $TERM-derived class on some setups, and
// the reverse-DNS app_id "org.kovidgoyal.kitty" appears under Wayland
// when the desktop file is honored. Match all three.
func IsKitty(wmClass string) bool {
	switch strings.ToLower(strings.TrimSpace(wmClass)) {
	case "kitty", "xterm-kitty", "org.kovidgoyal.kitty":
		return true
	}
	return false
}

// osWin / kittyTab / kittyWin mirror the relevant subset of `kitty @ ls`'s
// JSON shape (kitty 0.46). Extra fields are ignored thanks to encoding/json's
// default permissive decode.
type osWin struct {
	ID        int         `json:"id"`
	IsFocused bool        `json:"is_focused"`
	IsActive  bool        `json:"is_active"`
	Tabs      []kittyTab  `json:"tabs"`
}

type kittyTab struct {
	ID        int         `json:"id"`
	IsFocused bool        `json:"is_focused"`
	IsActive  bool        `json:"is_active"`
	Windows   []kittyWin  `json:"windows"`
}

type kittyWin struct {
	ID                  int            `json:"id"`
	IsFocused           bool           `json:"is_focused"`
	IsActive            bool           `json:"is_active"`
	Title               string         `json:"title"`
	ForegroundProcesses []kittyProcess `json:"foreground_processes"`
	// InAlternateScreen is true when the foreground program has
	// switched the terminal to its alternate screen buffer (vim,
	// less, fzf, btop, claude-code's full-screen modes, etc.).
	// Programs in alt-screen frequently handle bracketed paste
	// differently — some swallow it silently, some require a key
	// event to render — so this is the single highest-signal field
	// for "transcript-disappeared" triage.
	InAlternateScreen bool `json:"in_alternate_screen"`
	// AtPrompt requires kitty's shell-integration to be active in
	// the foreground program. When true, the cursor is sitting at a
	// shell prompt ready for input; when false the program is
	// running a command (or shell integration just isn't installed).
	// Useful but optional — log it when present.
	AtPrompt bool `json:"at_prompt"`
}

// kittyProcess mirrors one entry of `kitty @ ls`'s `foreground_processes`
// array — the running program(s) attached to a window's pty. Used only
// for diagnostic logging (no decisions branch on it) so we keep just
// enough fields to identify what was running when a paste was sent.
type kittyProcess struct {
	Cmdline []string `json:"cmdline"`
	PID     int      `json:"pid"`
	Cwd     string   `json:"cwd"`
}

// WindowInfo is a point-in-time snapshot of a single kitty window's
// state. The inject layer logs one of these at capture time and again
// after `kitty @ send-text` succeeds, so a future "transcript
// disappeared" report can be triaged off the actual session log
// without re-running the bug — we can see whether the title or
// foreground process changed between capture and delivery, whether
// the window was still focused, and what program was meant to receive
// the paste.
type WindowInfo struct {
	ID                int
	Title             string
	IsFocused         bool
	IsActive          bool
	InAlternateScreen bool   // see kittyWin.InAlternateScreen
	AtPrompt          bool   // see kittyWin.AtPrompt
	ForegroundCmd     string // joined cmdline of the first foreground process; empty when kitty reports none
	ForegroundPID     int
}

// FocusedWindowID returns the stable kitty window id that currently has
// keyboard focus. When kitty itself reports nothing focused (the common
// case when called from outside a kitty-focused context), it falls back
// to the active OS window's active tab's active window — that's the
// "if you typed into kitty right now, this is where it would go" pick.
//
// Empty string + nil error means kitty replied successfully but no
// window matched; a non-nil error means the CLI itself failed (binary
// missing, no socket, remote control disabled).
func FocusedWindowID(ctx context.Context) (string, error) {
	ctx, span := telemetry.StartSpan(ctx, "vocis.kitty.focused_window_id")
	var rerr error
	defer func() { telemetry.EndSpan(span, rerr) }()

	out, err := run(ctx, "kitty", "@", "ls")
	if err != nil {
		rerr = err
		return "", err
	}
	var oss []osWin
	if err := json.Unmarshal([]byte(out), &oss); err != nil {
		rerr = err
		return "", fmt.Errorf("parse kitty @ ls JSON: %w", err)
	}
	if id, ok := pickFocused(oss); ok {
		span.SetAttributes(attribute.Int("kitty.window_id", id))
		return strconv.Itoa(id), nil
	}
	span.SetAttributes(attribute.Bool("kitty.no_focus_match", true))
	return "", nil
}

// pickFocused picks the kitty window id we want to address later.
// Strategy: prefer the genuinely focused triple; if nothing in the JSON
// is focused (common when the kitty CLI sees a per-OS-window socket
// while focus is on a different kitty OS window), fall back to the
// active triple within the active OS window. Either way, returns
// (id, true) on success and (0, false) when the JSON has no kitty
// window at all.
func pickFocused(oss []osWin) (int, bool) {
	for _, os := range oss {
		if !os.IsFocused {
			continue
		}
		for _, t := range os.Tabs {
			if !t.IsFocused {
				continue
			}
			for _, w := range t.Windows {
				if w.IsFocused {
					return w.ID, true
				}
			}
		}
	}
	for _, os := range oss {
		if !os.IsActive {
			continue
		}
		for _, t := range os.Tabs {
			if !t.IsActive {
				continue
			}
			for _, w := range t.Windows {
				if w.IsActive {
					return w.ID, true
				}
			}
		}
	}
	return 0, false
}

// LookupWindow returns a diagnostic snapshot of the kitty window with
// the given internal id, or (nil, nil) when kitty replies with no
// match (the window was closed). A non-nil error means the kitty CLI
// itself failed (binary missing, no socket, remote control disabled,
// JSON parse error). This is purely observability — no decision
// branches on the returned data — so the inject layer treats any
// failure as "skip the diagnostic, keep going" rather than fatal.
func LookupWindow(ctx context.Context, id string) (*WindowInfo, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("empty kitty window id")
	}
	ctx, span := telemetry.StartSpan(ctx, "vocis.kitty.lookup_window",
		attribute.String("kitty.window_id", id),
	)
	var rerr error
	defer func() { telemetry.EndSpan(span, rerr) }()

	out, err := run(ctx, "kitty", "@", "ls", "--match", "id:"+id)
	if err != nil {
		if isNoMatchError(err) {
			span.SetAttributes(attribute.Bool("kitty.no_match", true))
			return nil, nil
		}
		rerr = err
		return nil, err
	}
	var oss []osWin
	if err := json.Unmarshal([]byte(out), &oss); err != nil {
		rerr = fmt.Errorf("parse kitty @ ls JSON: %w", err)
		return nil, rerr
	}
	info := findWindow(oss, id)
	if info == nil {
		span.SetAttributes(attribute.Bool("kitty.no_match", true))
		return nil, nil
	}
	span.SetAttributes(
		attribute.String("kitty.title", info.Title),
		attribute.String("kitty.foreground_cmd", info.ForegroundCmd),
		attribute.Bool("kitty.is_focused", info.IsFocused),
	)
	return info, nil
}

// findWindow walks a parsed `kitty @ ls` tree for the window whose
// integer id matches the string-form id passed in, returning a
// flattened snapshot. Extracted from LookupWindow so the JSON-walking
// can be unit-tested without shelling out to a real kitty.
func findWindow(oss []osWin, id string) *WindowInfo {
	for _, os := range oss {
		for _, t := range os.Tabs {
			for _, w := range t.Windows {
				if strconv.Itoa(w.ID) != id {
					continue
				}
				info := &WindowInfo{
					ID:                w.ID,
					Title:             w.Title,
					IsFocused:         w.IsFocused,
					IsActive:          w.IsActive,
					InAlternateScreen: w.InAlternateScreen,
					AtPrompt:          w.AtPrompt,
				}
				if len(w.ForegroundProcesses) > 0 {
					info.ForegroundPID = w.ForegroundProcesses[0].PID
					info.ForegroundCmd = strings.Join(w.ForegroundProcesses[0].Cmdline, " ")
				}
				return info
			}
		}
	}
	return nil
}

// Exists reports whether a kitty window with the given internal id is
// currently mapped. We use `kitty @ ls --match id:N` rather than
// `send-text`'s own response because send-text always exits 0 even
// when the match set is empty (per kitty docs) — it can't be used as
// a liveness probe.
//
// Returns (false, nil) for "kitty replied but no window matched" so
// the caller can branch to clipboard delivery. (false, err) means the
// kitty CLI itself failed (binary missing, socket unreachable).
func Exists(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, fmt.Errorf("empty kitty window id")
	}
	ctx, span := telemetry.StartSpan(ctx, "vocis.kitty.exists",
		attribute.String("kitty.window_id", id),
	)
	var rerr error
	defer func() { telemetry.EndSpan(span, rerr) }()

	_, err := run(ctx, "kitty", "@", "ls", "--match", "id:"+id)
	if err == nil {
		span.SetAttributes(attribute.Bool("kitty.exists", true))
		return true, nil
	}
	if isNoMatchError(err) {
		span.SetAttributes(attribute.Bool("kitty.exists", false))
		return false, nil
	}
	rerr = err
	return false, err
}

// SendText delivers text to the kitty window with the given id without
// changing keyboard focus. The text is piped via stdin so multi-line
// payloads survive intact (kitty's positional `[TEXT TO SEND]` arg
// applies Python escape rules; --stdin is raw).
//
// `--bracketed-paste=auto` lets kitty wrap the payload in bracketed
// paste markers when the program in the target window has bracketed
// paste mode enabled (most modern shells, Claude Code, etc.). That
// keeps multi-line transcripts from being executed line-by-line by a
// shell while still arriving as plain keystrokes in TUIs that don't
// understand the protocol.
//
// Note: kitty @ send-text always exits 0 even on no-match — call
// Exists first if you need to know whether the text actually reached
// a window. On a real CLI error (kitty crashed, socket vanished), we
// surface that as a non-nil error.
func SendText(ctx context.Context, id, text string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("empty kitty window id")
	}
	ctx, span := telemetry.StartSpan(ctx, "vocis.kitty.send_text",
		attribute.String("kitty.window_id", id),
		attribute.Int("text.length", len(text)),
	)
	var rerr error
	defer func() { telemetry.EndSpan(span, rerr) }()

	cmd := exec.CommandContext(ctx, "kitty", "@", "send-text",
		"--match", "id:"+id,
		"--bracketed-paste=auto",
		"--stdin",
	)
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		rerr = fmt.Errorf("kitty @ send-text id:%s: %s", id, msg)
		return rerr
	}
	return nil
}

// SendEnter sends a single carriage return to the kitty window
// without changing focus and WITHOUT bracketed paste — that's the
// "submit" semantic for shells and chat-style TUIs. Bracketed paste
// would buffer the \r as part of the paste rather than triggering
// execution, so we explicitly disable it here even though SendText
// uses auto.
func SendEnter(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("empty kitty window id")
	}
	ctx, span := telemetry.StartSpan(ctx, "vocis.kitty.send_enter",
		attribute.String("kitty.window_id", id),
	)
	var rerr error
	defer func() { telemetry.EndSpan(span, rerr) }()

	cmd := exec.CommandContext(ctx, "kitty", "@", "send-text",
		"--match", "id:"+id,
		"--bracketed-paste=disable",
		"--stdin",
	)
	cmd.Stdin = strings.NewReader("\r")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		rerr = fmt.Errorf("kitty @ send-text id:%s submit: %s", id, msg)
		return rerr
	}
	return nil
}

func isNoMatchError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no matching window") ||
		strings.Contains(msg, "no matching windows")
}

// run executes name+args and returns trimmed stdout, surfacing stderr in
// the error message so callers can tell "kitty isn't on PATH" from
// "remote control is disabled."
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %v: %s", name, args, msg)
	}
	return strings.TrimSpace(string(out)), nil
}
