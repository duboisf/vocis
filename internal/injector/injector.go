package injector

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/atotto/clipboard"
	"go.opentelemetry.io/otel/attribute"

	"vocis/internal/config"
	"vocis/internal/hotkeys"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

type Target struct {
	WindowID    string
	WindowClass string
	WindowName  string
}

type Injector struct {
	cfg config.InsertionConfig

	mu           sync.Mutex
	restoreTimer *time.Timer
	run          commandRunner
	releaseArgs  []string
}

type commandRunner func(ctx context.Context, name string, args ...string) (string, error)

var quotedValuePattern = regexp.MustCompile(`"([^"]+)"`)

func New(cfg config.InsertionConfig, shortcut string) *Injector {
	inj := &Injector{cfg: cfg}
	inj.run = inj.execTrimmed
	if strings.TrimSpace(shortcut) != "" {
		if keyNames, err := hotkeys.ReleaseKeyNames(shortcut); err == nil {
			inj.releaseArgs = append([]string{"keyup"}, keyNames...)
		} else {
			sessionlog.Warnf("resolve hotkey release keys: %v", err)
		}
	}
	if len(inj.releaseArgs) == 0 {
		inj.releaseArgs = buildModifierReleaseArgs()
	}
	return inj
}

func (i *Injector) CaptureTarget(ctx context.Context) (Target, error) {
	ctx, span := telemetry.StartSpan(ctx, "vocis.inject.capture_target")
	defer func() { telemetry.EndSpan(span, nil) }()

	windowID, err := i.run(ctx, "xdotool", "getactivewindow")
	if err != nil {
		telemetry.EndSpan(span, err)
		return Target{}, fmt.Errorf("active window: %w", err)
	}

	className, _ := i.run(ctx, "xdotool", "getwindowclassname", windowID)
	windowName, _ := i.run(ctx, "xdotool", "getwindowname", windowID)
	if className == "" || windowName == "" {
		fallbackClass, fallbackName := i.readWindowMetadata(ctx, windowID)
		if className == "" {
			className = fallbackClass
		}
		if windowName == "" {
			windowName = fallbackName
		}
	}

	span.SetAttributes(
		attribute.String("window.id", windowID),
		attribute.String("window.class", className),
	)
	return Target{
		WindowID:    windowID,
		WindowClass: className,
		WindowName:  windowName,
	}, nil
}

func (i *Injector) Insert(ctx context.Context, target Target, text string) error {
	if !hasVisibleText(text) {
		return nil
	}

	ctx, focusSpan := telemetry.StartSpan(ctx, "vocis.inject.focus",
		attribute.String("window.id", target.WindowID),
	)
	if target.WindowID != "" {
		if _, err := i.run(ctx, "xdotool", "windowactivate", "--sync", target.WindowID); err != nil {
			telemetry.EndSpan(focusSpan, err)
			return fmt.Errorf("restore focus: %w", err)
		}
		time.Sleep(120 * time.Millisecond)
	}
	if err := i.releaseHeldModifiers(ctx); err != nil {
		sessionlog.Warnf("release held modifiers: %v", err)
	} else {
		time.Sleep(25 * time.Millisecond)
	}
	telemetry.EndSpan(focusSpan, nil)

	mode := i.resolveMode(target.WindowClass)
	switch mode {
	case "type":
		ctx, typeSpan := telemetry.StartSpan(ctx, "vocis.inject.type",
			attribute.Int("text.length", len(text)),
		)
		err := i.typeText(ctx, target, text, true)
		telemetry.EndSpan(typeSpan, err)
		if err != nil {
			return fmt.Errorf("type text: %w", err)
		}
		return nil
	default:
		return i.paste(ctx, target, text)
	}
}

func (i *Injector) InsertLive(ctx context.Context, target Target, text string) error {
	if !hasVisibleText(text) {
		return nil
	}
	if err := i.focusTarget(ctx, target); err != nil {
		return err
	}
	if err := i.releaseHeldModifiers(ctx); err != nil {
		sessionlog.Warnf("release held modifiers: %v", err)
	} else {
		time.Sleep(25 * time.Millisecond)
	}
	sessionlog.Infof(
		"typing live segment into window=%s class=%q",
		target.WindowID,
		target.WindowClass,
	)
	if err := i.typeText(ctx, target, text, false); err != nil {
		return fmt.Errorf("type live segment: %w", err)
	}
	return nil
}

func (i *Injector) paste(ctx context.Context, target Target, text string) error {
	ctx, span := telemetry.StartSpan(ctx, "vocis.inject.paste",
		attribute.Int("text.length", len(text)),
		attribute.Bool("terminal", i.isTerminal(target.WindowClass)),
	)
	defer func() { telemetry.EndSpan(span, nil) }()

	if err := i.focusTarget(ctx, target); err != nil {
		telemetry.EndSpan(span, err)
		return err
	}

	originalClipboard := ""
	if i.cfg.RestoreClipboard {
		clip, err := clipboard.ReadAll()
		if err == nil {
			originalClipboard = clip
		}
	}

	if err := clipboard.WriteAll(text); err != nil {
		telemetry.EndSpan(span, err)
		return fmt.Errorf("clipboard write: %w", err)
	}

	pasteKey := i.cfg.DefaultPasteKey
	isTerminal := i.isTerminal(target.WindowClass)
	if isTerminal {
		pasteKey = i.cfg.TerminalPasteKey
	}
	span.SetAttributes(attribute.String("paste.key", pasteKey))
	sessionlog.Infof(
		"pasting transcript into window=%s class=%q terminal=%t key=%s",
		target.WindowID,
		target.WindowClass,
		isTerminal,
		pasteKey,
	)

	args := buildPasteArgs(pasteKey)

	if _, err := i.run(ctx, "xdotool", args...); err != nil {
		telemetry.EndSpan(span, err)
		return fmt.Errorf("paste text: %w", err)
	}

	if i.cfg.RestoreClipboard {
		i.mu.Lock()
		if i.restoreTimer != nil {
			i.restoreTimer.Stop()
		}
		restore := originalClipboard
		i.restoreTimer = time.AfterFunc(250*time.Millisecond, func() {
			if restore == "" {
				return
			}
			_ = clipboard.WriteAll(restore)
		})
		i.mu.Unlock()
	}

	return nil
}

func (i *Injector) focusTarget(ctx context.Context, target Target) error {
	if target.WindowID == "" {
		return nil
	}
	if _, err := i.run(ctx, "xdotool", "windowactivate", "--sync", target.WindowID); err != nil {
		return fmt.Errorf("restore focus: %w", err)
	}
	time.Sleep(120 * time.Millisecond)
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

func buildPasteArgs(pasteKey string) []string {
	args := []string{"key", "--clearmodifiers"}
	args = append(args, normalizeKeyChord(pasteKey)...)
	return args
}

func buildTypeArgs(typeDelayMS int, target Target, text string, useWindow bool) []string {
	args := []string{"type", "--clearmodifiers"}
	if typeDelayMS >= 0 {
		args = append(args, "--delay", fmt.Sprintf("%d", typeDelayMS))
	}
	if useWindow && target.WindowID != "" {
		args = append(args, "--window", target.WindowID)
	}
	args = append(args, "--", text)
	return args
}

func buildModifierReleaseArgs() []string {
	return []string{
		"keyup",
		"Control_L",
		"Control_R",
		"Shift_L",
		"Shift_R",
		"Alt_L",
		"Alt_R",
		"Super_L",
		"Super_R",
	}
}

func buildKeyReleaseArgs(shortcut string) ([]string, error) {
	names, err := hotkeys.ReleaseKeyNames(shortcut)
	if err != nil {
		return nil, err
	}
	return append([]string{"keyup"}, names...), nil
}

func normalizeKeyChord(chord string) []string {
	chord = strings.ReplaceAll(strings.ToLower(chord), "+", "+")
	return []string{chord}
}

func (i *Injector) typeText(
	ctx context.Context,
	target Target,
	text string,
	useWindow bool,
) error {
	args := buildTypeArgs(i.cfg.TypeDelayMS, target, text, useWindow)
	if _, err := i.run(ctx, "xdotool", args...); err != nil {
		return err
	}
	return nil
}

func (i *Injector) releaseHeldModifiers(ctx context.Context) error {
	args := i.releaseArgs
	if len(args) == 0 {
		args = buildModifierReleaseArgs()
	}
	if _, err := i.run(ctx, "xdotool", args...); err != nil {
		return err
	}
	return nil
}

func hasVisibleText(text string) bool {
	return strings.TrimSpace(text) != ""
}

func (i *Injector) execTrimmed(ctx context.Context, name string, args ...string) (string, error) {
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
	output, err := i.run(ctx, "xprop", "-id", windowID, "WM_CLASS", "WM_NAME")
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
