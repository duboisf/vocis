package audiocapture

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vocis/internal/config"
)

// fakeDir points XDG_STATE_HOME at a t.TempDir() so Dir() resolves
// inside the test sandbox. Returns the resolved audio dir.
func fakeDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	d, err := Dir()
	if err != nil {
		t.Fatalf("Dir(): %v", err)
	}
	return d
}

func TestWriter_DisabledIsNoOp(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	w, err := NewWriter(config.AudioCaptureConfig{Enabled: false})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if w == nil {
		t.Fatal("disabled writer should be non-nil so call sites stay branch-free")
	}
	if w.Enabled() {
		t.Fatal("disabled writer reports Enabled()=true")
	}
	if path := w.WriteChunk("vad_stopped", []byte("RIFF....")); path != "" {
		t.Fatalf("disabled WriteChunk wrote a file: %s", path)
	}
}

func TestWriter_FilenameAndCounter(t *testing.T) {
	dir := fakeDir(t)
	w, err := NewWriter(config.AudioCaptureConfig{
		Enabled:           true,
		TTLSeconds:        3600,
		GCIntervalSeconds: 600,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	wav := []byte("RIFFfake")
	p1 := w.WriteChunk("vad_stopped", wav)
	p2 := w.WriteChunk("vad_stopped", wav)
	p3 := w.WriteChunk("samples_closed-trailing", wav)

	if p1 == "" || p2 == "" || p3 == "" {
		t.Fatalf("expected three non-empty paths, got %q %q %q", p1, p2, p3)
	}

	for _, want := range []string{"chunk001", "chunk002", "chunk003"} {
		matched := false
		for _, p := range []string{p1, p2, p3} {
			if strings.Contains(filepath.Base(p), want) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("no file matched counter %s in %q %q %q", want, p1, p2, p3)
		}
	}

	if !strings.Contains(filepath.Base(p3), "samples_closed-trailing") {
		t.Errorf("reason slug missing from %s", p3)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Errorf("expected 3 wav files in %s, got %d", dir, len(entries))
	}
}

func TestWriter_EmptyWAVNoOp(t *testing.T) {
	fakeDir(t)
	w, _ := NewWriter(config.AudioCaptureConfig{Enabled: true, TTLSeconds: 60, GCIntervalSeconds: 30})
	if got := w.WriteChunk("vad_stopped", nil); got != "" {
		t.Errorf("empty WAV produced a path: %s", got)
	}
	if got := w.WriteChunk("vad_stopped", []byte{}); got != "" {
		t.Errorf("empty WAV produced a path: %s", got)
	}
}

func TestSlugifyReason(t *testing.T) {
	cases := map[string]string{
		"vad_stopped":             "vad_stopped",
		"samples_closed-trailing": "samples_closed-trailing",
		"weird/reason with space": "weird_reason_with_space",
		"":                        "unknown",
		"   ":                     "unknown",
		"!!!":                     "unknown",
	}
	for in, want := range cases {
		if got := slugifyReason(in); got != want {
			t.Errorf("slugifyReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSweep_DeletesStaleKeepsFresh(t *testing.T) {
	dir := fakeDir(t)

	stale := filepath.Join(dir, "20260515-100000-chunk001-vad_stopped.wav")
	fresh := filepath.Join(dir, "20260515-100001-chunk002-vad_stopped.wav")
	nonWAV := filepath.Join(dir, "notes.txt")

	for _, p := range []string{stale, fresh, nonWAV} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("backdate stale: %v", err)
	}

	deleted := sweep(dir, 1*time.Hour)
	if deleted != 1 {
		t.Errorf("sweep deleted %d, want 1", deleted)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale wav still present: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh wav missing: %v", err)
	}
	if _, err := os.Stat(nonWAV); err != nil {
		t.Errorf("non-wav file was deleted: %v", err)
	}
}

func TestStartGC_DisabledIsNoOp(t *testing.T) {
	dir := fakeDir(t)
	stale := filepath.Join(dir, "20260515-100000-chunk001-vad_stopped.wav")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	old := time.Now().Add(-24 * time.Hour)
	_ = os.Chtimes(stale, old, old)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartGC(ctx, config.AudioCaptureConfig{Enabled: false, TTLSeconds: 1, GCIntervalSeconds: 1})
	// A disabled GC must not delete the stale file even after a moment.
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("disabled GC deleted stale file: %v", err)
	}
}

func TestStartGC_StartupSweepRuns(t *testing.T) {
	dir := fakeDir(t)
	stale := filepath.Join(dir, "20260515-100000-chunk001-vad_stopped.wav")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	old := time.Now().Add(-24 * time.Hour)
	_ = os.Chtimes(stale, old, old)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartGC(ctx, config.AudioCaptureConfig{Enabled: true, TTLSeconds: 60, GCIntervalSeconds: 3600})

	// Startup sweep runs asynchronously inside the goroutine; poll.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stale); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("stale file not deleted by startup sweep within deadline")
}
