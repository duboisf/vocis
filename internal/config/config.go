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
	YAMLIndent     int                 `yaml:"yaml_indent"`
}

type PostProcessConfig struct {
	Enabled              bool   `yaml:"enabled"`
	Model                string `yaml:"model"`
	Prompt               string `yaml:"prompt"`
	MinWordCount         int    `yaml:"min_word_count"`
	FirstTokenTimeoutSec int    `yaml:"first_token_timeout_seconds"`
	TotalTimeoutSec      int    `yaml:"total_timeout_seconds"`
}

type TelemetryConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

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
			ShowPartialOverlay: true,
			PrefixPaddingMS:    300,
			SilenceDurationMS:  500,
			Threshold:          0.5,
			ManualCommit:       false,
			MinUtteranceMS:     1000,
			WaitFinalSeconds:   3,
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
		},
		Telemetry: TelemetryConfig{
			Enabled:  false,
			Endpoint: "localhost:4317",
		},
		YAMLIndent: 2,
	}
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

	c.validateOverlayTemplates()

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
