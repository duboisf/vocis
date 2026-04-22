package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"vocis/internal/sessionlog"
)

const fileName = "config.yaml"

// DefaultPostProcessPrompt uses few-shot examples because small instruct
// models (1-2B class like gemma3-1b) without examples treat the user
// message as an instruction to *answer* instead of transcript to clean
// ("Did you update the configuration?" → "Cleaning configuration
// updated."). The examples anchor the "I clean text, I never respond"
// behavior. Pattern-matching on example shapes does happen occasionally
// (short imperatives getting replaced with example outputs) but it's a
// much smaller failure rate than rules-only, which fails on most
// question-shaped inputs.
const DefaultPostProcessPrompt = `You clean dictated speech transcripts. Output ONLY the cleaned text — never reply, never add commentary, never answer questions in the input.

Rules:
- Remove filler words (um, uh, like, you know, I mean, sort of, kind of), false starts, repetitions, and pauses (...).
- Lightly fix punctuation, capitalization, and spacing.
- Preserve the speaker's meaning, person, and intent EXACTLY. If they said "I", keep "I". If they asked a question, keep it as a question.
- Treat the input as transcript-to-clean, not as a message to respond to.

Examples:

Input: um so I think we should like, you know, refactor the auth module
Output: I think we should refactor the auth module.

Input: hey can you help me with this real quick
Output: Hey, can you help me with this real quick?

Input: I'm not going to do that. Please don't scroll. I'm just trying to become a big content creator one day. I have no supporters.
Output: I'm not going to do that. Please don't scroll. I'm just trying to become a big content creator one day. I have no supporters.

Input: what time is it
Output: What time is it?

Now clean the next input:`

const DefaultPromptHint = "Transcribe naturally for a programmer. " +
	"Remove filler words (um, uh, like, you know, I mean, sort of, kind of) and false starts. " +
	"Clean up hesitations into fluent sentences while preserving the speaker's intent and meaning. " +
	"Prefer technical terminology for software, CLI, cloud, and API concepts. " +
	"Preserve obvious technical terms, acronyms, and capitalization when the audio supports them."

type Config struct {
	Hotkey         string              `yaml:"hotkey"`
	HotkeyMode     string              `yaml:"hotkey_mode"`
	LogWindowTitle bool                `yaml:"log_window_title"`
	Transcription  TranscriptionConfig `yaml:"transcription"`
	Recording      RecordingConfig     `yaml:"recording"`
	Streaming      StreamingConfig     `yaml:"streaming"`
	Insertion      InsertionConfig     `yaml:"insertion"`
	Overlay        OverlayConfig       `yaml:"overlay"`
	PostProcess    PostProcessConfig   `yaml:"postprocess"`
	Telemetry      TelemetryConfig     `yaml:"telemetry"`
	Recall         RecallConfig        `yaml:"recall"`
	YAMLIndent     int                 `yaml:"yaml_indent"`
}

type PostProcessConfig struct {
	Enabled              bool   `yaml:"enabled"`
	Model                string `yaml:"model"`
	Prompt               string `yaml:"prompt"`
	MinWordCount         int    `yaml:"min_word_count"`
	FirstTokenTimeoutSec int    `yaml:"first_token_timeout_seconds"`
	TotalTimeoutSec      int    `yaml:"total_timeout_seconds"`
	// Sampling knobs. Pointers so nil = "leave unset; use backend
	// default". Zero is a meaningful value (e.g. temperature=0 is
	// greedy decoding). OpenAI-standard: Temperature, TopP,
	// FrequencyPenalty, PresencePenalty, Stop. Non-standard but accepted
	// by Lemonade/llama.cpp backends: MinP, RepetitionPenalty — these
	// are sent as extra JSON fields on the request body and ignored by
	// the OpenAI Cloud API.
	Temperature       *float64 `yaml:"temperature,omitempty"`
	TopP              *float64 `yaml:"top_p,omitempty"`
	MinP              *float64 `yaml:"min_p,omitempty"`
	FrequencyPenalty  *float64 `yaml:"frequency_penalty,omitempty"`
	PresencePenalty   *float64 `yaml:"presence_penalty,omitempty"`
	RepetitionPenalty *float64 `yaml:"repetition_penalty,omitempty"`
	Stop              []string `yaml:"stop,omitempty"`
}

type TelemetryConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

// RecallConfig drives the always-on `vocis recall` daemon. The daemon
// captures mic audio continuously, runs Silero VAD, and keeps a bounded
// ring buffer of speech segments that the user can later transcribe via
// `vocis recall pick`. See docs/overview.md for the user-facing shape.
type RecallConfig struct {
	// RetentionSeconds is how far back in time segments are kept. Older
	// segments are evicted from the ring buffer even if MaxSegments
	// isn't reached yet. 0 disables the time bound (count-only).
	RetentionSeconds int `yaml:"retention_seconds"`
	// MaxSegments caps the number of segments held in memory. Oldest is
	// evicted when a new segment is added past this count. 0 disables
	// the count bound (time-only).
	MaxSegments int `yaml:"max_segments"`
	// SocketPath is the Unix domain socket the daemon listens on and
	// the pick subcommand connects to. Empty = auto-resolve under
	// $XDG_RUNTIME_DIR/vocis/recall.sock (or /tmp fallback).
	SocketPath string `yaml:"socket_path"`
	// MinSilenceMS / MinSpeechMS / MinUtteranceMS mirror the Silero VAD
	// hysteresis knobs in internal/transcribe/silero.go. They control
	// when a speech episode starts, when it ends, and whether it's long
	// enough to keep.
	MinSilenceMS   int `yaml:"min_silence_ms"`
	MinSpeechMS    int `yaml:"min_speech_ms"`
	MinUtteranceMS int `yaml:"min_utterance_ms"`
	// PrerollMS is how much audio before the VAD speech-start is
	// included in the segment, so word onsets aren't clipped.
	PrerollMS int `yaml:"preroll_ms"`
	// MaxSegmentSeconds caps an individual segment's duration. A long
	// monologue without a pause gets flushed at this boundary so the
	// ring buffer can't grow unbounded from a single stream.
	MaxSegmentSeconds int `yaml:"max_segment_seconds"`
	// MinSegmentPeak is the minimum peak sample level (0-1, abs-max /
	// 32768) a finalized segment must have to be kept in the ring
	// buffer. Below this, the segment is treated as silence or noise
	// and dropped. Silero VAD can get stuck declaring in-speech on
	// low-level ambient noise (fans, keyboards) that briefly crosses
	// its probability threshold; without this filter those sessions
	// fill the ring with 30 s force-flushed noise segments. A value
	// around 0.02 rejects fan/room tone while keeping quiet speech.
	// Set to 0 to keep every segment (useful only for debugging).
	MinSegmentPeak float64 `yaml:"min_segment_peak"`
	// MinSegmentRMS is the minimum RMS (root mean square) sample level
	// a finalized segment must have to be kept. RMS discriminates
	// sustained energy from a silent segment that happens to contain
	// a single loud click, which peak alone can't: a 24 s silence
	// segment with one keyboard clack can easily have peak > 0.04
	// while its RMS stays below 0.005. A value around 0.005 rejects
	// near-silence while keeping genuinely quiet speech (speech RMS
	// is typically 0.01-0.05 even at soft volumes). Set to 0 to
	// disable the RMS filter and rely on peak alone.
	MinSegmentRMS float64 `yaml:"min_segment_rms"`
	// Persist controls whether captured segments are mirrored to disk.
	// Default is memory-only — always-on mic audio does not land on
	// disk unless the user explicitly opts in by setting mode=disk.
	Persist RecallPersistConfig `yaml:"persist"`
}

// RecallPersistConfig is the nested `recall.persist` block. Mode is
// the on/off switch; Dir is only consulted when Mode is "disk" but is
// pre-populated with a sensible default so the user can flip mode with
// a single-line config change.
type RecallPersistConfig struct {
	// Mode is "in_memory" (default) or "disk". In-memory keeps the
	// ring buffer entirely in the daemon process — nothing written,
	// restart loses the buffer. Disk mirrors each segment to Dir as
	// one JSON file per segment (raw PCM base64-encoded + metadata +
	// cached transcript); the ring is reloaded on daemon start, with
	// the current retention applied.
	Mode string `yaml:"mode"`
	// Dir is where segment JSON files live when Mode is "disk". The
	// directory is created with mode 0700 at daemon startup. Supports
	// a leading ~/ expansion. Defaults to $XDG_STATE_HOME/vocis/recall
	// when XDG_STATE_HOME is set, else ~/.local/state/vocis/recall.
	Dir string `yaml:"dir"`
}

const (
	RecallPersistMemory = "in_memory"
	RecallPersistDisk   = "disk"
)

type TranscriptionConfig struct {
	Backend      string   `yaml:"backend"`
	BaseURL      string   `yaml:"base_url"`
	RealtimeURL  string   `yaml:"realtime_url"`
	Model        string   `yaml:"model"`
	Organization string   `yaml:"organization"`
	Project      string   `yaml:"project"`
	Language     string   `yaml:"language"`
	PromptHint   string   `yaml:"prompt_hint"`
	Vocabulary   []string `yaml:"vocabulary"`
	RequestLimit int      `yaml:"request_timeout_seconds"`
}

const (
	BackendOpenAI   = "openai"
	BackendLemonade = "lemonade"
)

type RecordingConfig struct {
	Backend            string  `yaml:"backend"`
	Device             string  `yaml:"device"`
	SampleRate         int     `yaml:"sample_rate"`
	Channels           int     `yaml:"channels"`
	MaxDurationSeconds int     `yaml:"max_duration_seconds"`
	DuckVolume         float64 `yaml:"duck_volume"`
}

type StreamingConfig struct {
	ShowPartialOverlay bool    `yaml:"show_partial_overlay"`
	PrefixPaddingMS    int     `yaml:"prefix_padding_ms"`
	SilenceDurationMS  int     `yaml:"silence_duration_ms"`
	Threshold          float64 `yaml:"threshold"`
	// ManualCommit disables server-side VAD for Lemonade and relies on the
	// client-issued input_audio_buffer.commit at Finalize. Trades live partial
	// transcripts (and the overlay that shows them) for lower end-of-utterance
	// latency, since Lemonade stops running an interim transcription in
	// parallel with the final. OpenAI backend ignores this flag.
	ManualCommit bool `yaml:"manual_commit"`
	// ClientVAD runs an energy-threshold voice activity detector on the
	// client side and sends input_audio_buffer.commit whenever it detects
	// a pause (silence_duration_ms of low-energy audio). Pauses chunk a
	// long dictation into several transcribed segments without the user
	// releasing the hotkey, cutting the end-of-utterance latency tied to
	// Lemonade's post-commit Whisper pass. Requires ManualCommit=true
	// (server VAD must be off or the two detectors would race).
	ClientVAD bool `yaml:"client_vad"`
	// MinUtteranceMS is the minimum accumulated speech duration an
	// episode needs before a pause is allowed to trigger a commit.
	// Shorter episodes reset silently and roll into the next utterance
	// (or the final hotkey-release commit). Whisper transcribes
	// reliably on ~1s+ segments; shorter clips produce unstable output.
	// Only meaningful when ClientVAD is on.
	MinUtteranceMS int `yaml:"min_utterance_ms"`
	// OnnxruntimeLibrary is an optional absolute path to
	// libonnxruntime.so. When empty, vocis auto-discovers the library
	// from common install locations (/usr/local/lib, /usr/lib, etc.).
	// Only consulted when ClientVAD is on.
	OnnxruntimeLibrary string `yaml:"onnxruntime_library"`
	// WaitFinalSeconds is the minimum time to wait for the trailing transcript
	// after Finalize commits the audio buffer. Cloud backends answer in under
	// a second; local backends running Whisper on CPU commonly need 5-15s for
	// first-request model load + inference. Scaled timeout is max(this,
	// trailing_duration / 5).
	WaitFinalSeconds int `yaml:"wait_final_seconds"`
}

type InsertionConfig struct {
	Mode             string   `yaml:"mode"`
	DefaultPasteKey  string   `yaml:"default_paste_key"`
	TerminalPasteKey string   `yaml:"terminal_paste_key"`
	TypeDelayMS      int      `yaml:"type_delay_ms"`
	RestoreClipboard bool     `yaml:"restore_clipboard"`
	TerminalClasses  []string `yaml:"terminal_classes"`
}

type OverlayConfig struct {
	Width          int            `yaml:"width"`
	Height         int            `yaml:"height"`
	MarginTop      int            `yaml:"margin_top"`
	Opacity        float64        `yaml:"opacity"`
	AutoHideMillis int            `yaml:"auto_hide_millis"`
	Font           string         `yaml:"font"`
	FontSize       float64        `yaml:"font_size"`
	Branding       string         `yaml:"branding"`
	Ready          OverlayReady   `yaml:"ready"`
	Listening      OverlayListen  `yaml:"listening"`
	Finishing      OverlayFinish  `yaml:"finishing"`
	Success        OverlaySuccess `yaml:"success"`
	Error          OverlayError   `yaml:"error"`
	Warning        OverlayWarning `yaml:"warning"`
}

type OverlayReady struct {
	Title    string `yaml:"title"`
	Subtitle string `yaml:"subtitle"`
}

type OverlayListen struct {
	Title        string `yaml:"title"`
	Suffix       string `yaml:"suffix"`
	SubmitHint   string `yaml:"submit_hint"`
	Connecting   string `yaml:"connecting"`
	Reconnecting string `yaml:"reconnecting"`
	Connected    string `yaml:"connected"`
	// LoadingModel is the subtitle shown while vocis is forcing a
	// local transcription model into memory at session-start. Supports
	// {model} template expansion with the configured transcribe model
	// name. Only applies on the Lemonade backend — cloud backends
	// always have the model warm.
	LoadingModel string `yaml:"loading_model"`
}

type OverlayFinish struct {
	Title          string `yaml:"title"`
	CancelHint     string `yaml:"cancel_hint"`
	WrappingUp     string `yaml:"wrapping_up"`
	PostProcessing string `yaml:"post_processing"`
	PPWait         string `yaml:"pp_wait"`
	PPStream       string `yaml:"pp_stream"`
	TimedOut       string `yaml:"timed_out"`
	PhaseDone      string `yaml:"phase_done"`
}

type OverlaySuccess struct {
	Title    string `yaml:"title"`
	Subtitle string `yaml:"subtitle"`
}

type OverlayError struct {
	Title string `yaml:"title"`
}

type OverlayWarning struct {
	Title              string `yaml:"title"`
	NoSpeech           string `yaml:"no_speech"`
	Cancelled          string `yaml:"cancelled"`
	PostprocessSkipped string `yaml:"postprocess_skipped"`
}

func Default() Config {
	return Config{
		Hotkey:     "ctrl+shift+space",
		HotkeyMode: "hold",
		Transcription: TranscriptionConfig{
			Backend:      BackendOpenAI,
			BaseURL:      "https://api.openai.com/v1",
			Model:        "gpt-4o-mini-transcribe",
			PromptHint:   DefaultPromptHint,
			RequestLimit: 45,
			Vocabulary: []string{
				"OpenAI",
				"GPT",
				"Vocis",
			},
		},
		Recording: RecordingConfig{
			Backend:            "auto",
			Device:             "default",
			SampleRate:         16000,
			Channels:           1,
			MaxDurationSeconds: 120,
			DuckVolume:         0.1,
		},
		Streaming: StreamingConfig{
			// Defaults tuned for the Lemonade + Silero setup most users
			// run locally: server-side VAD off (ManualCommit), Silero
			// client VAD on for mid-hold chunking, partial overlay off
			// (manual-commit mode doesn't stream interim deltas). Cloud
			// OpenAI users who want live partials should flip
			// ManualCommit=false, ClientVAD=false, ShowPartialOverlay=true.
			ShowPartialOverlay: false,
			PrefixPaddingMS:    300,
			SilenceDurationMS:  500,
			// 0.02 is an energy-threshold tuned for Silero client VAD —
			// the previous 0.5 was appropriate for server-VAD prob-mass
			// thresholds, which we no longer use by default.
			Threshold:        0.02,
			ManualCommit:     true,
			ClientVAD:        true,
			MinUtteranceMS:   1000,
			WaitFinalSeconds: 15,
		},
		Insertion: InsertionConfig{
			Mode:             "auto",
			DefaultPasteKey:  "ctrl+v",
			TerminalPasteKey: "ctrl+shift+v",
			TypeDelayMS:      1,
			RestoreClipboard: true,
			TerminalClasses: []string{
				"Alacritty",
				"kitty",
				"org.wezfurlong.wezterm",
				"WezTerm",
				"XTerm",
				"Gnome-terminal",
				"gnome-terminal-server",
				"code",
				"Cursor",
			},
		},
		Overlay: OverlayConfig{
			Width:          620,
			Height:         132,
			MarginTop:      44,
			Opacity:        0.94,
			AutoHideMillis: 1800,
			Branding:       "Vocis",
			Ready: OverlayReady{
				Title:    "Ready",
				Subtitle: "Voice typing is armed",
			},
			Listening: OverlayListen{
				Title:        "Listening",
				Suffix:       "— release to paste",
				SubmitHint:   "⏎ submit",
				Connecting:   "○ Connecting...",
				Reconnecting: "○ Reconnecting... (attempt {attempt}/{max})",
				Connected:    "● Ready to type into {window}",
				LoadingModel: "○ Loading {model}...",
			},
			Finishing: OverlayFinish{
				Title:          "Finishing",
				CancelHint:     "— press {shortcut} to cancel",
				WrappingUp:     "Wrapping up",
				PostProcessing: "Post-processing",
				PPWait:         "Wait",
				PPStream:       "Stream",
				TimedOut:       "{phase} — timed out",
				PhaseDone:      "done",
			},
			Success: OverlaySuccess{
				Title:    "Typed",
				Subtitle: "Transcription inserted into your active app",
			},
			Error: OverlayError{
				Title: "Error",
			},
			Warning: OverlayWarning{
				Title:              "Heads up",
				NoSpeech:           "No speech detected",
				Cancelled:          "Cancelled — transcription discarded",
				PostprocessSkipped: "Raw text pasted — cleanup was skipped due to a timeout or error",
			},
		},
		PostProcess: PostProcessConfig{
			Enabled:              true,
			Model:                "gpt-4o-mini",
			Prompt:               DefaultPostProcessPrompt,
			MinWordCount:         10,
			FirstTokenTimeoutSec: 10,
			TotalTimeoutSec:      15,
			Temperature:          floatPtr(0.2),
		},
		Telemetry: TelemetryConfig{
			Enabled:  false,
			Endpoint: "localhost:4317",
		},
		Recall: RecallConfig{
			// 7 days of retention is a sensible "long history" default for
			// an always-on recall. Segments are small (~100-400 KB each);
			// 7 days at typical speaking pace stays well under 1 GB even
			// with persist.mode=disk. Set to 0 for unbounded time.
			RetentionSeconds:  604800,
			// 2000 keeps disk under ~800 MB worst-case. Set to 0 for
			// unbounded count (only the time bound applies).
			MaxSegments:       2000,
			SocketPath:        "",
			MinSilenceMS:      500,
			MinSpeechMS:       150,
			MinUtteranceMS:    500,
			PrerollMS:         300,
			MaxSegmentSeconds: 30,
			MinSegmentPeak:    0.02,
			MinSegmentRMS:     0.005,
			Persist: RecallPersistConfig{
				Mode: RecallPersistMemory,
				Dir:  defaultRecallStateDir(),
			},
		},
		YAMLIndent: 2,
	}
}

func floatPtr(v float64) *float64 { return &v }

// defaultRecallStateDir returns the default for `recall.persist.dir`.
// Follows the XDG state-dir convention: $XDG_STATE_HOME/vocis/recall
// when set, else ~/.local/state/vocis/recall. Embedded into the config
// file at generation time so users can see exactly where segments
// would land if they flipped mode to disk.
func defaultRecallStateDir() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdg != "" {
		return filepath.Join(xdg, "vocis", "recall")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// os.UserHomeDir only fails when HOME is unset and the platform
		// has no fallback. Return the tilde form and let runtime
		// expansion either fix it or fail loudly.
		return "~/.local/state/vocis/recall"
	}
	return filepath.Join(home, ".local", "state", "vocis", "recall")
}

func Path() (string, error) {
	if env := strings.TrimSpace(os.Getenv("VOCIS_CONFIG")); env != "" {
		return env, nil
	}

	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "vocis", fileName), nil
}

func InitDefault() (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	return path, Save(path, Default())
}

func Load() (Config, string, error) {
	path, err := Path()
	if err != nil {
		return Config{}, "", err
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := Save(path, Default()); err != nil {
			return Config{}, "", err
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, "", err
	}

	if err := rejectDeprecatedKeys(path, data); err != nil {
		return Config{}, "", err
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, "", fmt.Errorf("decode %s: %w", path, err)
	}

	return cfg, path, cfg.Validate()
}

// rejectDeprecatedKeys fails loudly on config files that still use
// pre-rename top-level keys. Strict by design: silently accepting the
// old shape splits users across two spellings and hides the fact that
// the section no longer describes OpenAI specifically.
func rejectDeprecatedKeys(path string, data []byte) error {
	var raw map[string]yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		// Bad YAML will surface on the real unmarshal below with a
		// better error — don't preempt it here.
		return nil
	}
	if _, ok := raw["openai"]; ok {
		return fmt.Errorf(
			"%s: top-level key `openai:` was renamed to `transcription:`. "+
				"Rename the section in your config and try again.",
			path,
		)
	}
	return nil
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	indent := cfg.YAMLIndent
	if indent <= 0 {
		indent = 2
	}
	enc.SetIndent(indent)
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Hotkey) == "" {
		return errors.New("hotkey must not be empty")
	}

	if strings.TrimSpace(c.Transcription.Model) == "" {
		return errors.New("transcription.model must not be empty")
	}

	switch c.Transcription.Backend {
	case "", BackendOpenAI, BackendLemonade:
	default:
		return fmt.Errorf("transcription.backend must be %q or %q", BackendOpenAI, BackendLemonade)
	}

	switch c.HotkeyMode {
	case "hold", "toggle":
	default:
		return fmt.Errorf("hotkey_mode must be hold or toggle")
	}

	if c.Transcription.RequestLimit <= 0 || c.Transcription.RequestLimit > 300 {
		return errors.New("transcription.request_timeout_seconds must be between 1 and 300")
	}

	switch c.Insertion.Mode {
	case "auto", "clipboard", "type":
	default:
		return fmt.Errorf("insertion.mode must be auto, clipboard, or type")
	}

	if c.Insertion.TypeDelayMS < 0 || c.Insertion.TypeDelayMS > 1000 {
		return errors.New("insertion.type_delay_ms must be between 0 and 1000")
	}

	if c.Recording.SampleRate <= 0 || c.Recording.SampleRate > 48000 {
		return errors.New("recording.sample_rate must be between 1 and 48000")
	}

	if c.Recording.Channels <= 0 || c.Recording.Channels > 2 {
		return errors.New("recording.channels must be 1 or 2")
	}

	if c.Recording.MaxDurationSeconds < 0 || c.Recording.MaxDurationSeconds > 600 {
		return errors.New("recording.max_duration_seconds must be between 0 and 600")
	}

	switch c.Recording.Backend {
	case "", "auto", "pulse":
	default:
		return fmt.Errorf("recording.backend must be auto or pulse")
	}

	if c.Streaming.PrefixPaddingMS < 0 || c.Streaming.PrefixPaddingMS > 2000 {
		return errors.New("streaming.prefix_padding_ms must be between 0 and 2000")
	}

	if c.Streaming.SilenceDurationMS < 0 || c.Streaming.SilenceDurationMS > 5000 {
		return errors.New("streaming.silence_duration_ms must be between 0 and 5000")
	}

	if c.Streaming.Threshold < 0 || c.Streaming.Threshold > 1 {
		return errors.New("streaming.threshold must be between 0 and 1")
	}

	if c.Streaming.WaitFinalSeconds < 1 || c.Streaming.WaitFinalSeconds > 60 {
		return errors.New("streaming.wait_final_seconds must be between 1 and 60")
	}

	if c.Streaming.MinUtteranceMS < 0 || c.Streaming.MinUtteranceMS > 10000 {
		return errors.New("streaming.min_utterance_ms must be between 0 and 10000")
	}

	if c.Streaming.ManualCommit && c.Streaming.ShowPartialOverlay {
		return errors.New("streaming.show_partial_overlay requires streaming.manual_commit=false (manual-commit mode disables server-side interim transcripts)")
	}

	if c.Streaming.ClientVAD && !c.Streaming.ManualCommit {
		return errors.New("streaming.client_vad requires streaming.manual_commit=true (otherwise server VAD and client VAD race to commit)")
	}

	if c.Overlay.Width < 200 || c.Overlay.Height < 80 {
		return errors.New("overlay dimensions are too small")
	}

	// 30-day ceiling on retention: keeps the validation bound sane
	// while leaving headroom for "long history" setups. At typical
	// conversational pace persisted to disk, 30 days easily exceeds
	// the max_segments ceiling below — the count bound kicks in first.
	if c.Recall.RetentionSeconds < 0 || c.Recall.RetentionSeconds > 30*86400 {
		return errors.New("recall.retention_seconds must be between 0 and 2592000 (30 days)")
	}
	if c.Recall.MaxSegments < 0 || c.Recall.MaxSegments > 10000 {
		return errors.New("recall.max_segments must be between 0 and 10000")
	}
	if c.Recall.RetentionSeconds == 0 && c.Recall.MaxSegments == 0 {
		return errors.New("recall: at least one of retention_seconds or max_segments must be > 0")
	}
	if c.Recall.MinSilenceMS < 0 || c.Recall.MinSilenceMS > 5000 {
		return errors.New("recall.min_silence_ms must be between 0 and 5000")
	}
	if c.Recall.MinSpeechMS < 0 || c.Recall.MinSpeechMS > 5000 {
		return errors.New("recall.min_speech_ms must be between 0 and 5000")
	}
	if c.Recall.MinUtteranceMS < 0 || c.Recall.MinUtteranceMS > 10000 {
		return errors.New("recall.min_utterance_ms must be between 0 and 10000")
	}
	if c.Recall.PrerollMS < 0 || c.Recall.PrerollMS > 5000 {
		return errors.New("recall.preroll_ms must be between 0 and 5000")
	}
	if c.Recall.MaxSegmentSeconds < 1 || c.Recall.MaxSegmentSeconds > 300 {
		return errors.New("recall.max_segment_seconds must be between 1 and 300")
	}
	if c.Recall.MinSegmentPeak < 0 || c.Recall.MinSegmentPeak > 1 {
		return errors.New("recall.min_segment_peak must be between 0 and 1")
	}
	if c.Recall.MinSegmentRMS < 0 || c.Recall.MinSegmentRMS > 1 {
		return errors.New("recall.min_segment_rms must be between 0 and 1")
	}
	switch c.Recall.Persist.Mode {
	case "", RecallPersistMemory, RecallPersistDisk:
	default:
		return fmt.Errorf("recall.persist.mode must be %q or %q", RecallPersistMemory, RecallPersistDisk)
	}
	if c.Recall.Persist.Mode == RecallPersistDisk && strings.TrimSpace(c.Recall.Persist.Dir) == "" {
		return errors.New("recall.persist.mode=disk requires recall.persist.dir to be set")
	}

	if err := c.PostProcess.validate(); err != nil {
		return err
	}

	c.validateOverlayTemplates()

	return nil
}

func (p PostProcessConfig) validate() error {
	if p.Temperature != nil && (*p.Temperature < 0 || *p.Temperature > 2) {
		return errors.New("postprocess.temperature must be between 0 and 2")
	}
	if p.TopP != nil && (*p.TopP <= 0 || *p.TopP > 1) {
		return errors.New("postprocess.top_p must be between 0 (exclusive) and 1")
	}
	if p.MinP != nil && (*p.MinP < 0 || *p.MinP > 1) {
		return errors.New("postprocess.min_p must be between 0 and 1")
	}
	if p.FrequencyPenalty != nil && (*p.FrequencyPenalty < -2 || *p.FrequencyPenalty > 2) {
		return errors.New("postprocess.frequency_penalty must be between -2 and 2")
	}
	if p.PresencePenalty != nil && (*p.PresencePenalty < -2 || *p.PresencePenalty > 2) {
		return errors.New("postprocess.presence_penalty must be between -2 and 2")
	}
	if p.RepetitionPenalty != nil && (*p.RepetitionPenalty <= 0 || *p.RepetitionPenalty > 2) {
		return errors.New("postprocess.repetition_penalty must be between 0 (exclusive) and 2")
	}
	return nil
}

func (c Config) validateOverlayTemplates() {
	templates := []struct {
		key      string
		template string
		expected []string
	}{
		{"overlay.listening.connected", c.Overlay.Listening.Connected, []string{"window"}},
		{"overlay.listening.reconnecting", c.Overlay.Listening.Reconnecting, []string{"attempt", "max"}},
		{"overlay.finishing.cancel_hint", c.Overlay.Finishing.CancelHint, []string{"shortcut"}},
		{"overlay.finishing.timed_out", c.Overlay.Finishing.TimedOut, []string{"phase"}},
	}
	for _, tt := range templates {
		for _, w := range ValidateTemplate(tt.template, tt.expected) {
			sessionlog.Warnf("config %s: %s", tt.key, w)
		}
	}
}
