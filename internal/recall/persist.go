package recall

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Persister lets the Ring mirror its contents to durable storage so the
// buffer survives daemon restarts. nil persister means memory-only,
// which is the default and privacy-preserving behavior — no always-on
// mic audio ends up on disk unless the user opts in via
// `recall.persist_dir`.
type Persister interface {
	// Save stores a snapshot of the segment. Called after Add and after
	// every transcript update. Must be safe to call repeatedly for the
	// same ID — implementations overwrite prior writes.
	Save(seg *Segment) error
	// Delete removes the segment's stored copy. Called on evict/drop.
	// Missing files are not an error.
	Delete(id int64) error
	// Load returns every segment currently in the store, in
	// chronological order (oldest first). The daemon applies current
	// retention rules after load and deletes anything outside them.
	Load() ([]*Segment, error)
}

// FilePersister writes one JSON file per segment to a directory. Files
// are named seg-<id>.json; PCM is base64-encoded inside the JSON so
// everything a segment needs lives in a single atomic write.
type FilePersister struct {
	dir string
}

// NewFilePersister creates the directory (0700) if it doesn't already
// exist and returns a persister that writes into it. Accepts ~/... paths.
func NewFilePersister(dir string) (*FilePersister, error) {
	expanded, err := expandHome(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(expanded, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", expanded, err)
	}
	return &FilePersister{dir: expanded}, nil
}

// Dir returns the resolved directory for diagnostics.
func (p *FilePersister) Dir() string { return p.dir }

// persistedSegment is the on-disk JSON shape. Kept separate from the
// in-memory Segment so we can evolve either without breaking the other.
type persistedSegment struct {
	Version    int       `json:"version"`
	ID         int64     `json:"id"`
	StartedAt  time.Time `json:"started_at"`
	DurationNS int64     `json:"duration_ns"`
	SampleRate int       `json:"sample_rate"`
	PeakLevel  float64   `json:"peak_level"`
	AvgLevel   float64   `json:"avg_level,omitempty"`
	Transcript string    `json:"transcript,omitempty"`
	PCMBase64  string    `json:"pcm_b64"`
}

const persistedSegmentVersion = 1

func (p *FilePersister) Save(seg *Segment) error {
	payload, err := json.Marshal(persistedSegment{
		Version:    persistedSegmentVersion,
		ID:         seg.ID,
		StartedAt:  seg.StartedAt,
		DurationNS: int64(seg.Duration),
		SampleRate: seg.SampleRate,
		PeakLevel:  seg.PeakLevel,
		AvgLevel:   seg.AvgLevel,
		Transcript: seg.Transcript,
		PCMBase64:  encodePCM16(seg.PCM),
	})
	if err != nil {
		return fmt.Errorf("marshal segment %d: %w", seg.ID, err)
	}
	final := p.filePath(seg.ID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	// Atomic rename — readers either see the old file or the new file,
	// never a half-written JSON blob.
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", final, err)
	}
	return nil
}

func (p *FilePersister) Delete(id int64) error {
	err := os.Remove(p.filePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (p *FilePersister) Load() ([]*Segment, error) {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p.dir, err)
	}
	var segs []*Segment
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".json") || e.IsDir() {
			continue
		}
		full := filepath.Join(p.dir, name)
		data, err := os.ReadFile(full)
		if err != nil {
			// Skip unreadable files rather than aborting the whole
			// reload — a corrupted segment shouldn't make the daemon
			// refuse to start.
			continue
		}
		var ps persistedSegment
		if err := json.Unmarshal(data, &ps); err != nil {
			continue
		}
		pcm, err := decodePCM16(ps.PCMBase64)
		if err != nil {
			continue
		}
		segs = append(segs, &Segment{
			ID:         ps.ID,
			StartedAt:  ps.StartedAt,
			Duration:   time.Duration(ps.DurationNS),
			SampleRate: ps.SampleRate,
			PCM:        pcm,
			PeakLevel:  ps.PeakLevel,
			AvgLevel:   ps.AvgLevel,
			Transcript: ps.Transcript,
		})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].StartedAt.Before(segs[j].StartedAt) })
	return segs, nil
}

func (p *FilePersister) filePath(id int64) string {
	return filepath.Join(p.dir, fmt.Sprintf("seg-%d.json", id))
}

// encodePCM16 packs int16 samples little-endian into a base64 string.
// Matches the way WAV files store mono PCM16 so the on-disk data is
// trivially recoverable with standard tools if ever needed.
func encodePCM16(pcm []int16) string {
	if len(pcm) == 0 {
		return ""
	}
	buf := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

func decodePCM16(s string) ([]int16, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	if len(raw)%2 != 0 {
		return nil, fmt.Errorf("odd byte count (%d) — not PCM16", len(raw))
	}
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return out, nil
}

// expandHome resolves env vars and a leading ~/ to absolute paths so
// yaml values like `$HOME/.local/state/vocis/recall` or
// `~/.local/state/vocis/recall` both work and stay portable across
// machines. Other tilde forms (~user/...) are not supported — the Go
// stdlib has no portable way to resolve them.
func expandHome(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("empty path")
	}
	p = os.ExpandEnv(p)
	if !strings.HasPrefix(p, "~/") && p != "~" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}
