package config

import (
	"strings"
	"testing"
)

func TestStripRetiredKeysRemovesOrgAndProject(t *testing.T) {
	in := []byte(`hotkey: ctrl+shift+space
transcription:
  backend: lemonade-chat
  model: gemma4-it-e2b-FLM
  organization: ""
  project: ""
  prompt_hint: keep this
`)
	out := stripRetiredKeys(in)
	s := string(out)
	if strings.Contains(s, "organization:") {
		t.Fatalf("organization key still present: %s", s)
	}
	if strings.Contains(s, "project:") {
		t.Fatalf("project key still present: %s", s)
	}
	if !strings.Contains(s, "prompt_hint: keep this") {
		t.Fatalf("non-retired key was dropped: %s", s)
	}
	if !strings.Contains(s, "model: gemma4-it-e2b-FLM") {
		t.Fatalf("model key dropped: %s", s)
	}
}

func TestStripRetiredKeysIsNoopWhenAbsent(t *testing.T) {
	in := []byte(`hotkey: a
transcription:
  model: gemma4-it-e2b-FLM
`)
	out := stripRetiredKeys(in)
	if string(out) != string(in) {
		t.Fatalf("input should pass through unchanged when no retired keys present")
	}
}

// TestStripRetiredKeysDropsPostprocessAndRebatch guards the loop
// simplification: a config still carrying the removed postprocess
// section or the rebatch knobs must keep loading under the strict
// decoder instead of failing on unknown fields.
func TestStripRetiredKeysDropsPostprocessAndRebatch(t *testing.T) {
	in := []byte(`transcription:
  model: gemma4-it-e2b-FLM
  continuation_rebatch: true
  batch_until_release: false
  rebatch_max_seconds: 28
postprocess:
  enabled: true
  combine: false
  model: gemma4-it-e2b-FLM
  prompt: clean it
`)
	var cfg Config
	if err := decodeStrict(stripRetiredKeys(in), &cfg); err != nil {
		t.Fatalf("strict decode after strip: %v", err)
	}
	if cfg.Transcription.Model != "gemma4-it-e2b-FLM" {
		t.Fatalf("model dropped: %+v", cfg.Transcription)
	}
	s := string(stripRetiredKeys(in))
	for _, gone := range []string{"postprocess:", "continuation_rebatch", "batch_until_release", "rebatch_max_seconds"} {
		if strings.Contains(s, gone) {
			t.Fatalf("%s still present: %s", gone, s)
		}
	}
}
