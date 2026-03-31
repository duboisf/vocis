package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"vtt/internal/config"
	"vtt/internal/injector"
	"vtt/internal/openai"
)

func TestHandleStreamEventUpdatesOverlayWithPartialText(t *testing.T) {
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

	err := app.handleStreamEvent(context.Background(), state, openai.StreamEvent{
		Type: openai.StreamEventPartial,
		Text: "hello world",
	})
	if err != nil {
		t.Fatalf("handleStreamEvent: %v", err)
	}

	if fakeOverlay.windowClass != "Gedit" {
		t.Fatalf("windowClass = %q, want Gedit", fakeOverlay.windowClass)
	}
	if fakeOverlay.listeningText != "hello world" {
		t.Fatalf("listeningText = %q, want hello world", fakeOverlay.listeningText)
	}
}

func TestHandleStreamEventInsertsSegmentWhileHolding(t *testing.T) {
	t.Parallel()

	fakeInjector := &injectorStub{}
	fakeOverlay := &overlayStub{}
	app := &App{
		cfg: config.Config{
			HotkeyMode: "hold",
			Streaming:  config.StreamingConfig{Mode: "segment"},
		},
		overlay:  fakeOverlay,
		injector: fakeInjector,
	}
	state := &recordingState{
		results: make(chan streamResult, 1),
		target:  injector.Target{WindowID: "42", WindowClass: "Gedit"},
	}
	app.recording = state
	state.setLiveSegmentDelivery(true)

	err := app.handleStreamEvent(context.Background(), state, openai.StreamEvent{
		Type: openai.StreamEventFinal,
		Text: "segment one",
	})
	if err != nil {
		t.Fatalf("handleStreamEvent: %v", err)
	}

	if len(fakeInjector.liveInserted) != 1 || fakeInjector.liveInserted[0] != "segment one" {
		t.Fatalf("liveInserted = %v, want [segment one]", fakeInjector.liveInserted)
	}
	if len(fakeOverlay.animatedChunks) != 1 || fakeOverlay.animatedChunks[0] != "segment one" {
		t.Fatalf("animatedChunks = %v, want [segment one]", fakeOverlay.animatedChunks)
	}
	if !app.shouldIgnoreSyntheticUp() {
		t.Fatal("expected synthetic release guard after live insert")
	}
	select {
	case result := <-state.results:
		t.Fatalf("unexpected queued result: %+v", result)
	default:
	}
}

func TestHandleStreamEventPrefixesSpaceAfterPriorSegment(t *testing.T) {
	t.Parallel()

	fakeInjector := &injectorStub{}
	app := &App{
		cfg: config.Config{
			HotkeyMode: "hold",
			Streaming:  config.StreamingConfig{Mode: "segment"},
		},
		overlay:  &overlayStub{},
		injector: fakeInjector,
	}
	state := &recordingState{
		results:           make(chan streamResult, 1),
		target:            injector.Target{WindowID: "42", WindowClass: "Gedit"},
		deliveredSegments: 1,
	}
	app.recording = state
	state.setLiveSegmentDelivery(true)

	err := app.handleStreamEvent(context.Background(), state, openai.StreamEvent{
		Type: openai.StreamEventFinal,
		Text: "second chunk",
	})
	if err != nil {
		t.Fatalf("handleStreamEvent: %v", err)
	}

	if len(fakeInjector.liveInserted) != 1 || fakeInjector.liveInserted[0] != " second chunk" {
		t.Fatalf("liveInserted = %v, want [ second chunk]", fakeInjector.liveInserted)
	}
}

func TestHandleStreamEventQueuesFinalTextAfterRelease(t *testing.T) {
	t.Parallel()

	fakeInjector := &injectorStub{}
	app := &App{
		cfg: config.Config{
			Streaming: config.StreamingConfig{Mode: "segment"},
		},
		overlay:  &overlayStub{},
		injector: fakeInjector,
	}
	state := &recordingState{
		results: make(chan streamResult, 1),
		target:  injector.Target{WindowID: "42", WindowClass: "Gedit"},
	}
	state.setLiveSegmentDelivery(false)

	err := app.handleStreamEvent(context.Background(), state, openai.StreamEvent{
		Type: openai.StreamEventFinal,
		Text: "tail text",
	})
	if err != nil {
		t.Fatalf("handleStreamEvent: %v", err)
	}

	if len(fakeInjector.inserted) != 0 {
		t.Fatalf("inserted = %v, want no live inserts", fakeInjector.inserted)
	}
	select {
	case result := <-state.results:
		if result.text != "tail text" {
			t.Fatalf("queued text = %q, want tail text", result.text)
		}
		if result.err != nil {
			t.Fatalf("queued err = %v, want nil", result.err)
		}
	default:
		t.Fatal("expected queued final result")
	}
}

func TestHandleStreamEventReturnsStreamError(t *testing.T) {
	t.Parallel()

	app := &App{overlay: &overlayStub{}}
	state := &recordingState{}
	want := errors.New("boom")

	err := app.handleStreamEvent(context.Background(), state, openai.StreamEvent{
		Type: openai.StreamEventError,
		Err:  want,
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestHandleUpIgnoresSyntheticReleaseDuringSegmentMode(t *testing.T) {
	t.Parallel()

	app := &App{
		cfg: config.Config{
			HotkeyMode: "hold",
			Streaming: config.StreamingConfig{
				Mode: "segment",
			},
		},
		overlay: &overlayStub{},
		recording: &recordingState{
			id:        1,
			startedAt: time.Now(),
		},
		lastLiveInsert: time.Now(),
	}

	app.handleUp(context.Background())

	if app.recording == nil {
		t.Fatal("recording stopped, wanted synthetic release to be ignored")
	}
	if app.transcribing {
		t.Fatal("transcribing started, wanted synthetic release to be ignored")
	}
}

func TestShouldIgnoreSyntheticUpExpires(t *testing.T) {
	t.Parallel()

	app := &App{
		cfg: config.Config{
			HotkeyMode: "hold",
			Streaming: config.StreamingConfig{
				Mode: "segment",
			},
		},
		recording: &recordingState{id: 1},
	}

	app.lastLiveInsert = time.Now().Add(-syntheticReleaseGuard - 10*time.Millisecond)

	if app.shouldIgnoreSyntheticUp() {
		t.Fatal("expected expired synthetic release guard")
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

func TestShouldIgnoreEmptySegmentCommitAfterDeliveredChunk(t *testing.T) {
	t.Parallel()

	app := &App{
		cfg: config.Config{
			Streaming: config.StreamingConfig{
				Mode: "segment",
			},
		},
	}
	state := &recordingState{
		stream: &openai.Stream{},
	}
	state.noteDeliveredSegment()

	err := errors.Join(openai.ErrInputAudioBufferCommitEmpty, errors.New("buffer too small"))
	if !app.shouldIgnoreEmptySegmentCommit(state, err) {
		t.Fatal("expected empty commit after delivered segment to be ignored")
	}
}

func TestRecordingStateNeedsFinalCommitAfterAudioAppend(t *testing.T) {
	t.Parallel()

	state := &recordingState{}
	if state.needsFinalCommit() {
		t.Fatal("expected new state to not need a final commit")
	}

	state.markAudioAppended()
	if !state.needsFinalCommit() {
		t.Fatal("expected appended audio to require a final commit")
	}

	state.clearPendingFinalCommit()
	if state.needsFinalCommit() {
		t.Fatal("expected completed segment to clear pending final commit")
	}
}

func TestHandleStreamEventFinalClearsPendingFinalCommit(t *testing.T) {
	t.Parallel()

	app := &App{
		cfg: config.Config{
			Streaming: config.StreamingConfig{Mode: "segment"},
		},
		overlay:  &overlayStub{},
		injector: &injectorStub{},
	}
	state := &recordingState{
		results: make(chan streamResult, 1),
		target:  injector.Target{WindowID: "42", WindowClass: "Gedit"},
	}
	state.markAudioAppended()
	state.setLiveSegmentDelivery(false)

	err := app.handleStreamEvent(context.Background(), state, openai.StreamEvent{
		Type: openai.StreamEventFinal,
		Text: "tail text",
	})
	if err != nil {
		t.Fatalf("handleStreamEvent: %v", err)
	}
	if state.needsFinalCommit() {
		t.Fatal("expected final event to clear pending final commit")
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
func (o *overlayStub) ShowListening(string) {}
func (o *overlayStub) AnimateChunk(text string) {
	o.animatedChunks = append(o.animatedChunks, text)
}
func (o *overlayStub) ShowTranscribing() {}
func (o *overlayStub) ShowSuccess(text string) {
	o.successText = text
}
func (o *overlayStub) ShowError(error)  {}
func (o *overlayStub) SetLevel(float64) {}
func (o *overlayStub) Hide() {
	o.hideCalls++
}
func (o *overlayStub) Close()             {}
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
