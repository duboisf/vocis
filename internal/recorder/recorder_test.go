package recorder

import (
	"errors"
	"testing"
	"time"
)

func TestValidRecordingDurationRejectsZeroAudio(t *testing.T) {
	t.Parallel()

	err := validRecordingDuration(0)
	if err == nil {
		t.Fatal("expected zero-duration recording to be rejected")
	}
	if !errors.Is(err, ErrRecordingTooShort) {
		t.Fatalf("expected ErrRecordingTooShort, got %v", err)
	}
}

func TestValidRecordingDurationRejectsShortCapture(t *testing.T) {
	t.Parallel()

	err := validRecordingDuration(40 * time.Millisecond)
	if err == nil {
		t.Fatal("expected short recording to be rejected")
	}
	if !errors.Is(err, ErrRecordingTooShort) {
		t.Fatalf("expected ErrRecordingTooShort, got %v", err)
	}
}

func TestValidRecordingDurationAcceptsLongerCapture(t *testing.T) {
	t.Parallel()

	if err := validRecordingDuration(150 * time.Millisecond); err != nil {
		t.Fatalf("expected recording to be valid: %v", err)
	}
}

func TestSessionBytesCapturedUsesFrameCount(t *testing.T) {
	t.Parallel()

	session := &Session{sampleRate: 24000, channels: 2}
	session.frames.Store(2400)

	if got, want := session.BytesCaptured(), int64(9600); got != want {
		t.Fatalf("bytes captured = %d, want %d", got, want)
	}
	if got, want := session.Duration(), 100*time.Millisecond; got != want {
		t.Fatalf("duration = %s, want %s", got, want)
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

// TestLevelMeterIsLogarithmic: quiet-but-normal speech peaks around
// 0.05 of full scale. On a linear meter that is 5% bar height, which
// reads as "mic not moving". The meter maps dB instead: -40 dB is the
// floor, 0 dB is full, so 0.05 (-26 dB) lands near a third.
func TestLevelMeterIsLogarithmic(t *testing.T) {
	m := &levelMeter{}
	m.Update([]int16{1638}) // 0.05 of full scale, about -26 dB
	if got := m.Level(); got < 0.3 || got > 0.4 {
		t.Fatalf("Level() for 0.05 peak = %.3f, want about 0.35", got)
	}
	m = &levelMeter{}
	m.Update([]int16{0})
	if got := m.Level(); got != 0 {
		t.Fatalf("Level() for silence = %.3f, want 0", got)
	}
	m = &levelMeter{}
	m.Update([]int16{32767})
	if got := m.Level(); got < 0.99 {
		t.Fatalf("Level() for full scale = %.3f, want 1", got)
	}
}
