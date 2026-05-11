package recall

import (
	"testing"
	"time"
)

func TestSegmentIDsWithinWindow(t *testing.T) {
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	segs := []SegmentInfo{
		{ID: 1, StartedAt: now.Add(-30 * time.Minute)}, // out
		{ID: 2, StartedAt: now.Add(-9 * time.Minute)},  // in
		{ID: 3, StartedAt: now.Add(-1 * time.Minute)},  // in
		{ID: 4, StartedAt: now.Add(-11 * time.Minute)}, // out — note: order doesn't matter, filter is per-segment
	}
	got := SegmentIDsWithinWindow(segs, now, 10*time.Minute)
	want := []int64{2, 3}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSegmentIDsWithinWindowEdgeAtCutoff(t *testing.T) {
	// A segment whose StartedAt equals the cutoff exactly should be
	// included — the filter uses Before(cutoff), not <=.
	now := time.Now()
	segs := []SegmentInfo{
		{ID: 1, StartedAt: now.Add(-10 * time.Minute)},
	}
	got := SegmentIDsWithinWindow(segs, now, 10*time.Minute)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("segment exactly at cutoff should be kept; got %v", got)
	}
}
