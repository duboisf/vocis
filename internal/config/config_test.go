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

	data := []byte("hotkey: ctrl+shift+space\nopenai:\n  backend: lemonade\n")
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

	data := []byte("hotkey: ctrl+shift+space\ntranscription:\n  backend: lemonade\n")
	if err := rejectDeprecatedKeys("/tmp/example.yaml", data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestManualCommitAndPartialOverlayConflict pins the validation rule:
// manual-commit mode disables server-side interim transcripts on the
// Lemonade side, so show_partial_overlay has nothing to render. Fail
// fast rather than silently leaving the overlay blank.
func TestManualCommitAndPartialOverlayConflict(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Streaming.ManualCommit = true
	cfg.Streaming.ShowPartialOverlay = true

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for manual_commit + show_partial_overlay")
	}
	if !strings.Contains(err.Error(), "manual_commit") {
		t.Fatalf("error should mention manual_commit, got: %v", err)
	}
}

// TestManualCommitAloneIsValid confirms the new default pair is accepted.
func TestManualCommitAloneIsValid(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Streaming.ManualCommit = true
	cfg.Streaming.ShowPartialOverlay = false

	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestPartialOverlayWithoutManualCommitIsValid covers the VAD-streaming mode.
func TestPartialOverlayWithoutManualCommitIsValid(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Streaming.ManualCommit = false
	cfg.Streaming.ShowPartialOverlay = true

	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestClientVADRequiresManualCommit pins the rule that client-side pause
// detection only makes sense when server VAD is off — otherwise both
// would race to commit and we'd get duplicate turns.
func TestClientVADRequiresManualCommit(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Streaming.ManualCommit = false
	cfg.Streaming.ClientVAD = true

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error when client_vad is on without manual_commit")
	}
	if !strings.Contains(err.Error(), "client_vad") {
		t.Fatalf("error should mention client_vad, got: %v", err)
	}
}

func TestClientVADWithManualCommitIsValid(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Streaming.ManualCommit = true
	cfg.Streaming.ClientVAD = true
	cfg.Streaming.ShowPartialOverlay = false

	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
