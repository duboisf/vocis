package x11

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/atotto/clipboard"
	"go.opentelemetry.io/otel/attribute"

	"vocis/internal/platform"
	"vocis/internal/telemetry"
)

// Compositor implements platform.Compositor for pure-X11 sessions.
// CaptureTarget / ActivateWindow / SendKeys / ReleaseModifiers all shell
// out to xdotool; SetClipboard / GetClipboard go through xclip via
// github.com/atotto/clipboard.
//
// The struct is intentionally tiny — all the dictation logic
// (terminal-class detection, paste-key resolution, clipboard
// save/restore) lives in inject.Injector and reaches xdotool through
// this Compositor so the same flow works against the gnome backend.
type Compositor struct {
	run commandRunner
}

// commandRunner abstracts the exec call so tests can assert what
// xdotool / xprop arguments are emitted without actually invoking the
// real binaries.
type commandRunner func(ctx context.Context, name string, args ...string) (string, error)

var quotedValuePattern = regexp.MustCompile(`"([^"]+)"`)

func NewCompositor() *Compositor {
	c := &Compositor{}
	c.run = c.execTrimmed
	return c
}

// SetCommandRunner swaps the exec hook — used by tests to assert what
// xdotool / xprop arguments the Compositor emits without touching the
// real binaries.
func (c *Compositor) SetCommandRunner(run commandRunner) {
	c.run = run
}

func (c *Compositor) CaptureTarget(ctx context.Context) (platform.Target, error) {
	ctx, span := telemetry.StartSpan(ctx, "vocis.capture_target")
	defer func() { telemetry.EndSpan(span, nil) }()
	span.SetAttributes(attribute.String("capture.source", "xdotool"))

	windowID, err := c.run(ctx, "xdotool", "getactivewindow")
	if err != nil {
		telemetry.EndSpan(span, err)
		return platform.Target{}, fmt.Errorf("get focused window: %w", err)
	}
	className, _ := c.run(ctx, "xdotool", "getwindowclassname", windowID)
	windowName, _ := c.run(ctx, "xdotool", "getwindowname", windowID)
	if className == "" {
		// Some apps (notably JetBrains and Steam) leave WM_CLASS empty
		// on the X11 window. Fall back to xprop which can read the
		// instance name even when xdotool returns nothing.
		fallback, _ := c.readWindowClass(ctx, windowID)
		className = fallback
	}
	span.SetAttributes(
		attribute.String("window.id", windowID),
		attribute.String("window.class", className),
	)
	return platform.Target{
		WindowID:    windowID,
		WindowClass: className,
		WindowName:  windowName,
	}, nil
}

func (c *Compositor) ActivateWindow(ctx context.Context, target platform.Target) error {
	if target.WindowID == "" {
		return nil
	}
	if _, err := c.run(ctx, "xdotool", "windowactivate", "--sync", target.WindowID); err != nil {
		return fmt.Errorf("activate window: %w", err)
	}
	return nil
}

func (c *Compositor) SendKeys(ctx context.Context, combo string) error {
	args := []string{"key", "--clearmodifiers", strings.ToLower(combo)}
	if _, err := c.run(ctx, "xdotool", args...); err != nil {
		return fmt.Errorf("send keys %q: %w", combo, err)
	}
	return nil
}

func (c *Compositor) ReleaseModifiers(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	args := append([]string{"keyup"}, keys...)
	if _, err := c.run(ctx, "xdotool", args...); err != nil {
		return fmt.Errorf("release modifiers: %w", err)
	}
	return nil
}

func (c *Compositor) SetClipboard(ctx context.Context, text string) error {
	if err := clipboard.WriteAll(text); err != nil {
		return fmt.Errorf("clipboard write: %w", err)
	}
	return nil
}

func (c *Compositor) GetClipboard(ctx context.Context) (string, error) {
	text, err := clipboard.ReadAll()
	if err != nil {
		return "", fmt.Errorf("clipboard read: %w", err)
	}
	return text, nil
}

func (c *Compositor) Type(ctx context.Context, target platform.Target, text string, useWindow bool) error {
	args := []string{"type", "--clearmodifiers", "--delay", "1"}
	if useWindow && target.WindowID != "" {
		args = append(args, "--window", target.WindowID)
	}
	args = append(args, "--", text)
	if _, err := c.run(ctx, "xdotool", args...); err != nil {
		return fmt.Errorf("type text: %w", err)
	}
	return nil
}

func (c *Compositor) execTrimmed(ctx context.Context, name string, args ...string) (string, error) {
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

// readWindowClass falls back to xprop for windows where xdotool returns
// no WM_CLASS. Some apps don't set the class until first map; xprop
// reads the live property directly.
func (c *Compositor) readWindowClass(ctx context.Context, windowID string) (string, error) {
	output, err := c.run(ctx, "xprop", "-id", windowID, "WM_CLASS")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "WM_CLASS") {
			continue
		}
		matches := quotedValuePattern.FindAllStringSubmatch(line, -1)
		if len(matches) == 0 {
			continue
		}
		return matches[len(matches)-1][1], nil
	}
	return "", nil
}
