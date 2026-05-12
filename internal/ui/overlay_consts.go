package ui

// Overlay layout, font, and copy constants. These used to be the
// entire `overlay:` block in config.yaml — every value was a default
// the user never changed. Pinning them here keeps the YAML surface
// small and lets the renderer/X11 backend reach them by const name.
//
// Templates that take runtime values are exposed as raw strings with
// {placeholder} tokens; ExpandTemplate (in internal/config) does the
// substitution at render time.
const (
	OverlayWidth          = 620
	OverlayHeight         = 132
	OverlayMarginTop      = 44
	OverlayOpacity        = 0.94
	OverlayAutoHideMillis = 1800

	// OverlayFont is the fc-match query string. Empty maps to
	// "monospace" inside loadFont. A build-time override could change
	// this if a future user really wanted a different face.
	OverlayFont     = ""
	OverlayFontSize = 13.0

	OverlayBranding = "Vocis"

	OverlayReadyTitle    = "Ready"
	OverlayReadySubtitle = "Voice typing is armed"

	OverlayListeningTitle        = "Listening"
	OverlayListeningSuffix       = "— release to paste"
	OverlayListeningSubmitHint   = "⏎ submit"
	OverlayListeningConnecting   = "○ Connecting..."
	OverlayListeningReconnecting = "○ Reconnecting... (attempt {attempt}/{max})"
	OverlayListeningConnected    = "● Ready to type into {window}"
	// OverlayListeningLoadingModel shows while vocis is forcing a
	// local transcription model into memory at session-start.
	// {model} expands to the configured transcribe model name.
	OverlayListeningLoadingModel = "○ Loading {model}..."

	OverlayFinishingTitle      = "Finishing"
	OverlayFinishingCancelHint = "— press {shortcut} to cancel"
	OverlayFinishingWrappingUp = "Wrapping up"
	// OverlayFinishingPPWait / OverlayFinishingPPStream label the
	// postprocess sub-phases inside the Wrapping-up countdown.
	OverlayFinishingPPWait    = "Wait"
	OverlayFinishingPPStream  = "Stream"
	OverlayFinishingPhaseDone = "done"

	OverlaySuccessTitle    = "Typed"
	OverlaySuccessSubtitle = "Transcription inserted into your active app"

	OverlayErrorTitle = "Error"

	OverlayWarningTitle              = "Heads up"
	OverlayWarningNoSpeech           = "No speech detected"
	OverlayWarningCancelled          = "Cancelled — transcription discarded"
	OverlayWarningPostprocessSkipped = "Raw text pasted — cleanup was skipped due to a timeout or error"
	OverlayWarningTargetGone         = "Target window closed — transcript copied to clipboard"
)
