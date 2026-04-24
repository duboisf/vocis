package recall

import (
	"strings"
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

func TestConcatSegmentPCMBasic(t *testing.T) {
	segs := []*Segment{
		{ID: 1, SampleRate: 16000, PCM: []int16{1, 2, 3}},
		{ID: 2, SampleRate: 16000, PCM: []int16{4, 5}},
		{ID: 3, SampleRate: 16000, PCM: []int16{6}},
	}

	// No gap → plain concatenation.
	pcm, sr, err := concatSegmentPCM(segs, 0, 0)
	if err != nil {
		t.Fatalf("concat: %v", err)
	}
	if sr != 16000 {
		t.Fatalf("sample rate: got %d, want 16000", sr)
	}
	want := []int16{1, 2, 3, 4, 5, 6}
	if len(pcm) != len(want) {
		t.Fatalf("len(pcm)=%d, want %d", len(pcm), len(want))
	}
	for i := range want {
		if pcm[i] != want[i] {
			t.Fatalf("pcm[%d]=%d, want %d", i, pcm[i], want[i])
		}
	}
}

func TestConcatSegmentPCMGapInsertsSilence(t *testing.T) {
	// 1ms gap at 16 kHz = 16 samples of silence between each pair.
	segs := []*Segment{
		{ID: 1, SampleRate: 16000, PCM: []int16{1, 1, 1}},
		{ID: 2, SampleRate: 16000, PCM: []int16{2, 2}},
		{ID: 3, SampleRate: 16000, PCM: []int16{3}},
	}
	pcm, _, err := concatSegmentPCM(segs, 1, 0)
	if err != nil {
		t.Fatalf("concat: %v", err)
	}
	// Expected length: 3 + 16 + 2 + 16 + 1 = 38
	if want := 3 + 16 + 2 + 16 + 1; len(pcm) != want {
		t.Fatalf("len(pcm)=%d, want %d", len(pcm), want)
	}
	// Verify the two silence regions are zero.
	for i := 3; i < 3+16; i++ {
		if pcm[i] != 0 {
			t.Fatalf("gap sample pcm[%d]=%d, want 0", i, pcm[i])
		}
	}
	for i := 3 + 16 + 2; i < 3+16+2+16; i++ {
		if pcm[i] != 0 {
			t.Fatalf("gap sample pcm[%d]=%d, want 0", i, pcm[i])
		}
	}
}

func TestConcatSegmentPCMMixedSampleRate(t *testing.T) {
	segs := []*Segment{
		{ID: 1, SampleRate: 16000, PCM: []int16{1}},
		{ID: 2, SampleRate: 24000, PCM: []int16{2}},
	}
	if _, _, err := concatSegmentPCM(segs, 0, 0); err == nil {
		t.Fatal("expected error on mixed sample rates, got nil")
	} else if !strings.Contains(err.Error(), "mixed sample rates") {
		t.Fatalf("expected 'mixed sample rates' error, got: %v", err)
	}
}

func TestConcatSegmentPCMEmpty(t *testing.T) {
	if _, _, err := concatSegmentPCM(nil, 0, 0); err == nil {
		t.Fatal("expected error on empty input, got nil")
	}
}

func TestConcatSegmentPCMMaxSecondsCap(t *testing.T) {
	// 2 seconds of audio at 16 kHz = 32000 samples per segment.
	// Two segments = 4 s; cap at 3 s must reject it.
	seg := func(id int64) *Segment {
		pcm := make([]int16, 32000)
		return &Segment{ID: id, SampleRate: 16000, PCM: pcm}
	}
	segs := []*Segment{seg(1), seg(2)}
	if _, _, err := concatSegmentPCM(segs, 0, 3); err == nil {
		t.Fatal("expected cap error, got nil")
	} else if !strings.Contains(err.Error(), "exceeds batch_max_seconds") {
		t.Fatalf("expected cap error, got: %v", err)
	}
	// Cap at 0 disables the check — same segments should succeed.
	if _, _, err := concatSegmentPCM(segs, 0, 0); err != nil {
		t.Fatalf("cap=0 should disable the check, got: %v", err)
	}
}
