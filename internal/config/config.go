package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const fileName = "config.json"

type Config struct {
	Hotkey     string          `json:"hotkey"`
	HotkeyMode string          `json:"hotkey_mode"`
	OpenAI     OpenAIConfig    `json:"openai"`
	Recording  RecordingConfig `json:"recording"`
	Insertion  InsertionConfig `json:"insertion"`
	Overlay    OverlayConfig   `json:"overlay"`
}

type OpenAIConfig struct {
	BaseURL      string   `json:"base_url"`
	Model        string   `json:"model"`
	Organization string   `json:"organization"`
	Project      string   `json:"project"`
	Language     string   `json:"language"`
	PromptHint   string   `json:"prompt_hint"`
	Vocabulary   []string `json:"vocabulary"`
	RequestLimit int      `json:"request_timeout_seconds"`
}

type RecordingConfig struct {
	Backend            string `json:"backend"`
	Device             string `json:"device"`
	SampleRate         int    `json:"sample_rate"`
	Channels           int    `json:"channels"`
	MaxDurationSeconds int    `json:"max_duration_seconds"`
}

type InsertionConfig struct {
	Mode             string   `json:"mode"`
	DefaultPasteKey  string   `json:"default_paste_key"`
	TerminalPasteKey string   `json:"terminal_paste_key"`
	TypeDelayMS      int      `json:"type_delay_ms"`
	RestoreClipboard bool     `json:"restore_clipboard"`
	TerminalClasses  []string `json:"terminal_classes"`
}

type OverlayConfig struct {
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	MarginTop      int     `json:"margin_top"`
	Opacity        float64 `json:"opacity"`
	AutoHideMillis int     `json:"auto_hide_millis"`
}

func Default() Config {
	return Config{
		Hotkey:     "ctrl+shift+space",
		HotkeyMode: "hold",
		OpenAI: OpenAIConfig{
			BaseURL:      "https://api.openai.com/v1",
			Model:        "gpt-4o-mini-transcribe",
			RequestLimit: 45,
			Vocabulary: []string{
				"OpenAI",
				"GPT",
				"VTT",
			},
		},
		Recording: RecordingConfig{
			Backend:            "auto",
			Device:             "default",
			SampleRate:         16000,
			Channels:           1,
			MaxDurationSeconds: 120,
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
		},
	}
}

func Path() (string, error) {
	if env := strings.TrimSpace(os.Getenv("VTT_CONFIG")); env != "" {
		return env, nil
	}

	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "vtt", fileName), nil
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

	bytes, err := os.ReadFile(path)
	if err != nil {
		return Config{}, "", err
	}

	cfg := Default()
	if err := json.Unmarshal(bytes, &cfg); err != nil {
		return Config{}, "", fmt.Errorf("decode %s: %w", path, err)
	}

	return cfg, path, cfg.Validate()
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	bytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	bytes = append(bytes, '\n')
	return os.WriteFile(path, bytes, 0o600)
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

	if c.OpenAI.RequestLimit <= 0 {
		return errors.New("openai.request_timeout_seconds must be greater than zero")
	}

	switch c.Insertion.Mode {
	case "auto", "clipboard", "type":
	default:
		return fmt.Errorf("insertion.mode must be auto, clipboard, or type")
	}

	if c.Recording.SampleRate <= 0 {
		return errors.New("recording.sample_rate must be greater than zero")
	}

	if c.Recording.Channels <= 0 {
		return errors.New("recording.channels must be greater than zero")
	}

	switch c.Recording.Backend {
	case "", "auto", "pulse":
	default:
		return fmt.Errorf("recording.backend must be auto or pulse")
	}

	if c.Overlay.Width < 200 || c.Overlay.Height < 80 {
		return errors.New("overlay dimensions are too small")
	}

	return nil
}
