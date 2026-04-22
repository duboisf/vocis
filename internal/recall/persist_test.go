package recall

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFilePersister_ExpandsEnvVarsAndTilde pins the portability
// contract on recall.persist.dir: $HOME (and any other env var) plus
// a leading ~/ are resolved at open time so users can keep yaml
// values portable across machines.
func TestFilePersister_ExpandsEnvVarsAndTilde(t *testing.T) {
	base := t.TempDir()
	t.Setenv("VOCIS_TEST_RECALL_BASE", base)
	t.Setenv("HOME", base) // scoped to this test by t.Setenv

	cases := []struct {
		name string
		in   string
	}{
		{"env var", "$VOCIS_TEST_RECALL_BASE/vocis/recall"},
		{"tilde", "~/vocis/recall"},
		{"env var with tilde-like segment", "$VOCIS_TEST_RECALL_BASE/sub/dir"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewFilePersister(tc.in)
			if err != nil {
				t.Fatalf("NewFilePersister(%q): %v", tc.in, err)
			}
			if _, err := os.Stat(p.Dir()); err != nil {
				t.Fatalf("directory not created at resolved path %q: %v", p.Dir(), err)
			}
			if got, want := filepath.IsAbs(p.Dir()), true; got != want {
				t.Fatalf("resolved dir %q IsAbs = %v, want %v", p.Dir(), got, want)
			}
		})
	}
}

func TestFilePersister_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p, err := NewFilePersister(dir)
	if err != nil {
		t.Fatalf("NewFilePersister: %v", err)
	}

	orig := &Segment{
		ID:         42,
		StartedAt:  time.Date(2026, 4, 21, 10, 30, 0, 0, time.UTC),
		Duration:   2500 * time.Millisecond,
		SampleRate: 16000,
		PCM:        []int16{-100, 0, 100, 32767, -32768},
		PeakLevel:  0.42,
		Transcript: "hello there",
	}

	if err := p.Save(orig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(loaded))
	}
	got := loaded[0]
	if got.ID != orig.ID {
		t.Errorf("ID: got %d, want %d", got.ID, orig.ID)
	}
	if !got.StartedAt.Equal(orig.StartedAt) {
		t.Errorf("StartedAt: got %v, want %v", got.StartedAt, orig.StartedAt)
	}
	if got.Duration != orig.Duration {
		t.Errorf("Duration: got %v, want %v", got.Duration, orig.Duration)
	}
	if got.SampleRate != orig.SampleRate {
		t.Errorf("SampleRate: got %d, want %d", got.SampleRate, orig.SampleRate)
	}
	if got.PeakLevel != orig.PeakLevel {
		t.Errorf("PeakLevel: got %v, want %v", got.PeakLevel, orig.PeakLevel)
	}
	if got.Transcript != orig.Transcript {
		t.Errorf("Transcript: got %q, want %q", got.Transcript, orig.Transcript)
	}
	if !equalInt16(got.PCM, orig.PCM) {
		t.Errorf("PCM mismatch: got %v, want %v", got.PCM, orig.PCM)
	}
}

func TestFilePersister_Delete(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewFilePersister(dir)
	p.Save(&Segment{ID: 7, StartedAt: time.Now(), SampleRate: 16000, PCM: []int16{1, 2}})

	if err := p.Delete(7); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	loaded, _ := p.Load()
	if len(loaded) != 0 {
		t.Fatalf("expected 0 segments after delete, got %d", len(loaded))
	}
	// Second Delete must not fail on missing file.
	if err := p.Delete(7); err != nil {
		t.Fatalf("Delete (missing) should be no-op, got %v", err)
	}
}

func TestFilePersister_LoadSkipsJunk(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewFilePersister(dir)
	p.Save(&Segment{ID: 1, StartedAt: time.Now(), SampleRate: 16000, PCM: []int16{1}})
	// Junk files should be ignored, not abort the load.
	_ = os.WriteFile(filepath.Join(dir, "seg-bad.json"), []byte("not json"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("ignore me"), 0o600)

	loaded, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ID != 1 {
		t.Fatalf("expected only segment 1, got %v", loaded)
	}
}

func TestRing_WithPersister_AddDeletesEvicted(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewFilePersister(dir)

	r := NewRing(2, 0) // count-only, cap 2
	r.SetPersister(p)

	base := time.Now()
	r.Add(&Segment{StartedAt: base.Add(0 * time.Second), SampleRate: 16000, PCM: []int16{1}})
	r.Add(&Segment{StartedAt: base.Add(1 * time.Second), SampleRate: 16000, PCM: []int16{2}})
	r.Add(&Segment{StartedAt: base.Add(2 * time.Second), SampleRate: 16000, PCM: []int16{3}}) // evicts id 1

	// Files on disk should be exactly the retained ones.
	loaded, _ := p.Load()
	if len(loaded) != 2 {
		t.Fatalf("expected 2 persisted segments after eviction, got %d", len(loaded))
	}
	for _, s := range loaded {
		if s.ID == 1 {
			t.Fatalf("evicted segment 1 is still persisted")
		}
	}
}

func TestRing_WithPersister_SetTranscriptRewrites(t *testing.T) {
	dir := t.TempDir()
	p, _ := NewFilePersister(dir)
	r := NewRing(10, 0)
	r.SetPersister(p)

	id := r.Add(&Segment{StartedAt: time.Now(), SampleRate: 16000, PCM: []int16{1, 2}})
	r.SetTranscript(id, "hello world")

	loaded, _ := p.Load()
	if len(loaded) != 1 || loaded[0].Transcript != "hello world" {
		t.Fatalf("persister did not store transcript: %+v", loaded)
	}
}

func TestRing_Reload_SetsNextID(t *testing.T) {
	r := NewRing(10, 0)
	r.Reload([]*Segment{
		{ID: 5, StartedAt: time.Now(), SampleRate: 16000, PCM: []int16{1}},
		{ID: 9, StartedAt: time.Now(), SampleRate: 16000, PCM: []int16{2}},
	})
	id := r.Add(&Segment{StartedAt: time.Now(), SampleRate: 16000, PCM: []int16{3}})
	if id != 10 {
		t.Fatalf("expected nextID to continue from max loaded id 9, got %d", id)
	}
}
