package recorder

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidRecordingRejectsHeaderOnlyAudio(t *testing.T) {
	t.Parallel()

	path := writePCMRecording(t, 16000, 1, nil)
	err := validRecording(path)
	if err == nil {
		t.Fatal("expected header-only recording to be rejected")
	}
}

func TestValidRecordingRejectsShortPCM(t *testing.T) {
	t.Parallel()

	samples := make([]int16, 16*40)
	path := writePCMRecording(t, 16000, 1, samples)

	err := validRecording(path)
	if err == nil {
		t.Fatal("expected short recording to be rejected")
	}
}

func TestValidRecordingAcceptsDurationsAboveThreshold(t *testing.T) {
	t.Parallel()

	samples := make([]int16, 16000)
	path := writePCMRecording(t, 16000, 1, samples)

	if err := validRecording(path); err != nil {
		t.Fatalf("expected recording to be valid: %v", err)
	}
}

func TestWAVDurationUsesHeaderData(t *testing.T) {
	t.Parallel()

	samples := make([]int16, 32000)
	path := writePCMRecording(t, 16000, 2, samples)

	duration, err := wavDuration(path)
	if err != nil {
		t.Fatalf("wav duration: %v", err)
	}
	if duration != time.Second {
		t.Fatalf("duration = %s, want %s", duration, time.Second)
	}
}

func TestLevelMeterDropsToZeroWhenStale(t *testing.T) {
	t.Parallel()

	meter := &levelMeter{}
	meter.Update([]int16{0, 8000, -12000, 4000})
	if got := meter.Level(); got <= 0 {
		t.Fatalf("level = %f, want > 0", got)
	}

	meter.mu.Lock()
	meter.updatedAt = time.Now().Add(-300 * time.Millisecond)
	meter.mu.Unlock()

	if got := meter.Level(); got != 0 {
		t.Fatalf("stale level = %f, want 0", got)
	}
}

func writePCMRecording(t *testing.T, sampleRate, channels int, samples []int16) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "sample.wav")

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp recording: %v", err)
	}

	wav, err := newWAVFile(file, sampleRate, channels)
	if err != nil {
		t.Fatalf("new wav file: %v", err)
	}
	if len(samples) > 0 {
		if _, err := wav.Write(samples); err != nil {
			t.Fatalf("write samples: %v", err)
		}
	}
	if err := wav.Close(); err != nil {
		t.Fatalf("close wav file: %v", err)
	}

	return path
}
