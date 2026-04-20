package transcribe

import (
	"fmt"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
)

// buildVAD constructs the client-side VAD chosen by streaming.VADBackend.
// "" and "rms" produce an energy-threshold detector; "silero" produces
// the neural detector (which lazily initializes the ONNX runtime on
// first use). Returns an error the caller can surface to the user —
// falling back silently would hide misconfigurations.
func buildVAD(streaming config.StreamingConfig, sampleRate int) (VAD, error) {
	switch streaming.VADBackend {
	case "", "rms":
		return buildRMSVAD(streaming, sampleRate), nil

	case "silero":
		if err := initSilero(streaming.OnnxruntimeLibrary); err != nil {
			// ONNX Runtime isn't available on this host. Fall back to
			// the RMS detector rather than failing — dictation still
			// works, just with a less robust pause detector.
			sessionlog.Warnf("silero VAD unavailable, falling back to rms: %v", err)
			return buildRMSVAD(streaming, sampleRate), nil
		}
		if sampleRate != sileroSampleRate {
			// Silero operates on 16 kHz internally; if the recorder
			// is at a different rate the VAD windowing math is off.
			// Don't silently run with bad timing.
			sessionlog.Warnf(
				"silero VAD expects 16 kHz but sampleRate=%d; falling back to rms",
				sampleRate,
			)
			return buildRMSVAD(streaming, sampleRate), nil
		}
		v, err := NewSileroVAD(
			streaming.SilenceDurationMS,
			streaming.PrefixPaddingMS,
			streaming.MinUtteranceMS,
		)
		if err != nil {
			sessionlog.Warnf("silero VAD construction failed, falling back to rms: %v", err)
			return buildRMSVAD(streaming, sampleRate), nil
		}
		sessionlog.Infof(
			"client VAD (silero): silence=%dms prefix=%dms min_utterance=%dms",
			streaming.SilenceDurationMS,
			streaming.PrefixPaddingMS,
			streaming.MinUtteranceMS,
		)
		return v, nil

	default:
		return nil, fmt.Errorf("unknown vad_backend %q", streaming.VADBackend)
	}
}

// buildRMSVAD constructs the energy-threshold detector. Extracted
// because the silero branch falls back to this when ONNX Runtime is
// unavailable.
func buildRMSVAD(streaming config.StreamingConfig, sampleRate int) VAD {
	v := NewClientVAD(
		sampleRate,
		streaming.Threshold,
		streaming.SilenceDurationMS,
		streaming.PrefixPaddingMS,
		streaming.MinUtteranceMS,
		0, // vadMargin: use default
	)
	sessionlog.Infof(
		"client VAD (rms): abs_threshold=%.3f silence=%dms prefix=%dms min_utterance=%dms",
		streaming.Threshold,
		streaming.SilenceDurationMS,
		streaming.PrefixPaddingMS,
		streaming.MinUtteranceMS,
	)
	return v
}
