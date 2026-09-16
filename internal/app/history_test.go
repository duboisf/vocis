package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendTranscriptHistoryWritesOneJSONLinePerDictation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VOCIS_TEST_HISTORY_DIR", dir)
	path := "$VOCIS_TEST_HISTORY_DIR/nested/transcripts.jsonl"

	first := historyEntry{Time: time.Date(2026, 9, 16, 14, 0, 0, 0, time.UTC), Text: "Hello world.", AudioMS: 1500, WindowClass: "kitty"}
	second := historyEntry{Time: first.Time.Add(time.Minute), Text: "Second one.", Audio: "/tmp/x.wav", AudioMS: 900}
	for _, e := range []historyEntry{first, second} {
		if err := appendTranscriptHistory(path, e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	raw, err := os.ReadFile(filepath.Join(dir, "nested", "transcripts.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%d want 2: %q", len(lines), raw)
	}
	var got historyEntry
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Text != "Second one." || got.Audio != "/tmp/x.wav" || got.AudioMS != 900 {
		t.Fatalf("entry=%+v", got)
	}
}
