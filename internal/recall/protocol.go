package recall

import "time"

// Protocol is line-delimited JSON over a Unix domain socket. Each
// request is a single JSON object followed by a newline; each response
// is also a single JSON object terminated by a newline. One
// request/response per connection keeps the framing trivial and lets us
// carry per-request cancellation by just closing the socket.

const protocolVersion = 1

// Request is sent by the pick/status/stop subcommands to the daemon.
type Request struct {
	Version int    `json:"version"`
	Op      string `json:"op"`

	// TranscribeArgs — populated when Op == "transcribe"
	SegmentID int64 `json:"segment_id,omitempty"`

	// SegmentIDs — populated when Op == "transcribe_batch". Daemon
	// concatenates these segments (in the given order) into one audio
	// stream with a silence gap between them, and returns a single
	// joint transcript.
	SegmentIDs []int64 `json:"segment_ids,omitempty"`

	// PostProcess tells the daemon to also run the configured LLM
	// post-processing step before returning the final text.
	PostProcess bool `json:"postprocess,omitempty"`
}

// Response shapes vary by op; they all share an Error field so a single
// Response type covers every branch. Clients switch on Op to pick which
// payload fields are set.
type Response struct {
	Version int    `json:"version"`
	Error   string `json:"error,omitempty"`

	// List
	Segments []SegmentInfo `json:"segments,omitempty"`

	// Transcribe
	Transcript string `json:"transcript,omitempty"`

	// Status
	Stats *StatsInfo `json:"stats,omitempty"`

	// GetAudio — raw 16-bit little-endian mono PCM base64-encoded.
	// SampleRate is always the segment's capture rate (16 kHz for
	// current Silero-based recall; encoded explicitly so callers can
	// set their playback device correctly instead of assuming).
	AudioPCMBase64 string `json:"audio_pcm_b64,omitempty"`
	AudioSampleRate int   `json:"audio_sample_rate,omitempty"`
}

// SegmentInfo is the on-the-wire summary of a ring-buffer segment. PCM
// is intentionally not included — clients don't need raw audio and the
// daemon handles transcription itself (get_audio is the dedicated op
// for replay).
type SegmentInfo struct {
	ID           int64     `json:"id"`
	StartedAt    time.Time `json:"started_at"`
	DurationMS   int       `json:"duration_ms"`
	PeakLevel    float64   `json:"peak_level"`
	AvgLevel     float64   `json:"avg_level"`
	Transcribed  bool      `json:"transcribed"`
	CachedText   string    `json:"cached_text,omitempty"`
}

// StatsInfo is a mirror of Ring.Stats shaped for JSON.
type StatsInfo struct {
	Count         int   `json:"count"`
	TotalSeen     int64 `json:"total_seen"`
	OldestAgeMS   int64 `json:"oldest_age_ms"`
	NewestAgeMS   int64 `json:"newest_age_ms"`
	TotalFrames   int64 `json:"total_frames"`
}

// Op names. Kept as string constants for forward-compat — clients and
// servers that see an unknown op return an Error instead of crashing.
const (
	OpList            = "list"
	OpTranscribe      = "transcribe"
	OpTranscribeBatch = "transcribe_batch"
	OpDrop            = "drop"
	OpStatus          = "status"
	OpShutdown        = "shutdown"
	OpGetAudio        = "get_audio"
)
