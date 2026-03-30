package injector

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/atotto/clipboard"

	"vtt/internal/config"
)

type Target struct {
	WindowID    string
	WindowClass string
	WindowName  string
}

type Injector struct {
	cfg config.InsertionConfig
}

var quotedValuePattern = regexp.MustCompile(`"([^"]+)"`)

func New(cfg config.InsertionConfig) *Injector {
	return &Injector{cfg: cfg}
}

func (i *Injector) CaptureTarget(ctx context.Context) (Target, error) {
	windowID, err := i.runTrimmed(ctx, "xdotool", "getactivewindow")
	if err != nil {
		return Target{}, fmt.Errorf("active window: %w", err)
	}

	className, _ := i.runTrimmed(ctx, "xdotool", "getwindowclassname", windowID)
	windowName, _ := i.runTrimmed(ctx, "xdotool", "getwindowname", windowID)
	if className == "" || windowName == "" {
		fallbackClass, fallbackName := i.readWindowMetadata(ctx, windowID)
		if className == "" {
			className = fallbackClass
		}
		if windowName == "" {
			windowName = fallbackName
		}
	}

	return Target{
		WindowID:    windowID,
		WindowClass: className,
		WindowName:  windowName,
	}, nil
}

func (i *Injector) Insert(ctx context.Context, target Target, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	if target.WindowID != "" {
		if _, err := i.runTrimmed(ctx, "xdotool", "windowactivate", "--sync", target.WindowID); err != nil {
			return fmt.Errorf("restore focus: %w", err)
		}
		time.Sleep(120 * time.Millisecond)
	}

	mode := i.resolveMode(target.WindowClass)
	switch mode {
	case "type":
		args := []string{"type", "--clearmodifiers"}
		if i.cfg.TypeDelayMS >= 0 {
			args = append(args, "--delay", fmt.Sprintf("%d", i.cfg.TypeDelayMS))
		}
		if target.WindowID != "" {
			args = append(args, "--window", target.WindowID)
		}
		args = append(args, "--", text)
		if _, err := i.runTrimmed(ctx, "xdotool", args...); err != nil {
			return fmt.Errorf("type text: %w", err)
		}
		return nil
	default:
		return i.paste(ctx, target, text)
	}
}

func (i *Injector) paste(ctx context.Context, target Target, text string) error {
	originalClipboard := ""
	if i.cfg.RestoreClipboard {
		clip, err := clipboard.ReadAll()
		if err == nil {
			originalClipboard = clip
		}
	}

	if err := clipboard.WriteAll(text); err != nil {
		return fmt.Errorf("clipboard write: %w", err)
	}

	pasteKey := i.cfg.DefaultPasteKey
	isTerminal := i.isTerminal(target.WindowClass)
	if isTerminal {
		pasteKey = i.cfg.TerminalPasteKey
	}
	log.Printf(
		"pasting transcript into window=%s class=%q terminal=%t key=%s",
		target.WindowID,
		target.WindowClass,
		isTerminal,
		pasteKey,
	)

	args := []string{"key", "--clearmodifiers"}
	if target.WindowID != "" {
		args = append(args, "--window", target.WindowID)
	}
	args = append(args, normalizeKeyChord(pasteKey)...)

	if _, err := i.runTrimmed(ctx, "xdotool", args...); err != nil {
		return fmt.Errorf("paste text: %w", err)
	}

	if i.cfg.RestoreClipboard {
		restore := originalClipboard
		go func() {
			time.Sleep(250 * time.Millisecond)
			if restore == "" {
				return
			}
			_ = clipboard.WriteAll(restore)
		}()
	}

	return nil
}

func (i *Injector) resolveMode(windowClass string) string {
	if i.cfg.Mode != "auto" {
		return i.cfg.Mode
	}
	return "clipboard"
}

func (i *Injector) isTerminal(windowClass string) bool {
	for _, candidate := range i.cfg.TerminalClasses {
		if strings.EqualFold(candidate, windowClass) {
			return true
		}
	}
	return false
}

func normalizeKeyChord(chord string) []string {
	chord = strings.ReplaceAll(strings.ToLower(chord), "+", "+")
	return []string{chord}
}

func (i *Injector) runTrimmed(ctx context.Context, name string, args ...string) (string, error) {
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

func (i *Injector) readWindowMetadata(ctx context.Context, windowID string) (className, windowName string) {
	output, err := i.runTrimmed(ctx, "xprop", "-id", windowID, "WM_CLASS", "WM_NAME")
	if err != nil {
		return "", ""
	}
	return parseXPropMetadata(output)
}

func parseXPropMetadata(output string) (className, windowName string) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		matches := quotedValuePattern.FindAllStringSubmatch(line, -1)
		if len(matches) == 0 {
			continue
		}

		switch {
		case strings.HasPrefix(line, "WM_CLASS"):
			className = matches[len(matches)-1][1]
		case strings.HasPrefix(line, "WM_NAME"):
			windowName = matches[0][1]
		}
	}

	return className, windowName
}
