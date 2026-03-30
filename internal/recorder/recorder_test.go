package recorder

import (
	"testing"
	"time"
)

func TestValidRecordingDurationRejectsZeroAudio(t *testing.T) {
	t.Parallel()

	if err := validRecordingDuration(0); err == nil {
		t.Fatal("expected zero-duration recording to be rejected")
	}
}

func TestValidRecordingDurationRejectsShortCapture(t *testing.T) {
	t.Parallel()

	if err := validRecordingDuration(40 * time.Millisecond); err == nil {
		t.Fatal("expected short recording to be rejected")
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
