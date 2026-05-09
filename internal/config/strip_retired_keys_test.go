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
  backend: lemonade
`)
	out := stripRetiredKeys(in)
	if string(out) != string(in) {
		t.Fatalf("input should pass through unchanged when no retired keys present")
	}
}
