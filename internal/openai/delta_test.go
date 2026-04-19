package openai

import "testing"

// TestDeltaStrategyForModel pins the model → merge-strategy mapping.
// Whisper variants emit cumulative deltas (each event is the full
// transcript); gpt-4o-transcribe and gpt-4o-mini-transcribe emit
// incremental deltas. Unknown / empty names default to incremental.
func TestDeltaStrategyForModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		model    string
		existing string
		delta    string
		want     string
	}{
		{"whisper-1 replaces", "whisper-1", "Ok", "OK I", "OK I"},
		{"whisper-large-v3 replaces", "whisper-large-v3", "Ok", "OK I", "OK I"},
		{"whisper-v3-turbo-FLM replaces", "whisper-v3-turbo-FLM", "Ok", "OK I", "OK I"},
		{"Whisper mixed case replaces", "Whisper-1", "Ok", "OK I", "OK I"},
		{"gpt-4o-transcribe appends", "gpt-4o-transcribe", "Ok", " I", "Ok I"},
		{"gpt-4o-mini-transcribe appends", "gpt-4o-mini-transcribe", "Ok", " I", "Ok I"},
		{"non-whisper model appends", "parakeet-rnnt", "Ok", " I", "Ok I"},
		{"empty model defaults to append", "", "Ok", " I", "Ok I"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merge := deltaStrategyForModel(tc.model)
			if got := merge(tc.existing, tc.delta); got != tc.want {
				t.Fatalf("merge(%q,%q) = %q, want %q", tc.existing, tc.delta, got, tc.want)
			}
		})
	}
}
