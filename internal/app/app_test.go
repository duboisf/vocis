package app

import (
	"context"
	"testing"
	"time"

	"vocis/internal/config"
	"vocis/internal/platform"
	"vocis/internal/transcribe"
)

func TestHandleDictationEventUpdatesOverlayWithPartialText(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	app := &App{cfg: config.Config{}, overlay: fakeOverlay}
	state := &recordingState{target: platform.Target{WindowClass: "Gedit"}}

	app.handleDictationEvent(state, transcribe.DictationEvent{
		Type: transcribe.DictationEventPartial,
		Text: "hello world ",
	})
	if fakeOverlay.windowClass != "Gedit" {
		t.Fatalf("windowClass = %q, want Gedit", fakeOverlay.windowClass)
	}
	if fakeOverlay.listeningText != "hello world" {
		t.Fatalf("listeningText = %q, want hello world", fakeOverlay.listeningText)
	}

	// A later partial replaces the previous one in place.
	app.handleDictationEvent(state, transcribe.DictationEvent{
		Type: transcribe.DictationEventPartial,
		Text: "hello world again",
	})
	if fakeOverlay.listeningText != "hello world again" {
		t.Fatalf("listeningText = %q, want replaced partial", fakeOverlay.listeningText)
	}
}

func TestHandleUpDoesNothingWhenNotRecording(t *testing.T) {
	t.Parallel()

	app := &App{
		cfg: config.Config{
			HotkeyMode: "hold",
		},
		overlay: &overlayStub{},
	}

	app.handleUp(context.Background())

	if app.recording != nil || app.finishing != nil {
		t.Fatal("expected no dictation state")
	}
}

func TestHandleDownDismissesOldOverlayWhileTranscribing(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	cfg := config.Default()
	cfg.HotkeyMode = "hold"
	cancelled := false
	state := &recordingState{cancel: func() { cancelled = true }}
	app := &App{
		cfg:       cfg,
		overlay:   fakeOverlay,
		finishing: state,
	}

	app.handleDown(context.Background())

	if fakeOverlay.warningText == "" {
		t.Fatal("expected cancellation warning overlay")
	}
	if !app.dismissed(state) || !cancelled {
		t.Fatal("expected finishing state to be dismissed and cancelled")
	}
	if app.finishing != nil {
		t.Fatal("expected finishing to be cleared")
	}
}

// TestHandleDownAfterDeliveryStartsNewSessionInsteadOfCancelling
// reproduces the kitty fade-out race: once the transcript has been
// pasted into kitty and (if applicable) submit Enter has fired, the
// user should be able to start a fresh dictation immediately. Before
// the fix, finishRecording only cleared `transcribing` via a deferred
// call that ran AFTER overlay.Hide()'s 320ms fade-out animation, so a
// fast user pressing the hotkey within that window hit
// dismissInFlightOverlay() and got a stray "Cancelled" warning instead
// of a new recording session.
func TestHandleDownAfterDeliveryStartsNewSessionInsteadOfCancelling(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	cfg := config.Default()
	cfg.HotkeyMode = "hold"
	state := &recordingState{cancel: func() {}}
	app := &App{
		cfg:       cfg,
		overlay:   fakeOverlay,
		finishing: state, // simulate "post-Insert, deferred clear hasn't run yet"
	}

	// markDelivered is the post-paste hook finishRecording calls once
	// the transcript is in the destination — well before overlay
	// fade-out completes.
	app.markDelivered(state)

	if app.finishing != nil {
		t.Fatal("markDelivered should have cleared finishing")
	}
	if app.dismissInFlightOverlay() {
		t.Fatal("dismissInFlightOverlay must return false once delivery is done")
	}
	if fakeOverlay.warningText != "" {
		t.Fatalf("expected no Cancelled warning, got %q", fakeOverlay.warningText)
	}
}

type overlayStub struct {
	windowClass   string
	listeningText string
	warningText   string
	hideCalls     int
}

func (o *overlayStub) ShowHint(string)              {}
func (o *overlayStub) ShowListening(string, string) {}
func (o *overlayStub) SetConnected(string)          {}
func (o *overlayStub) SetLoadingModel(string)       {}
func (o *overlayStub) SetSubmitMode(bool)           {}
func (o *overlayStub) ShowFinishing(string, string) {}
func (o *overlayStub) SetFinishingText(string)      {}
func (o *overlayStub) ShowError(error)              {}
func (o *overlayStub) ShowWarning(text string)      { o.warningText = text }
func (o *overlayStub) SetLevel(float64)             {}
func (o *overlayStub) Hide() {
	o.hideCalls++
}
func (o *overlayStub) Close() {}
func (o *overlayStub) SetListeningText(windowClass, text string) {
	o.windowClass = windowClass
	o.listeningText = text
}

func TestDrainPrerollCapturesUntilStop(t *testing.T) {
	src := make(chan []int16, 4)
	stop := drainPreroll(src)

	src <- []int16{1, 2, 3}
	src <- []int16{4, 5}
	// Wait for the drain goroutine to consume both chunks. Polling
	// with a deadline beats a fixed sleep — passes fast on healthy
	// machines, doesn't flake on slow CI.
	deadline := time.Now().Add(500 * time.Millisecond)
	for len(src) > 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	snap := stop()
	if len(snap.chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(snap.chunks))
	}
	if snap.samples != 5 {
		t.Fatalf("got %d samples, want 5", snap.samples)
	}
	// stop() is idempotent — second call is suppressed by sync.Once
	// and returns the zero value.
	snap2 := stop()
	if len(snap2.chunks) != 0 {
		t.Fatalf("stop() should be a no-op on second call; got %d chunks", len(snap2.chunks))
	}
}

func TestDrainPrerollHandlesClosedSource(t *testing.T) {
	src := make(chan []int16, 2)
	src <- []int16{9, 9}
	close(src)

	stop := drainPreroll(src)
	deadline := time.Now().Add(500 * time.Millisecond)
	for len(src) > 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	snap := stop()
	if len(snap.chunks) != 1 || snap.samples != 2 {
		t.Fatalf("got chunks=%d samples=%d, want 1 chunk / 2 samples", len(snap.chunks), snap.samples)
	}
}

func TestWrapSamplesWithPrerollEmitsPrerollFirstThenLive(t *testing.T) {
	preroll := [][]int16{{1, 2}, {3}}
	live := make(chan []int16, 2)
	live <- []int16{4, 5}
	live <- []int16{6}
	close(live)

	out := wrapSamplesWithPreroll(preroll, live)

	var got [][]int16
	for chunk := range out {
		got = append(got, chunk)
	}
	want := [][]int16{{1, 2}, {3}, {4, 5}, {6}}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d", len(got), len(want))
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("chunk %d length: got %d, want %d", i, len(got[i]), len(want[i]))
		}
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("chunk %d sample %d: got %d, want %d", i, j, got[i][j], want[i][j])
			}
		}
	}
}

func TestWrapSamplesWithPrerollEmptyPreroll(t *testing.T) {
	live := make(chan []int16, 1)
	live <- []int16{42}
	close(live)

	out := wrapSamplesWithPreroll(nil, live)
	chunks := 0
	for range out {
		chunks++
	}
	if chunks != 1 {
		t.Fatalf("got %d chunks, want 1", chunks)
	}
}
