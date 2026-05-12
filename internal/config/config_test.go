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

	data := []byte("hotkey: ctrl+shift+space\ntranscription:\n  base_url: x\n")
	if err := rejectDeprecatedKeys("/tmp/example.yaml", data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDecodeStrictRejectsUnknownField pins the policy: any key in the user's
// config that doesn't map to a struct field must fail the load. This is
// how we prevent stale fields from silently hanging around after a removal —
// the user has to delete the stale key before vocis starts again.
//
// The previous spelling used `overlay.finishing.timed_out`; with the
// overlay block gone, we now check a transcription-level typo instead.
func TestDecodeStrictRejectsUnknownField(t *testing.T) {
	t.Parallel()

	data := []byte("hotkey: ctrl+shift+space\ntranscription:\n  totally_made_up: 1\n")
	cfg := Default()
	err := decodeStrict(data, &cfg)
	if err == nil {
		t.Fatal("expected decodeStrict to reject unknown field `totally_made_up`")
	}
	if !strings.Contains(err.Error(), "totally_made_up") {
		t.Fatalf("error should mention the offending field, got: %v", err)
	}
}

// TestDecodeStrictAcceptsKnownFields confirms a normal config still loads.
func TestDecodeStrictAcceptsKnownFields(t *testing.T) {
	t.Parallel()

	data := []byte("hotkey: ctrl+shift+space\ntranscription:\n  model: gemma4-it-e2b-FLM\n")
	cfg := Default()
	if err := decodeStrict(data, &cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Transcription.Model != "gemma4-it-e2b-FLM" {
		t.Fatalf("expected model to be set, got %q", cfg.Transcription.Model)
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

// TestRejectDeprecatedKeysRejectsNestedChatAudio pins the migration
// error for configs that still nest knobs under
// `transcription.chat_audio:`. The fields were all hoisted up to
// `transcription:` directly; silently dropping the nested block would
// lose every user-set value, so the load path errors out instead.
func TestRejectDeprecatedKeysRejectsNestedChatAudio(t *testing.T) {
	t.Parallel()

	data := []byte("hotkey: ctrl+shift+space\ntranscription:\n  model: gemma4-it-e2b-FLM\n  chat_audio:\n    chunk_max_seconds: 28\n")
	err := rejectDeprecatedKeys("/tmp/example.yaml", data)
	if err == nil {
		t.Fatal("expected error for deprecated transcription.chat_audio block")
	}
	if !strings.Contains(err.Error(), "chat_audio") {
		t.Fatalf("error %q should mention chat_audio", err)
	}
	if !strings.Contains(err.Error(), "hoist") && !strings.Contains(err.Error(), "transcription:") {
		t.Fatalf("error %q should hint at the migration", err)
	}
}

// TestRejectDeprecatedKeysAllowsFlatTranscription confirms that a
// transcription block with the new flat shape (no chat_audio: child)
// passes the deprecation check cleanly.
func TestRejectDeprecatedKeysAllowsFlatTranscription(t *testing.T) {
	t.Parallel()

	data := []byte("hotkey: ctrl+shift+space\ntranscription:\n  model: gemma4-it-e2b-FLM\n  silero:\n    onnxruntime_library: /tmp/libonnxruntime.so\n")
	if err := rejectDeprecatedKeys("/tmp/example.yaml", data); err != nil {
		t.Fatalf("unexpected error: %v", err)
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

// TestStripRetiredKeysDropsOverlaySection confirms a config still
// carrying an `overlay:` block loads cleanly after the cull — every
// overlay knob is now pinned in internal/ui, so the YAML section is
// dropped wholesale rather than rejected.
func TestStripRetiredKeysDropsOverlaySection(t *testing.T) {
	t.Parallel()

	in := []byte(`hotkey: ctrl+shift+space
transcription:
  model: gemma4-it-e2b-FLM
overlay:
  width: 800
  height: 200
  ready:
    title: Custom
    subtitle: Custom Sub
`)
	out := stripRetiredKeys(in)
	s := string(out)
	if strings.Contains(s, "overlay:") {
		t.Fatalf("overlay section still present after strip: %s", s)
	}
	if !strings.Contains(s, "model: gemma4-it-e2b-FLM") {
		t.Fatalf("model key dropped: %s", s)
	}
}

// TestStripRetiredKeysDropsRecordingTuningKnobs confirms the
// recording.sample_rate/channels/etc. knobs are dropped (with a
// warn-log) rather than failing the strict decoder.
func TestStripRetiredKeysDropsRecordingTuningKnobs(t *testing.T) {
	t.Parallel()

	in := []byte(`hotkey: ctrl+shift+space
recording:
  device: default
  backend: pulse
  sample_rate: 16000
  channels: 1
  max_duration_seconds: 120
  duck_volume: 0.1
`)
	out := stripRetiredKeys(in)
	s := string(out)
	if !strings.Contains(s, "device: default") {
		t.Fatalf("device key dropped: %s", s)
	}
	for _, k := range []string{"backend:", "sample_rate:", "channels:", "max_duration_seconds:", "duck_volume:"} {
		if strings.Contains(s, k) {
			t.Fatalf("retired key %q still present: %s", k, s)
		}
	}
}
