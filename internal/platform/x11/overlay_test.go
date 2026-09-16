package x11

import (
	"testing"
	"time"

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

func TestFormatElapsedCountsUpFromZero(t *testing.T) {
	t.Parallel()

	if got := formatElapsed("Wrapping up", 0); got != "Wrapping up... (0.0s)" {
		t.Fatalf("formatElapsed(0) = %q, want %q", got, "Wrapping up... (0.0s)")
	}
	if got := formatElapsed("Wrapping up", 2300*time.Millisecond); got != "Wrapping up... (2.3s)" {
		t.Fatalf("formatElapsed(2.3s) = %q, want %q", got, "Wrapping up... (2.3s)")
	}
}
