package app

import (
	"errors"
	"testing"
)

// TestNoopOverlaySatisfiesInterface locks down that NoopOverlay can stand in
// for the real overlay when X11 is unavailable, and that none of its methods
// panic when driven through a representative session.
func TestNoopOverlaySatisfiesInterface(t *testing.T) {
	t.Parallel()

	var ov OverlayUI = NoopOverlay{}

	ov.ShowHint("hint")
	ov.ShowListening("Gedit", "push-to-talk")
	ov.SetConnected("Gedit")
	ov.SetLoadingModel("whisper")
	ov.SetSubmitMode(true)
	ov.SetListeningText("Gedit", "hello world")
	ov.ShowFinishing("body", "Esc")
	ov.SetFinishingText("text")
	ov.SetLevel(0.5)
	ov.ShowWarning("careful")
	ov.ShowError(errors.New("boom"))
	ov.Hide()
	ov.Close()
}
