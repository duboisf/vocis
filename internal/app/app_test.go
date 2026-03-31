package app

import (
	"context"
	"testing"

	"vocis/internal/config"
	"vocis/internal/injector"
	"vocis/internal/openai"
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
		target: injector.Target{WindowClass: "Gedit"},
	}

	err := app.handleDictationEvent(context.Background(), state, openai.DictationEvent{
		Type: openai.DictationEventPartial,
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

func TestPartialPrependsAccumulatedSegments(t *testing.T) {
	t.Parallel()

	fakeOverlay := &overlayStub{}
	app := &App{
		cfg: config.Config{
			Streaming: config.StreamingConfig{ShowPartialOverlay: true},
		},
		overlay: fakeOverlay,
	}
	state := &recordingState{
		target:      injector.Target{WindowClass: "Gedit"},
		liveText:    "Hello world.",
		displayText: "Hello world.",
	}

	_ = app.handleDictationEvent(context.Background(), state, openai.DictationEvent{
		Type: openai.DictationEventPartial,
		Text: "this is more",
	})

	if fakeOverlay.listeningText != "Hello world. this is more" {
		t.Fatalf("listeningText = %q, want accumulated + partial", fakeOverlay.listeningText)
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
		target:      injector.Target{WindowClass: "Gedit"},
		liveText:    "Hello world.",
		displayText: "Hello world.",
	}

	// Set initial text so we can detect if it gets cleared.
	fakeOverlay.listeningText = "Hello world."

	_ = app.handleDictationEvent(context.Background(), state, openai.DictationEvent{
		Type: openai.DictationEventPartial,
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
		target: injector.Target{WindowID: "42", WindowClass: "Gedit"},
	}
	app.recording = state

	for _, seg := range []string{"segment one", " segment two"} {
		err := app.handleDictationEvent(context.Background(), state, openai.DictationEvent{
			Type: openai.DictationEventSegment,
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
	app := &App{
		cfg: config.Config{
			HotkeyMode: "hold",
		},
		overlay:      fakeOverlay,
		transcribing: true,
	}

	app.handleDown(context.Background())

	if fakeOverlay.hideCalls != 1 {
		t.Fatalf("hideCalls = %d, want 1", fakeOverlay.hideCalls)
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
	hideCalls      int
}

func (o *overlayStub) ShowHint(string)      {}
func (o *overlayStub) ShowListening(string, string) {}
func (o *overlayStub) AnimateChunk(text string) {
	o.animatedChunks = append(o.animatedChunks, text)
}
func (o *overlayStub) ShowFinishing(string, string) {}
func (o *overlayStub) ShowSuccess(text string) {
	o.successText = text
}
func (o *overlayStub) ShowError(error)  {}
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

func (i *injectorStub) CaptureTarget(context.Context) (injector.Target, error) {
	return injector.Target{}, nil
}

func (i *injectorStub) Insert(_ context.Context, _ injector.Target, text string) error {
	if i.err != nil {
		return i.err
	}
	i.inserted = append(i.inserted, text)
	return nil
}

func (i *injectorStub) InsertLive(_ context.Context, _ injector.Target, text string) error {
	if i.err != nil {
		return i.err
	}
	i.liveInserted = append(i.liveInserted, text)
	return nil
}

