package app

// NoopOverlay is an OverlayUI that does nothing. It is used as a fallback
// when the real (X11) overlay cannot be created — e.g. on a Wayland session
// with no XWayland server available. The overlay is purely visual feedback,
// so dictation still works end to end without it; degrading to a no-op keeps
// the app usable instead of making overlay init a fatal startup error.
type NoopOverlay struct{}

var _ OverlayUI = NoopOverlay{}

func (NoopOverlay) ShowHint(string)                 {}
func (NoopOverlay) ShowListening(string, string)    {}
func (NoopOverlay) SetConnected(string)             {}
func (NoopOverlay) SetConnecting(int, int)          {}
func (NoopOverlay) SetLoadingModel(string)          {}
func (NoopOverlay) SetSubmitMode(bool)              {}
func (NoopOverlay) SetListeningText(string, string) {}
func (NoopOverlay) AnimateChunk(string)             {}
func (NoopOverlay) ShowFinishing(string, string)    {}
func (NoopOverlay) SetFinishingPhase(string)        {}
func (NoopOverlay) ExtendFinishingPhase(string)     {}
func (NoopOverlay) SetFinishingText(string)         {}
func (NoopOverlay) ShowSuccess(string)              {}
func (NoopOverlay) ShowError(error)                 {}
func (NoopOverlay) ShowWarning(string)              {}

// GrabEscape returns a channel that never fires. The app selects on it to
// detect an Escape cancel; with no overlay/X connection there is nothing to
// grab, so the channel simply never delivers.
func (NoopOverlay) GrabEscape() <-chan struct{} { return make(chan struct{}) }

func (NoopOverlay) UngrabEscape()    {}
func (NoopOverlay) SetLevel(float64) {}
func (NoopOverlay) Hide()            {}
func (NoopOverlay) Close()           {}
