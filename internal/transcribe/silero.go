package transcribe

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

//go:embed silero/silero_vad.onnx
var sileroModelBytes []byte

// Silero VAD runs inference on a sliding window: 64 samples of
// context (the tail of the previous chunk) + 512 new samples = 576
// samples total at 16 kHz. The context buffer preserves temporal
// continuity across chunks; feeding only 512 samples with no context
// makes the model classify everything as not-speech. Confirmed
// against snakers4/silero-vad src/silero_vad/utils_vad.py.
const (
	sileroSampleRate      = 16000
	sileroWindowSamples   = 512
	sileroContextSamples  = 64
	sileroInputSamples    = sileroContextSamples + sileroWindowSamples // 576
	sileroWindowMs        = 32                                         // 512 / 16000 * 1000
	sileroStateSize       = 128
	sileroStateRank       = 2
	sileroSpeechThreshold = 0.5
)

// Input/output names from the Silero VAD model. Confirmed against
// snakers4/silero-vad master as of model revision in this tree.
var (
	sileroInputNames  = []string{"input", "state", "sr"}
	sileroOutputNames = []string{"output", "stateN"}
)

// Global ONNX runtime + session state. The runtime and compiled model
// graph are expensive to set up (~50-100 ms), so we do it once per
// process and share the session across dictation sessions. Each
// SileroVAD instance gets its own hidden-state tensor so their
// inferences don't interfere.
var (
	sileroInitOnce sync.Once
	sileroInitErr  error
	sileroSession  *ort.DynamicAdvancedSession
)

// initSilero loads the ONNX Runtime shared library and compiles the
// Silero VAD graph from the embedded model bytes. Safe to call
// repeatedly — the first call does the work, subsequent calls return
// the cached result. If libraryPath is empty, tries a list of common
// install locations so users don't need to configure a path when
// they've followed a standard install procedure.
func initSilero(libraryPath string) error {
	sileroInitOnce.Do(func() {
		if !ort.IsInitialized() {
			resolved, err := resolveOnnxruntimeLibrary(libraryPath)
			if err != nil {
				sileroInitErr = err
				return
			}
			ort.SetSharedLibraryPath(resolved)
			if err := ort.InitializeEnvironment(); err != nil {
				sileroInitErr = fmt.Errorf("onnxruntime init (%s): %w", resolved, err)
				return
			}
		}
		if len(sileroModelBytes) == 0 {
			sileroInitErr = errors.New("silero: embedded model bytes are empty")
			return
		}
		sess, err := ort.NewDynamicAdvancedSessionWithONNXData(
			sileroModelBytes,
			sileroInputNames,
			sileroOutputNames,
			nil,
		)
		if err != nil {
			sileroInitErr = fmt.Errorf("silero session: %w", err)
			return
		}
		sileroSession = sess
	})
	return sileroInitErr
}

// resolveOnnxruntimeLibrary picks a path to libonnxruntime.so. If the
// user configured one, that wins. Otherwise we probe a list of common
// install locations — system-wide first, then user-local — and return
// the first that exists. Returns a descriptive error when nothing
// works so the caller can surface it (and we can gracefully fall back
// to the RMS VAD).
func resolveOnnxruntimeLibrary(configured string) (string, error) {
	if configured != "" {
		if _, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf("onnxruntime library %q: %w", configured, err)
		}
		return configured, nil
	}
	candidates := []string{
		"/usr/local/lib/libonnxruntime.so",
		"/usr/lib/libonnxruntime.so",
		"/usr/lib/x86_64-linux-gnu/libonnxruntime.so",
		os.ExpandEnv("$HOME/opt/onnxruntime/lib/libonnxruntime.so"),
		os.ExpandEnv("$HOME/.local/lib/libonnxruntime.so"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"libonnxruntime.so not found (tried %v); install from https://github.com/microsoft/onnxruntime/releases or set streaming.onnxruntime_library",
		candidates,
	)
}

// SileroVAD wraps the shared Silero session with per-instance hidden
// state and a ring buffer that collects incoming recorder chunks until
// a full 512-sample window is ready for inference. The hysteresis state
// machine (minSpeechMs / minSilenceMs / minUtteranceMs) is the same as
// ClientVAD — Silero just replaces the "is this frame speech?"
// decision with a neural-net probability check instead of an RMS
// comparison.
type SileroVAD struct {
	sampleRate     int
	minSilenceMs   int
	minSpeechMs    int
	minUtteranceMs int

	// Persistent tensors. stateIn is fed to the model; stateOut
	// receives the updated hidden state, which we copy back into
	// stateIn after each run. audioIn is refilled per-inference from
	// buf; probOut captures the speech probability.
	stateIn, stateOut *ort.Tensor[float32]
	audioIn           *ort.Tensor[float32]
	probOut           *ort.Tensor[float32]
	sr                *ort.Scalar[int64] // Silero wants sr as a 0-dim scalar tensor

	// buf holds int16 samples that haven't filled a full 512-window
	// yet. Capacity grows to accommodate the largest recorder chunk.
	buf []int16

	// ctx is the 64-sample context Silero wants prepended to each
	// inference input. Seeded with zeros; updated every run to the
	// last 64 normalized samples of the most recent window.
	ctx [sileroContextSamples]float32

	// Hysteresis state — mirror of ClientVAD's fields.
	inSpeech  bool
	silenceMs int
	speechMs  int

	// Snapshot diagnostics.
	lastProb float64
	minProb  float64
	maxProb  float64
}

// NewSileroVAD builds a Silero-backed detector. The caller is expected
// to have already verified the library + model paths exist and called
// initSilero once. minSilenceMs / minSpeechMs / minUtteranceMs work
// the same way they do in ClientVAD.
func NewSileroVAD(minSilenceMs, minSpeechMs, minUtteranceMs int) (*SileroVAD, error) {
	if sileroSession == nil {
		return nil, errors.New("silero not initialized: call initSilero first")
	}

	stateIn, err := ort.NewTensor(ort.NewShape(sileroStateRank, 1, sileroStateSize), make([]float32, sileroStateRank*sileroStateSize))
	if err != nil {
		return nil, fmt.Errorf("alloc stateIn: %w", err)
	}
	stateOut, err := ort.NewTensor(ort.NewShape(sileroStateRank, 1, sileroStateSize), make([]float32, sileroStateRank*sileroStateSize))
	if err != nil {
		stateIn.Destroy()
		return nil, fmt.Errorf("alloc stateOut: %w", err)
	}
	audioIn, err := ort.NewTensor(ort.NewShape(1, sileroInputSamples), make([]float32, sileroInputSamples))
	if err != nil {
		stateIn.Destroy()
		stateOut.Destroy()
		return nil, fmt.Errorf("alloc audioIn: %w", err)
	}
	probOut, err := ort.NewTensor(ort.NewShape(1, 1), make([]float32, 1))
	if err != nil {
		stateIn.Destroy()
		stateOut.Destroy()
		audioIn.Destroy()
		return nil, fmt.Errorf("alloc probOut: %w", err)
	}
	// Silero's `sr` input is a 0-dim scalar tensor. Use the binding's
	// dedicated scalar constructor — passing a 1D tensor silently made
	// the model output near-zero probability on every window.
	sr, err := ort.NewScalar[int64](sileroSampleRate)
	if err != nil {
		stateIn.Destroy()
		stateOut.Destroy()
		audioIn.Destroy()
		probOut.Destroy()
		return nil, fmt.Errorf("alloc sr: %w", err)
	}

	return &SileroVAD{
		sampleRate:     sileroSampleRate,
		minSilenceMs:   minSilenceMs,
		minSpeechMs:    minSpeechMs,
		minUtteranceMs: minUtteranceMs,
		stateIn:        stateIn,
		stateOut:       stateOut,
		audioIn:        audioIn,
		probOut:        probOut,
		sr:             sr,
		minProb:        1.0,
	}, nil
}

// Destroy releases per-instance tensor allocations. The shared session
// stays alive.
func (v *SileroVAD) Destroy() {
	if v.stateIn != nil {
		v.stateIn.Destroy()
	}
	if v.stateOut != nil {
		v.stateOut.Destroy()
	}
	if v.audioIn != nil {
		v.audioIn.Destroy()
	}
	if v.probOut != nil {
		v.probOut.Destroy()
	}
	if v.sr != nil {
		v.sr.Destroy()
	}
}

// Feed buffers incoming samples and runs Silero on every filled
// 512-sample window. If multiple transitions happen within one Feed
// call (unlikely, but possible on long chunks), the returned event
// is the most recent — the pump only reacts to SpeechStopped, so
// coalescing is safe.
func (v *SileroVAD) Feed(samples []int16) VADEvent {
	if len(samples) == 0 {
		return VADNone
	}
	v.buf = append(v.buf, samples...)

	last := VADNone
	for len(v.buf) >= sileroWindowSamples {
		window := v.buf[:sileroWindowSamples]
		v.buf = v.buf[sileroWindowSamples:]

		// Build the 576-sample inference input: 64 samples of
		// context from the tail of the previous window, then 512
		// fresh normalized samples. Context preserves temporal
		// continuity across chunks — without it Silero sees each
		// chunk as standalone and classifies everything as
		// not-speech. Per-window mean is subtracted from the new
		// samples to strip any DC offset from the mic.
		audio := v.audioIn.GetData()
		copy(audio[:sileroContextSamples], v.ctx[:])

		var meanSum float64
		for _, s := range window {
			meanSum += float64(s)
		}
		mean := float32(meanSum / float64(len(window)))
		for i, s := range window {
			audio[sileroContextSamples+i] = (float32(s) - mean) / 32768.0
		}

		// Save the last 64 samples of this window (already
		// normalized) as context for the next call.
		copy(v.ctx[:], audio[sileroContextSamples+sileroWindowSamples-sileroContextSamples:])

		if err := sileroSession.Run(
			[]ort.Value{v.audioIn, v.stateIn, v.sr},
			[]ort.Value{v.probOut, v.stateOut},
		); err != nil {
			// Don't fail the entire dictation on an inference error —
			// just skip this window and keep going. Caller will see
			// VADNone and eventually commit on hotkey release.
			continue
		}

		copy(v.stateIn.GetData(), v.stateOut.GetData())

		prob := float64(v.probOut.GetData()[0])
		v.lastProb = prob
		if prob < v.minProb {
			v.minProb = prob
		}
		if prob > v.maxProb {
			v.maxProb = prob
		}

		if evt := v.applyHysteresis(prob); evt != VADNone {
			last = evt
		}
	}
	return last
}

// applyHysteresis converts per-frame speech probability into VAD
// events using the same state machine as ClientVAD. Kept here rather
// than factored out because the two detectors have slightly different
// diagnostic fields and copy-pasting 20 lines is cheaper than forcing
// a shared helper through an interface.
func (v *SileroVAD) applyHysteresis(prob float64) VADEvent {
	if prob >= sileroSpeechThreshold {
		v.speechMs += sileroWindowMs
		v.silenceMs = 0
		if !v.inSpeech && v.speechMs >= v.minSpeechMs {
			v.inSpeech = true
			return VADSpeechStarted
		}
		return VADNone
	}
	v.silenceMs += sileroWindowMs
	if v.inSpeech && v.silenceMs >= v.minSilenceMs {
		episodeMs := v.speechMs
		v.inSpeech = false
		v.speechMs = 0
		if episodeMs < v.minUtteranceMs {
			return VADNone
		}
		return VADSpeechStopped
	}
	if !v.inSpeech && v.silenceMs > 200 {
		v.speechMs = 0
	}
	return VADNone
}

func (v *SileroVAD) InSpeech() bool { return v.inSpeech }

func (v *SileroVAD) Reset() {
	v.inSpeech = false
	v.silenceMs = 0
	v.speechMs = 0
	v.buf = v.buf[:0]
	v.ctx = [sileroContextSamples]float32{}
	// Keep the learned hidden state — it represents the mic's
	// speech/non-speech acoustic context and doesn't need to be
	// cleared across commits.
}

func (v *SileroVAD) Snapshot() VADSnapshot {
	return VADSnapshot{
		InSpeech:     v.inSpeech,
		LastRMS:      v.lastProb, // overloaded: probability, not RMS
		EffectiveThr: sileroSpeechThreshold,
		MinRMS:       v.minProb,
		MaxRMS:       v.maxProb,
		SilenceMs:    v.silenceMs,
		SpeechMs:     v.speechMs,
	}
}

var _ VAD = (*SileroVAD)(nil)
