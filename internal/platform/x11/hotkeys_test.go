package x11

import (
	"testing"

	"vocis/internal/hotkey"
)

func TestReleaseKeyNamesIncludesModifiersAndTriggerKey(t *testing.T) {
	t.Parallel()

	got, err := hotkey.ReleaseKeyNames("ctrl+shift+space")
	if err != nil {
		t.Fatalf("ReleaseKeyNames: %v", err)
	}

	want := []string{"Control_L", "Control_R", "Shift_L", "Shift_R", "space"}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q; got=%v", i, got[i], want[i], got)
		}
	}
}
