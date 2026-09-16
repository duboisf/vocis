package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// historyEntry is one line of the transcript history file: what was
// pasted, when, into which app, and where the matching audio capture
// lives (empty when capture is off or the WAV has been pruned).
type historyEntry struct {
	Time        time.Time `json:"time"`
	Text        string    `json:"text"`
	Audio       string    `json:"audio,omitempty"`
	AudioMS     int       `json:"audio_ms"`
	WindowClass string    `json:"window_class,omitempty"`
}

// appendTranscriptHistory appends e as one JSON line to path, creating
// the parent directory on first use. path supports $VAR expansion.
func appendTranscriptHistory(path string, e historyEntry) error {
	path = os.ExpandEnv(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}
