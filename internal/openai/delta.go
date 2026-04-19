package openai

import "strings"

// deltaStrategyForModel picks a transcription.delta merge strategy based
// on the model name. Per OpenAI's realtime docs:
//
//   - whisper-1 (and any Whisper variant): delta = full turn transcript,
//     same as completed → replace.
//   - gpt-4o-transcribe / gpt-4o-mini-transcribe: delta = incremental new
//     text → append.
//
// Semantics follow the model, not the backend — a Whisper-family model
// emits cumulative deltas whether it's served by OpenAI, Lemonade, or
// anything else. Case-insensitive substring match on "whisper" catches
// OpenAI's whisper-1, Lemonade ids like whisper-v3-turbo-FLM, and any
// other vendor name that keeps the family label. Unknown or empty model
// names default to incremental (the modern OpenAI convention).
func deltaStrategyForModel(model string) func(existing, delta string) string {
	if strings.Contains(strings.ToLower(model), "whisper") {
		return mergeCumulativeDelta
	}
	return mergeIncrementalDelta
}

// mergeIncrementalDelta treats each transcription.delta as new text to
// append to the running partial.
func mergeIncrementalDelta(existing, delta string) string {
	return existing + delta
}

// mergeCumulativeDelta treats each transcription.delta as the full
// transcript so far — replace rather than append.
func mergeCumulativeDelta(_, delta string) string {
	return delta
}
