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

const DefaultPostProcessPrompt = "You are a dictation cleanup tool. The user's speech has been transcribed and you must clean it up. " +
	"ONLY remove filler words (um, uh, like, you know, I mean, sort of, kind of), false starts, and repetitions. " +
	"NEVER answer questions, add information, change meaning, or rephrase beyond minimal grammar fixes. " +
	"If the input is a question, return the question. If it is a statement, return the statement. " +
	"Return ONLY the cleaned transcription, nothing else."

const DefaultPromptHint = "Transcribe naturally for a programmer. " +
	"Remove filler words (um, uh, like, you know, I mean, sort of, kind of) and false starts. " +
	"Clean up hesitations into fluent sentences while preserving the speaker's intent and meaning. " +
	"Prefer technical terminology for software, CLI, cloud, and API concepts. " +
	"Preserve obvious technical terms, acronyms, and capitalization when the audio supports them."

type Config struct {
	Hotkey         string            `yaml:"hotkey"`
	HotkeyMode     string           `yaml:"hotkey_mode"`
	LogWindowTitle bool             `yaml:"log_window_title"`
	OpenAI         OpenAIConfig     `yaml:"openai"`
	Recording      RecordingConfig  `yaml:"recording"`
	Streaming      StreamingConfig  `yaml:"streaming"`
	Insertion      InsertionConfig  `yaml:"insertion"`
	Overlay        OverlayConfig    `yaml:"overlay"`
	PostProcess    PostProcessConfig `yaml:"postprocess"`
	Telemetry      TelemetryConfig  `yaml:"telemetry"`
	YAMLIndent     int              `yaml:"yaml_indent"`
}

type PostProcessConfig struct {
	Enabled      bool   `yaml:"enabled"`
	Model        string `yaml:"model"`
	Prompt       string `yaml:"prompt"`
	MinWordCount int    `yaml:"min_word_count"`
}

type TelemetryConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

type OpenAIConfig struct {
	BaseURL      string   `yaml:"base_url"`
	Model        string   `yaml:"model"`
	Organization string   `yaml:"organization"`
	Project      string   `yaml:"project"`
	Language     string   `yaml:"language"`
	PromptHint   string   `yaml:"prompt_hint"`
	Vocabulary   []string `yaml:"vocabulary"`
	RequestLimit int      `yaml:"request_timeout_seconds"`
}

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
	Width          int             `yaml:"width"`
	Height         int             `yaml:"height"`
	MarginTop      int             `yaml:"margin_top"`
	Opacity        float64         `yaml:"opacity"`
	AutoHideMillis int             `yaml:"auto_hide_millis"`
	Font           string          `yaml:"font"`
	FontSize       float64         `yaml:"font_size"`
	Branding       string          `yaml:"branding"`
	Ready          OverlayReady    `yaml:"ready"`
	Listening      OverlayListen   `yaml:"listening"`
	Finishing      OverlayFinish   `yaml:"finishing"`
	Success        OverlaySuccess  `yaml:"success"`
	Error          OverlayError    `yaml:"error"`
	Warning        OverlayWarning  `yaml:"warning"`
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
		OpenAI: OpenAIConfig{
			BaseURL:    "https://api.openai.com/v1",
			Model:      "gpt-4o-mini-transcribe",
			PromptHint: DefaultPromptHint,
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
			Enabled:      true,
			Model:        "gpt-4o-mini",
			Prompt:       DefaultPostProcessPrompt,
			MinWordCount: 10,
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

	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, "", fmt.Errorf("decode %s: %w", path, err)
	}

	return cfg, path, cfg.Validate()
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

	if strings.TrimSpace(c.OpenAI.Model) == "" {
		return errors.New("openai.model must not be empty")
	}

	switch c.HotkeyMode {
	case "hold", "toggle":
	default:
		return fmt.Errorf("hotkey_mode must be hold or toggle")
	}

	if c.OpenAI.RequestLimit <= 0 || c.OpenAI.RequestLimit > 300 {
		return errors.New("openai.request_timeout_seconds must be between 1 and 300")
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
