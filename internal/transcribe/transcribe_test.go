package transcribe

import (
	"os"
	"testing"
)

func TestAppendSegmentTextAddsSpaceBetweenChunks(t *testing.T) {
	t.Parallel()

	got := appendSegmentText("hello", "world")
	if got != "hello world" {
		t.Fatalf("appendSegmentText = %q, want hello world", got)
	}
}

func TestAppendSegmentTextRespectsLeadingPunctuation(t *testing.T) {
	t.Parallel()

	got := appendSegmentText("hello", ", world")
	if got != "hello, world" {
		t.Fatalf("appendSegmentText = %q, want hello, world", got)
	}
}

func TestStartsWithPunctuation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"hello", false},
		{",hi", true},
		{". end", true},
		{"!", true},
		{")close", true},
	}
	for _, tc := range cases {
		if got := startsWithPunctuation(tc.in); got != tc.want {
			t.Errorf("startsWithPunctuation(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestBuildHallucinationSetLowercasesAndTrims(t *testing.T) {
	t.Parallel()

	set := buildHallucinationSet([]string{"Thank you.", "  Bye.  ", ""})
	if !set["thank you."] {
		t.Fatalf("expected 'thank you.' in set, got %v", set)
	}
	if !set["bye."] {
		t.Fatalf("expected 'bye.' in set, got %v", set)
	}
	if len(set) != 2 {
		t.Fatalf("set should have 2 entries, got %d (%v)", len(set), set)
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
