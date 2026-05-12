package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExampleYAMLMatchesDefaults parses config.example.yaml under the
// post-cull flat shape and verifies every kept knob lands on the
// Config struct at the same value Default() would produce. The example
// is the canonical documentation for the YAML surface, so drift
// between it and Default() is treated as a regression.
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
	if cfg.Transcription.MinChunkPeak != def.Transcription.MinChunkPeak {
		t.Errorf("MinChunkPeak: example=%v default=%v",
			cfg.Transcription.MinChunkPeak, def.Transcription.MinChunkPeak)
	}
	if cfg.Transcription.MinChunkRMS != def.Transcription.MinChunkRMS {
		t.Errorf("MinChunkRMS: example=%v default=%v",
			cfg.Transcription.MinChunkRMS, def.Transcription.MinChunkRMS)
	}
	if cfg.Transcription.Silero.OnnxruntimeLibrary != def.Transcription.Silero.OnnxruntimeLibrary {
		t.Errorf("Silero.OnnxruntimeLibrary: example=%q default=%q",
			cfg.Transcription.Silero.OnnxruntimeLibrary, def.Transcription.Silero.OnnxruntimeLibrary)
	}
	if cfg.Transcription.Language != def.Transcription.Language {
		t.Errorf("Language: example=%q default=%q",
			cfg.Transcription.Language, def.Transcription.Language)
	}
}
