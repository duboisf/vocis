package x11

import (
	"os"
	"testing"

	"github.com/BurntSushi/xgbutil"

	"vocis/internal/ui"
)

func TestShouldAnimatePartialFromFirstWord(t *testing.T) {
	t.Parallel()

	if !ui.ShouldAnimatePartial("", "hello world") {
		t.Fatal("expected first partial to animate")
	}
}

func TestShouldAnimatePartialFromHelperBody(t *testing.T) {
	t.Parallel()

	current := ui.DisplayedListeningText(ui.ListeningBody(""))
	if current != "" {
		t.Fatalf("ui.DisplayedListeningText(helper) = %q, want empty", current)
	}

	if !ui.ShouldAnimatePartial(current, "hello world") {
		t.Fatal("expected first transcript words to animate from helper body")
	}
}

func TestShouldAnimatePartialOnlyWhenTextExtends(t *testing.T) {
	t.Parallel()

	if !ui.ShouldAnimatePartial("hello", "hello world") {
		t.Fatal("expected growing partial to animate")
	}
	if ui.ShouldAnimatePartial("hello world", "hello") {
		t.Fatal("expected shrinking/revised partial not to animate")
	}
	if ui.ShouldAnimatePartial("hello world", "goodbye world") {
		t.Fatal("expected unrelated partial not to animate")
	}
}

func TestDisplayedListeningTextTreatsHelperAsEmpty(t *testing.T) {
	t.Parallel()

	if got := ui.DisplayedListeningText(ui.ListeningBody("")); got != "" {
		t.Fatalf("ui.DisplayedListeningText(helper) = %q, want empty", got)
	}
	if got := ui.DisplayedListeningText(ui.ListeningBody("hello world")); got != "hello world" {
		t.Fatalf("ui.DisplayedListeningText(transcript) = %q, want hello world", got)
	}
}

func TestWrapLinesShortTextSingleLine(t *testing.T) {
	t.Parallel()

	lines := ui.WrapLines("hello world", 60)
	if len(lines) != 1 || lines[0] != "hello world" {
		t.Fatalf("wrapLines = %v, want [hello world]", lines)
	}
}

func TestWrapLinesLongTextWraps(t *testing.T) {
	t.Parallel()

	lines := ui.WrapLines("the quick brown fox jumps over the lazy dog", 20)
	if len(lines) < 2 {
		t.Fatalf("expected multiple lines, got %v", lines)
	}
	for _, line := range lines {
		if len([]rune(line)) > 20 {
			t.Fatalf("line %q exceeds 20 chars", line)
		}
	}
}

func TestWrapLinesEmptyReturnsNil(t *testing.T) {
	t.Parallel()

	lines := ui.WrapLines("", 60)
	if lines != nil {
		t.Fatalf("wrapLines empty = %v, want nil", lines)
	}
}

func TestShortenUsesASCIIEllipsis(t *testing.T) {
	t.Parallel()

	got := ui.Shorten("hello world", 8)
	if got != "hello..." {
		t.Fatalf("shorten = %q, want hello...", got)
	}
}

func TestGrabEscapeAndUngrab(t *testing.T) {
	t.Parallel()

	if os.Getenv("DISPLAY") == "" {
		t.Skip("no DISPLAY set")
	}

	xu, err := xgbutil.NewConn()
	if err != nil {
		t.Skipf("cannot open X connection: %v", err)
	}
	defer xu.Conn().Close()

	o := &Overlay{x: xu}
	ch := o.GrabEscape()
	if ch == nil {
		t.Fatal("GrabEscape returned nil channel")
	}
	if !o.escapeGrabbed {
		t.Fatal("expected escapeGrabbed to be true")
	}

	// Double grab should return same channel.
	ch2 := o.GrabEscape()
	if ch != ch2 {
		t.Fatal("expected same channel on double grab")
	}

	// Ungrab should not panic.
	o.UngrabEscape()
	if o.escapeGrabbed {
		t.Fatal("expected escapeGrabbed to be false after ungrab")
	}
}

func TestUngrabEscapeIsIdempotent(t *testing.T) {
	t.Parallel()

	o := &Overlay{}

	// Should not panic when not grabbed.
	o.UngrabEscape()
	o.UngrabEscape()
}
