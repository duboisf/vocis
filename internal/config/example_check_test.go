package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExampleYAMLMatchesDefaults parses config.example.yaml under the
// new flattened shape and verifies every chat-audio knob lands on the
// transcription struct at the same value Default() would produce.
// Regression check for the chat_audio.* flatten — silently dropping
// the nested block would leave the user with implicit defaults, which
// would mask example-yaml drift.
func TestExampleYAMLMatchesDefaults(t *testing.T) {
	t.Parallel()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	example := filepath.Join(root, "config.example.yaml")
	data, err := os.ReadFile(example)
	if err != nil {
		t.Fatalf("read %s: %v", example, err)
	}
	if err := rejectDeprecatedKeys(example, data); err != nil {
		t.Fatalf("rejectDeprecatedKeys: %v", err)
	}
	data = stripRetiredKeys(data)
	cfg := Default()
	if err := decodeStrict(data, &cfg); err != nil {
		t.Fatalf("decodeStrict: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	def := Default()
	if cfg.Transcription.ChunkMaxSeconds != def.Transcription.ChunkMaxSeconds {
		t.Errorf("ChunkMaxSeconds: example=%v default=%v",
			cfg.Transcription.ChunkMaxSeconds, def.Transcription.ChunkMaxSeconds)
	}
	if cfg.Transcription.HistoryTurns != def.Transcription.HistoryTurns {
		t.Errorf("HistoryTurns: example=%v default=%v",
			cfg.Transcription.HistoryTurns, def.Transcription.HistoryTurns)
	}
	if cfg.Transcription.MinChunkPeak != def.Transcription.MinChunkPeak {
		t.Errorf("MinChunkPeak: example=%v default=%v",
			cfg.Transcription.MinChunkPeak, def.Transcription.MinChunkPeak)
	}
	if cfg.Transcription.MinChunkRMS != def.Transcription.MinChunkRMS {
		t.Errorf("MinChunkRMS: example=%v default=%v",
			cfg.Transcription.MinChunkRMS, def.Transcription.MinChunkRMS)
	}
	if cfg.Transcription.Silero != def.Transcription.Silero {
		t.Errorf("Silero: example=%+v default=%+v",
			cfg.Transcription.Silero, def.Transcription.Silero)
	}
	if cfg.Transcription.Language != def.Transcription.Language {
		t.Errorf("Language: example=%q default=%q",
			cfg.Transcription.Language, def.Transcription.Language)
	}
	if cfg.Transcription.ContextMode != def.Transcription.ContextMode {
		t.Errorf("ContextMode: example=%q default=%q",
			cfg.Transcription.ContextMode, def.Transcription.ContextMode)
	}
}
