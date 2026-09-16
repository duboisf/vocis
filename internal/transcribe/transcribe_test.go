package transcribe

import (
	"os"
	"testing"
)

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
