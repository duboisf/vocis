package app

import (
	"errors"
	"testing"
	"time"
)

// TestNoopOverlaySatisfiesInterface locks down that NoopOverlay can stand in
// for the real overlay when X11 is unavailable, and that none of its methods
// panic when driven through a representative session.
func TestNoopOverlaySatisfiesInterface(t *testing.T) {
	t.Parallel()

	var ov OverlayUI = NoopOverlay{}

	ov.ShowHint("hint")
	ov.ShowListening("Gedit", "push-to-talk")
	ov.SetConnecting(1, 3)
	ov.SetConnected("Gedit")
	ov.SetLoadingModel("whisper")
	ov.SetSubmitMode(true)
	ov.SetListeningText("Gedit", "hello world")
	ov.AnimateChunk("chunk")
	ov.ShowFinishing("body", "Esc")
	ov.SetFinishingPhase("Wrapping up")
	ov.ExtendFinishingPhase("Streaming")
	ov.SetFinishingText("text")
	ov.SetLevel(0.5)
	ov.ShowSuccess("done")
	ov.ShowWarning("careful")
	ov.ShowError(errors.New("boom"))
	ov.Hide()
	ov.UngrabEscape()
	ov.Close()
}

// TestNoopOverlayGrabEscapeNeverFires guards the cancel path: the app selects
// on GrabEscape()'s channel, so it must be non-nil and must not spuriously
// deliver (which would look like the user pressed Escape).
func TestNoopOverlayGrabEscapeNeverFires(t *testing.T) {
	t.Parallel()

	ch := NoopOverlay{}.GrabEscape()
	if ch == nil {
		t.Fatal("GrabEscape returned nil channel")
	}
	select {
	case <-ch:
		t.Fatal("GrabEscape channel fired without an Escape press")
	case <-time.After(20 * time.Millisecond):
	}
}
