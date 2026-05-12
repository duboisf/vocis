package config

import (
	"strings"
	"testing"
)

func TestDefaultConfigIsValid(t *testing.T) {
	t.Parallel()

	if err := Default().Validate(); err != nil {
		t.Fatalf("validate default config: %v", err)
	}
}

func TestConfigRejectsInvalidHotkeyMode(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.HotkeyMode = "press"

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid hotkey mode to be rejected")
	}
}

func TestDefaultRecallPersist(t *testing.T) {
	t.Parallel()

	cfg := Default()
	if cfg.Recall.Persist.Mode != RecallPersistMemory {
		t.Fatalf("default persist mode should be %q, got %q",
			RecallPersistMemory, cfg.Recall.Persist.Mode)
	}
	if cfg.Recall.Persist.Dir == "" {
		t.Fatal("default persist dir should be non-empty (either XDG path or ~/.local/state/vocis/recall)")
	}
}

func TestRecallPersistValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mode    string
		dir     string
		wantErr bool
	}{
		{"default memory mode", RecallPersistMemory, "", false},
		{"memory mode with dir is fine", RecallPersistMemory, "/some/path", false},
		{"disk mode with dir is fine", RecallPersistDisk, "/some/path", false},
		{"disk mode without dir errors", RecallPersistDisk, "", true},
		{"unknown mode errors", "cloud", "/some/path", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Recall.Persist.Mode = tc.mode
			cfg.Recall.Persist.Dir = tc.dir
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestRejectDeprecatedOpenAIKey pins the strict migration error for
// config files that still use the old top-level `openai:` section.
func TestRejectDeprecatedOpenAIKey(t *testing.T) {
	t.Parallel()

	data := []byte("hotkey: ctrl+shift+space\nopenai:\n  backend: lemonade-chat\n")
	err := rejectDeprecatedKeys("/tmp/example.yaml", data)
	if err == nil {
		t.Fatal("expected error for deprecated openai: key")
	}
	if !strings.Contains(err.Error(), "transcription:") {
		t.Fatalf("error %q should mention the new key name", err)
	}
}

// TestRejectDeprecatedOpenAIKey_AcceptsTranscription confirms a config
// using the new `transcription:` key passes the deprecation check.
func TestRejectDeprecatedOpenAIKey_AcceptsTranscription(t *testing.T) {
	t.Parallel()

	data := []byte("hotkey: ctrl+shift+space\ntranscription:\n  backend: lemonade-chat\n")
	if err := rejectDeprecatedKeys("/tmp/example.yaml", data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDecodeStrictRejectsUnknownField pins the policy: any key in the user's
// config that doesn't map to a struct field must fail the load. This is
// how we prevent stale fields (e.g. a removed `timed_out:`) from silently
// hanging around after a rename — the user has to delete the stale key
// before vocis starts again.
func TestDecodeStrictRejectsUnknownField(t *testing.T) {
	t.Parallel()

	data := []byte("hotkey: ctrl+shift+space\noverlay:\n  finishing:\n    title: Finishing\n    timed_out: \"oops\"\n")
	cfg := Default()
	err := decodeStrict(data, &cfg)
	if err == nil {
		t.Fatal("expected decodeStrict to reject unknown field `timed_out`")
	}
	if !strings.Contains(err.Error(), "timed_out") {
		t.Fatalf("error should mention the offending field, got: %v", err)
	}
}

// TestDecodeStrictAcceptsKnownFields confirms a normal config still loads.
func TestDecodeStrictAcceptsKnownFields(t *testing.T) {
	t.Parallel()

	data := []byte("hotkey: ctrl+shift+space\noverlay:\n  finishing:\n    title: Finishing\n    wrapping_up: \"Wrapping up\"\n")
	cfg := Default()
	if err := decodeStrict(data, &cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Overlay.Finishing.Title != "Finishing" {
		t.Fatalf("expected title to be set, got %q", cfg.Overlay.Finishing.Title)
	}
}

// TestDecodeStrictEmptyInputIsOK confirms an empty config file (or one that
// only sets a stub) doesn't blow up — leaves defaults in place.
func TestDecodeStrictEmptyInputIsOK(t *testing.T) {
	t.Parallel()

	cfg := Default()
	if err := decodeStrict([]byte(""), &cfg); err != nil {
		t.Fatalf("empty input should be accepted, got: %v", err)
	}
	if cfg.Hotkey != Default().Hotkey {
		t.Fatalf("expected default hotkey to remain, got %q", cfg.Hotkey)
	}
}

// TestStripRetiredKeysDropsStreamingSection confirms that a config
// still carrying the old `streaming:` section loads cleanly — the
// section's contents (and the section itself) are silently stripped
// before strict decoding so users can upgrade without editing.
func TestStripRetiredKeysDropsStreamingSection(t *testing.T) {
	t.Parallel()

	in := []byte(`hotkey: ctrl+shift+space
transcription:
  backend: lemonade-chat
  model: gemma4-it-e2b-FLM
streaming:
  manual_commit: true
  silence_duration_ms: 800
  prefix_padding_ms: 300
  onnxruntime_library: /tmp/libonnxruntime.so
`)
	out := stripRetiredKeys(in)
	s := string(out)
	if strings.Contains(s, "streaming:") {
		t.Fatalf("streaming section still present after strip: %s", s)
	}
	if strings.Contains(s, "backend:") {
		t.Fatalf("retired transcription.backend still present: %s", s)
	}
	if !strings.Contains(s, "model: gemma4-it-e2b-FLM") {
		t.Fatalf("model key dropped: %s", s)
	}
}
