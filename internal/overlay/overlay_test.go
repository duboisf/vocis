package overlay

import "testing"

func TestShouldAnimatePartialFromFirstWord(t *testing.T) {
	t.Parallel()

	if !shouldAnimatePartial("", "hello world") {
		t.Fatal("expected first partial to animate")
	}
}

func TestShouldAnimatePartialFromHelperBody(t *testing.T) {
	t.Parallel()

	current := displayedListeningText(listeningBody(""))
	if current != "" {
		t.Fatalf("displayedListeningText(helper) = %q, want empty", current)
	}

	if !shouldAnimatePartial(current, "hello world") {
		t.Fatal("expected first transcript words to animate from helper body")
	}
}

func TestShouldAnimatePartialOnlyWhenTextExtends(t *testing.T) {
	t.Parallel()

	if !shouldAnimatePartial("hello", "hello world") {
		t.Fatal("expected growing partial to animate")
	}
	if shouldAnimatePartial("hello world", "hello") {
		t.Fatal("expected shrinking/revised partial not to animate")
	}
	if shouldAnimatePartial("hello world", "goodbye world") {
		t.Fatal("expected unrelated partial not to animate")
	}
}

func TestDisplayedListeningTextTreatsHelperAsEmpty(t *testing.T) {
	t.Parallel()

	if got := displayedListeningText(listeningBody("")); got != "" {
		t.Fatalf("displayedListeningText(helper) = %q, want empty", got)
	}
	if got := displayedListeningText(listeningBody("hello world")); got != "hello world" {
		t.Fatalf("displayedListeningText(transcript) = %q, want hello world", got)
	}
}

func TestShortenUsesASCIIEllipsis(t *testing.T) {
	t.Parallel()

	got := shorten("hello world", 8)
	if got != "hello..." {
		t.Fatalf("shorten = %q, want hello...", got)
	}
}
