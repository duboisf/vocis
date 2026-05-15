// Package audiocapture mirrors every chunk POSTed to the
// /chat/completions endpoint to a WAV file on disk for after-the-fact
// bug replay. Files are written to a per-user state dir, named to
// correlate with the session log timestamp, and pruned by a long-
// lived GC goroutine.
//
// Lifecycle:
//
//   - The app opens one Writer at startup (NewWriter) with a session
//     timestamp matching the session log. Each chunk POST in chat_audio
//     calls Writer.WriteChunk(reason, wavBytes), which writes a file
//     named "<session-ts>-chunkNNN-<reason>.wav" and bumps the counter.
//   - StartGC launches a goroutine that sweeps the audio dir at startup
//     and on every gc_interval tick, deleting files older than TTL.
//   - Both Writer and GC become no-ops when audio_capture.enabled is
//     false.
package audiocapture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
)

// Dir returns the audio-capture directory, creating it if necessary.
// Mirrors sessionlog.logDir's $XDG_STATE_HOME convention so the audio
// files live next to the session logs (~/.local/state/vocis/audio).
func Dir() (string, error) {
	base := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "vocis", "audio")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// Writer mirrors POSTed chunk WAVs to disk for replay. A nil receiver
// is a valid no-op so call sites don't need to branch on the config.
type Writer struct {
	dir       string
	sessionTS string
	enabled   bool
	counter   atomic.Uint64
}

// NewWriter opens the writer for one app session. When cfg.Enabled is
// false the returned Writer is non-nil but no-ops on WriteChunk; this
// keeps the call-site shape identical regardless of config.
func NewWriter(cfg config.AudioCaptureConfig) (*Writer, error) {
	if !cfg.Enabled {
		return &Writer{enabled: false}, nil
	}
	dir, err := Dir()
	if err != nil {
		return nil, fmt.Errorf("audio capture: prepare dir: %w", err)
	}
	w := &Writer{
		dir:       dir,
		sessionTS: time.Now().Format("20060102-150405"),
		enabled:   true,
	}
	sessionlog.Infof("audio capture: writing chunks to %s (session=%s)", dir, w.sessionTS)
	return w, nil
}

// reasonSlugRE keeps filenames POSIX-safe and grep-friendly.
var reasonSlugRE = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func slugifyReason(reason string) string {
	s := reasonSlugRE.ReplaceAllString(strings.TrimSpace(reason), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "unknown"
	}
	return s
}

// WriteChunk writes one chunk's WAV bytes to disk. Returns the path
// written (empty when disabled or on error — error is logged, not
// propagated, because audio capture must never break a dictation).
// Safe to call concurrently: the counter is atomic and each call
// writes a distinct file.
func (w *Writer) WriteChunk(reason string, wav []byte) string {
	if w == nil || !w.enabled {
		return ""
	}
	if len(wav) == 0 {
		return ""
	}
	n := w.counter.Add(1)
	name := fmt.Sprintf("%s-chunk%03d-%s.wav", w.sessionTS, n, slugifyReason(reason))
	path := filepath.Join(w.dir, name)
	if err := os.WriteFile(path, wav, 0o600); err != nil {
		sessionlog.Warnf("audio capture: write %s failed: %v", path, err)
		return ""
	}
	sessionlog.Infof("audio capture: wrote %s bytes=%d", path, len(wav))
	return path
}

// Enabled reports whether this writer will actually write files. The
// caller can use this to skip work (e.g. encoding the WAV) when the
// feature is off — though WriteChunk itself is a cheap no-op.
func (w *Writer) Enabled() bool {
	return w != nil && w.enabled
}

// StartGC launches a goroutine that prunes stale WAVs from the audio
// directory. It runs one sweep immediately, then sweeps every
// cfg.GCIntervalSeconds until ctx is done. No-op when cfg.Enabled is
// false. Safe to call once at app startup.
func StartGC(ctx context.Context, cfg config.AudioCaptureConfig) {
	if !cfg.Enabled {
		return
	}
	dir, err := Dir()
	if err != nil {
		sessionlog.Warnf("audio capture: gc disabled — dir prepare failed: %v", err)
		return
	}
	ttl := time.Duration(cfg.TTLSeconds) * time.Second
	interval := time.Duration(cfg.GCIntervalSeconds) * time.Second
	sessionlog.Infof("audio capture: gc started dir=%s ttl=%s interval=%s", dir, ttl, interval)

	go func() {
		// Startup sweep happens synchronously inside the goroutine so
		// app startup isn't blocked by a large dir scan on machines
		// that suspended overnight with thousands of stale files.
		sweep(dir, ttl)

		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				sessionlog.Debugf("audio capture: gc stopped")
				return
			case <-t.C:
				sweep(dir, ttl)
			}
		}
	}()
}

// sweep deletes every .wav file in dir whose ModTime is older than
// now-ttl. Errors are logged but never propagated — GC failure must
// never break the app. Returns count of files deleted (exposed for
// tests).
func sweep(dir string, ttl time.Duration) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		sessionlog.Warnf("audio capture: gc read dir %s: %v", dir, err)
		return 0
	}
	cutoff := time.Now().Add(-ttl)
	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".wav") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(dir, entry.Name())
			if err := os.Remove(path); err != nil {
				sessionlog.Warnf("audio capture: gc remove %s: %v", path, err)
				continue
			}
			deleted++
		}
	}
	if deleted > 0 {
		sessionlog.Infof("audio capture: gc deleted %d files older than %s", deleted, ttl)
	} else {
		sessionlog.Debugf("audio capture: gc swept dir=%s (0 stale)", dir)
	}
	return deleted
}
