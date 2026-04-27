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
	app := &App{
		cfg: config.Config{
			Streaming: config.StreamingConfig{ShowPartialOverlay: true},
		},
		overlay: fakeOverlay,
	}
	state := &recordingState{
		target: platform.Target{WindowClass: "Gedit"},
	}

	err := app.handleDictationEvent(context.Background(), state, transcribe.DictationEvent{
		Type: transcribe.DictationEventPartial,
		Text: "hello world",
	})
	if err != nil {
		t.Fatalf("handleDictationEvent: %v", err)
	}

	if fakeOverlay.windowClass != "Gedit" {
		t.Fatalf("windowClass = %q, want Gedit", fakeOverlay.windowClass)
	}
	if fakeOverlay.listeningText != "hello world" {
		t.Fatalf("listeningText = %q, want hello world", fakeOverlay.listeningText)
	}
}

// TestPartialAppendsBelowAccumulatedSegments locks down live-subtitle
// behavior: the in-flight partial renders on its own line below the
// committed segments. When the matching `completed` event arrives, the
// canonical text replaces the partial in place (covered separately).
func TestPartialAppendsBelowAccumulatedSegments(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	app := &App{
		cfg: config.Config{
			Streaming: config.StreamingConfig{ShowPartialOverlay: true},
		},
		overlay: fakeOverlay,
	}
	state := &recordingState{
		target:      platform.Target{WindowClass: "Gedit"},
		liveText:    "Hello world.",
		displayText: "Hello world.",
	}

	_ = app.handleDictationEvent(context.Background(), state, transcribe.DictationEvent{
		Type: transcribe.DictationEventPartial,
		Text: "this is more",
	})

	if fakeOverlay.listeningText != "Hello world.\nthis is more" {
		t.Fatalf("listeningText = %q, want committed + newline + partial", fakeOverlay.listeningText)
	}
	if state.currentPartial != "this is more" {
		t.Fatalf("currentPartial = %q, want %q", state.currentPartial, "this is more")
	}
}

// TestPartialReplacesPreviousPartial confirms the in-place update
// behavior — newer partial overwrites the previous one rather than
// appending.
func TestPartialReplacesPreviousPartial(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	app := &App{
		cfg: config.Config{
			Streaming: config.StreamingConfig{ShowPartialOverlay: true},
		},
		overlay: fakeOverlay,
	}
	state := &recordingState{
		target:         platform.Target{WindowClass: "Gedit"},
		displayText:    "Hello world.",
		currentPartial: "this is",
	}

	_ = app.handleDictationEvent(context.Background(), state, transcribe.DictationEvent{
		Type: transcribe.DictationEventPartial,
		Text: "this is more text",
	})

	if fakeOverlay.listeningText != "Hello world.\nthis is more text" {
		t.Fatalf("listeningText = %q, want previous partial replaced", fakeOverlay.listeningText)
	}
}

// TestSegmentClearsPartial confirms that when a turn completes, the
// canonical segment text replaces the in-flight partial (no double
// rendering of "this is more text" + "this is more text, you know").
func TestSegmentClearsPartial(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	app := &App{
		cfg: config.Config{
			Streaming: config.StreamingConfig{ShowPartialOverlay: true},
		},
		overlay: fakeOverlay,
	}
	state := &recordingState{
		target:         platform.Target{WindowClass: "Gedit"},
		displayText:    "Hello world.",
		currentPartial: "this is mo",
	}

	_ = app.handleDictationEvent(context.Background(), state, transcribe.DictationEvent{
		Type: transcribe.DictationEventSegment,
		Text: "this is more text, finalized.",
	})

	if state.currentPartial != "" {
		t.Fatalf("currentPartial = %q, want cleared after segment", state.currentPartial)
	}
	want := "Hello world.\nthis is more text, finalized."
	if fakeOverlay.listeningText != want {
		t.Fatalf("listeningText = %q, want %q", fakeOverlay.listeningText, want)
	}
}

func TestEmptyPartialDoesNotFlashHelperWhenSegmentsExist(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	app := &App{
		cfg: config.Config{
			Streaming: config.StreamingConfig{ShowPartialOverlay: true},
		},
		overlay: fakeOverlay,
	}
	state := &recordingState{
		target:      platform.Target{WindowClass: "Gedit"},
		liveText:    "Hello world.",
		displayText: "Hello world.",
	}

	// Set initial text so we can detect if it gets cleared.
	fakeOverlay.listeningText = "Hello world."

	_ = app.handleDictationEvent(context.Background(), state, transcribe.DictationEvent{
		Type: transcribe.DictationEventPartial,
		Text: "",
	})

	if fakeOverlay.listeningText != "Hello world." {
		t.Fatalf("listeningText = %q, want unchanged display text", fakeOverlay.listeningText)
	}
}

func TestHandleDictationEventAccumulatesSegments(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	app := &App{
		cfg: config.Config{
			HotkeyMode: "hold",
			Streaming:  config.StreamingConfig{},
		},
		overlay: fakeOverlay,
	}
	state := &recordingState{
		target: platform.Target{WindowID: "42", WindowClass: "Gedit"},
	}
	app.recording = state

	for _, seg := range []string{"segment one", " segment two"} {
		err := app.handleDictationEvent(context.Background(), state, transcribe.DictationEvent{
			Type: transcribe.DictationEventSegment,
			Text: seg,
		})
		if err != nil {
			t.Fatalf("handleDictationEvent: %v", err)
		}
	}

	if state.liveText != "segment one segment two" {
		t.Fatalf("liveText = %q, want %q", state.liveText, "segment one segment two")
	}
	if fakeOverlay.listeningText != "segment one\nsegment two" {
		t.Fatalf("listeningText = %q, want newline-separated display", fakeOverlay.listeningText)
	}
}

func TestHandleUpDoesNothingWhenNotRecording(t *testing.T) {
	t.Parallel()

	app := &App{
		cfg: config.Config{
			HotkeyMode: "hold",
			Streaming: config.StreamingConfig{
				},
		},
		overlay: &overlayStub{},
	}

	app.handleUp(context.Background())

	if app.recording != nil {
		t.Fatal("expected no recording state")
	}
	if app.transcribing {
		t.Fatal("expected transcribing to remain false")
	}
}

func TestHandleDownDismissesOldOverlayWhileTranscribing(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	cfg := config.Default()
	cfg.HotkeyMode = "hold"
	app := &App{
		cfg:          cfg,
		overlay:      fakeOverlay,
		transcribing: true,
	}

	app.handleDown(context.Background())

	if fakeOverlay.warningText == "" {
		t.Fatal("expected cancellation warning overlay")
	}
	if !app.completionOverlayDismissed() {
		t.Fatal("expected completion overlay to be dismissed")
	}
}

func TestShowCompletionSuccessStaysHiddenAfterDismiss(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	app := &App{
		overlay: fakeOverlay,
	}
	app.dismissCompletionOverlay = true

	app.showCompletionSuccess("hello")

	if fakeOverlay.successText != "" {
		t.Fatalf("successText = %q, want empty", fakeOverlay.successText)
	}
	if fakeOverlay.hideCalls != 1 {
		t.Fatalf("hideCalls = %d, want 1", fakeOverlay.hideCalls)
	}
}

type overlayStub struct {
	windowClass    string
	listeningText  string
	animatedChunks []string
	successText    string
	warningText    string
	hideCalls      int
}

func (o *overlayStub) ShowHint(string)      {}
func (o *overlayStub) ShowListening(string, string)  {}
func (o *overlayStub) SetConnected(string)            {}
func (o *overlayStub) SetConnecting(int, int)         {}
func (o *overlayStub) SetLoadingModel(string)         {}
func (o *overlayStub) SetSubmitMode(bool)             {}
func (o *overlayStub) AnimateChunk(text string) {
	o.animatedChunks = append(o.animatedChunks, text)
}
func (o *overlayStub) ShowFinishing(string, string) {}
func (o *overlayStub) SetFinishingPhase(string)     {}
func (o *overlayStub) ExtendFinishingPhase(string)  {}
func (o *overlayStub) SetFinishingText(string)                    {}
func (o *overlayStub) ShowSuccess(text string) {
	o.successText = text
}
func (o *overlayStub) ShowError(error)    {}
func (o *overlayStub) ShowWarning(text string) { o.warningText = text }
func (o *overlayStub) GrabEscape() <-chan struct{} { return make(chan struct{}) }
func (o *overlayStub) UngrabEscape()              {}
func (o *overlayStub) SetLevel(float64) {}
func (o *overlayStub) Hide() {
	o.hideCalls++
}
func (o *overlayStub) Close() {}
func (o *overlayStub) SetListeningText(windowClass, text string) {
	o.windowClass = windowClass
	o.listeningText = text
}

type injectorStub struct {
	inserted     []string
	liveInserted []string
	err          error
}

func (i *injectorStub) CaptureTarget(context.Context) (platform.Target, error) {
	return platform.Target{}, nil
}

func (i *injectorStub) Insert(_ context.Context, _ platform.Target, text string) error {
	if i.err != nil {
		return i.err
	}
	i.inserted = append(i.inserted, text)
	return nil
}

func (i *injectorStub) PressEnter(_ context.Context, _ platform.Target) error { return nil }

func (i *injectorStub) InsertLive(_ context.Context, _ platform.Target, text string) error {
	if i.err != nil {
		return i.err
	}
	i.liveInserted = append(i.liveInserted, text)
	return nil
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


