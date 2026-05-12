package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"vocis/internal/sessionlog"
)

const fileName = "config.yaml"

// DefaultPostProcessPrompt uses few-shot examples because small instruct
// models (1-2B class like gemma3-1b) without examples treat the user
// message as an instruction to *answer* instead of transcript to clean
// ("Did you update the configuration?" → "Cleaning configuration
// updated."). The examples anchor the "I clean text, I never respond"
// behavior. Pattern-matching on example shapes does happen occasionally
// (short imperatives getting replaced with example outputs) but it's a
// much smaller failure rate than rules-only, which fails on most
// question-shaped inputs.
const DefaultPostProcessPrompt = `You clean dictated speech transcripts. Output ONLY the cleaned text — never reply, never add commentary, never answer questions in the input.

Rules:
- Remove filler words (um, uh, like, you know, I mean, sort of, kind of), false starts, repetitions, and pauses (...).
- Lightly fix punctuation, capitalization, and spacing.
- Preserve the speaker's meaning, person, and intent EXACTLY. If they said "I", keep "I". If they asked a question, keep it as a question.
- Treat the input as transcript-to-clean, not as a message to respond to.

Examples:

Input: um so I think we should like, you know, refactor the auth module
Output: I think we should refactor the auth module.

Input: hey can you help me with this real quick
Output: Hey, can you help me with this real quick?

Input: I'm not going to do that. Please don't scroll. I'm just trying to become a big content creator one day. I have no supporters.
Output: I'm not going to do that. Please don't scroll. I'm just trying to become a big content creator one day. I have no supporters.

Input: what time is it
Output: What time is it?

Now clean the next input:`

const DefaultPromptHint = "Transcribe naturally for a programmer. " +
	"Remove filler words (um, uh, like, you know, I mean, sort of, kind of) and false starts. " +
	"Clean up hesitations into fluent sentences while preserving the speaker's intent and meaning. " +
	"Prefer technical terminology for software, CLI, cloud, and API concepts. " +
	"Preserve obvious technical terms, acronyms, and capitalization when the audio supports them."

type Config struct {
	Hotkey         string              `yaml:"hotkey"`
	HotkeyMode     string              `yaml:"hotkey_mode"`
	LogWindowTitle bool                `yaml:"log_window_title"`
	Transcription  TranscriptionConfig `yaml:"transcription"`
	Recording      RecordingConfig     `yaml:"recording"`
	Insertion      InsertionConfig     `yaml:"insertion"`
	Overlay        OverlayConfig       `yaml:"overlay"`
	PostProcess    PostProcessConfig   `yaml:"postprocess"`
	Telemetry      TelemetryConfig     `yaml:"telemetry"`
	Recall         RecallConfig        `yaml:"recall"`
	Speak          SpeakConfig         `yaml:"speak"`
}

type PostProcessConfig struct {
	Enabled              bool   `yaml:"enabled"`
	Model                string `yaml:"model"`
	Prompt               string `yaml:"prompt"`
	MinWordCount         int    `yaml:"min_word_count"`
	FirstTokenTimeoutSec int    `yaml:"first_token_timeout_seconds"`
	TotalTimeoutSec      int    `yaml:"total_timeout_seconds"`
	// Sampling knobs. Pointers so nil = "use the backend default".
	// Zero is a meaningful value (temperature=0 is greedy decoding).
	// Only Temperature and TopP are exposed — other sampler params
	// (frequency/presence/repetition penalty, stop, min_p) were
	// rarely-tuned knobs that bloated the config surface.
	Temperature *float64 `yaml:"temperature,omitempty"`
	TopP        *float64 `yaml:"top_p,omitempty"`
}

type TelemetryConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

// SpeakConfig drives the `vocis speak` text-to-speech command. The
// only backend currently supported is Lemonade's OpenAI-compatible
// /audio/speech endpoint, which serves Kokoro TTS locally. BaseURL
// defaults to the same host as transcription.base_url at load time
// when left empty, since most users run a single Lemonade instance.
type SpeakConfig struct {
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	Voice   string `yaml:"voice"`
}

// RecallConfig drives the always-on `vocis recall` daemon. The daemon
// captures mic audio continuously, runs Silero VAD, and keeps a bounded
// ring buffer of speech segments that the user can later transcribe via
// `vocis recall pick`. See docs/overview.md for the user-facing shape.
type RecallConfig struct {
	// RetentionSeconds is how far back in time segments are kept. Older
	// segments are evicted from the ring buffer even if MaxSegments
	// isn't reached yet. 0 disables the time bound (count-only).
	RetentionSeconds int `yaml:"retention_seconds"`
	// MaxSegments caps the number of segments held in memory. Oldest is
	// evicted when a new segment is added past this count. 0 disables
	// the count bound (time-only).
	MaxSegments int `yaml:"max_segments"`
	// SocketPath is the Unix domain socket the daemon listens on and
	// the pick subcommand connects to. Empty = auto-resolve under
	// $XDG_RUNTIME_DIR/vocis/recall.sock (or /tmp fallback).
	SocketPath string `yaml:"socket_path"`
	// MinSilenceMS / MinSpeechMS / MinUtteranceMS mirror the Silero VAD
	// hysteresis knobs in internal/transcribe/silero.go. They control
	// when a speech episode starts, when it ends, and whether it's long
	// enough to keep.
	MinSilenceMS   int `yaml:"min_silence_ms"`
	MinSpeechMS    int `yaml:"min_speech_ms"`
	MinUtteranceMS int `yaml:"min_utterance_ms"`
	// PrerollMS is how much audio before the VAD speech-start is
	// included in the segment, so word onsets aren't clipped.
	PrerollMS int `yaml:"preroll_ms"`
	// MaxSegmentSeconds caps an individual segment's duration. A long
	// monologue without a pause gets flushed at this boundary so the
	// ring buffer can't grow unbounded from a single stream.
	MaxSegmentSeconds int `yaml:"max_segment_seconds"`
	// MinSegmentPeak is the minimum peak sample level (0-1, abs-max /
	// 32768) a finalized segment must have to be kept in the ring
	// buffer. Below this, the segment is treated as silence or noise
	// and dropped. Silero VAD can get stuck declaring in-speech on
	// low-level ambient noise (fans, keyboards) that briefly crosses
	// its probability threshold; without this filter those sessions
	// fill the ring with 30 s force-flushed noise segments. A value
	// around 0.02 rejects fan/room tone while keeping quiet speech.
	// Set to 0 to keep every segment (useful only for debugging).
	MinSegmentPeak float64 `yaml:"min_segment_peak"`
	// MinSegmentRMS is the minimum RMS (root mean square) sample level
	// a finalized segment must have to be kept. RMS discriminates
	// sustained energy from a silent segment that happens to contain
	// a single loud click, which peak alone can't: a 24 s silence
	// segment with one keyboard clack can easily have peak > 0.04
	// while its RMS stays below 0.005. A value around 0.005 rejects
	// near-silence while keeping genuinely quiet speech (speech RMS
	// is typically 0.01-0.05 even at soft volumes). Set to 0 to
	// disable the RMS filter and rely on peak alone.
	MinSegmentRMS float64 `yaml:"min_segment_rms"`
	// Persist controls whether captured segments are mirrored to disk.
	// Default is memory-only — always-on mic audio does not land on
	// disk unless the user explicitly opts in by setting mode=disk.
	Persist RecallPersistConfig `yaml:"persist"`
}

// RecallPersistConfig is the nested `recall.persist` block. Mode is
// the on/off switch; Dir is only consulted when Mode is "disk" but is
// pre-populated with a sensible default so the user can flip mode with
// a single-line config change.
type RecallPersistConfig struct {
	// Mode is "in_memory" (default) or "disk". In-memory keeps the
	// ring buffer entirely in the daemon process — nothing written,
	// restart loses the buffer. Disk mirrors each segment to Dir as
	// one JSON file per segment (raw PCM base64-encoded + metadata +
	// cached transcript); the ring is reloaded on daemon start, with
	// the current retention applied.
	Mode string `yaml:"mode"`
	// Dir is where segment JSON files live when Mode is "disk". The
	// directory is created with mode 0700 at daemon startup. Supports
	// a leading ~/ expansion. Defaults to $XDG_STATE_HOME/vocis/recall
	// when XDG_STATE_HOME is set, else ~/.local/state/vocis/recall.
	Dir string `yaml:"dir"`
}

const (
	RecallPersistMemory = "in_memory"
	RecallPersistDisk   = "disk"
)

// TranscriptionConfig holds every transcription knob. With only one
// backend (lemonade-chat) left, the chunker/few-shot/Silero/batch
// knobs that used to live under `transcription.chat_audio.*` were
// hoisted onto this struct directly.
type TranscriptionConfig struct {
	BaseURL    string `yaml:"base_url"`
	Model      string `yaml:"model"`
	PromptHint string `yaml:"prompt_hint"`
	// RequestLimit is the HTTP request timeout (seconds) applied to the
	// transcription SDK client. Gates postprocess `/chat/completions`
	// and any other REST calls. Set to 0 to disable the timeout
	// entirely — useful on Lemonade where a cold local model load can
	// legitimately take minutes. The WS handshake and write deadlines
	// still get a 5 s floor regardless.
	RequestLimit int `yaml:"request_timeout_seconds"`
	// HallucinationFilters drops finals whose trimmed text exactly
	// matches one of these entries (case-insensitive). Whisper and
	// Gemma-FLM routinely hallucinate stock phrases — "Thank you.",
	// "Thanks for watching." — on silence or very quiet audio. Exact
	// match only; a substring filter would eat legitimate speech.
	HallucinationFilters []string `yaml:"hallucination_filters"`
	// ChunkMaxSeconds is the upper bound on a single chunk's audio
	// duration, in seconds. Gemma 3n / 4 cap audio at 30s per request,
	// so we hold a safety margin under that. A long monologue without
	// a VAD-detected pause gets force-cut at this boundary and the
	// remainder rolls into the next chunk.
	ChunkMaxSeconds int `yaml:"chunk_max_seconds"`
	// HistoryTurns is how many prior (user-audio, assistant-transcript)
	// pairs to include as few-shot context on each request. 0 disables
	// history entirely (each chunk transcribed in isolation). Larger
	// values balloon the request body fast — at 30s of 16 kHz mono
	// PCM16 each prior turn adds ~1.3 MB of base64 audio, so keep it
	// modest. Default 2 matches the user's tested payload shape.
	HistoryTurns int `yaml:"history_turns"`
	// Prompt is the instruction text sent alongside every audio chunk.
	// Defaults to the Google-recommended ASR prompt with formatting
	// rules. {language} expands to Language at send time.
	Prompt string `yaml:"prompt"`
	// Language is the spoken language hint substituted into Prompt.
	// "its original language" lets the model auto-detect (and is what
	// the user tested with). Set to "en", "fr", etc. to force.
	Language string `yaml:"language"`
	// Stream controls whether requests use SSE streaming. true emits
	// per-token deltas to the overlay as they arrive; false waits for
	// the full completion before showing the chunk transcript.
	Stream bool `yaml:"stream"`
	// ContextMode picks how prior chunks are threaded into a new
	// request:
	//   "few_shot"     — each prior chunk becomes a (user-audio,
	//                    assistant-transcript) turn pair. The model
	//                    sees its own transcripts verbatim, which
	//                    keeps proper-noun spelling stable across
	//                    chunks. Cost: re-uploads the prior audio
	//                    bytes on every request.
	//   "inline_clips" — prior chunks are appended as additional
	//                    `input_audio` parts inside the same user
	//                    message, alongside the current chunk. The
	//                    model gets all audio at once and produces
	//                    one transcript covering only the final clip
	//                    (per the prompt instruction). Matches the
	//                    multi-clip shape Google's Gemma docs show.
	//                    Cost: model has to re-decode prior audio
	//                    instead of trusting a prior transcript.
	// Empty value defaults to "few_shot" for backward compatibility
	// with users who set up the backend before this knob existed.
	ContextMode string `yaml:"context_mode"`
	// MinChunkPeak / MinChunkRMS gate every chunk on peak (max-abs)
	// and RMS sample energy (each normalized to 0-1 by /32768) before
	// a /chat/completions POST. Below either threshold the chunk is
	// dropped client-side. Critical when Silero isn't installed —
	// without VAD, a hold over silence sends the entire silent buffer
	// to the model, and Gemma helpfully invents a long
	// "I cannot transcribe..." response. With Silero installed, VAD
	// already trims silence around utterances, so this gate rarely
	// fires; it just provides defense in depth.
	//
	// Defaults match recall's segment filters (0.02 / 0.005). Set
	// either to 0 to disable that arm of the check.
	MinChunkPeak float64 `yaml:"min_chunk_peak"`
	MinChunkRMS  float64 `yaml:"min_chunk_rms"`
	// BatchPrompt is the system prompt for the one-shot multi-segment
	// transcription used by `vocis recall last`. The model receives
	// every segment as a labelled input_audio part and emits one line
	// per segment prefixed with the capture timestamp. Override to
	// tweak formatting or per-language rules. {language} expands to
	// Language at send time.
	BatchPrompt string `yaml:"batch_prompt"`
	// BatchMaxAudioSeconds caps the total audio duration packed into
	// one /chat/completions request when `recall last` has more audio
	// than fits in a single shot. Gemma's audio inputs are capped at
	// ~30s each AND the model degrades with too many multimodal
	// inputs at once, so a multi-minute window has to be split. The
	// caller packs as many segments as fit under this budget per
	// request and sends them sequentially. Smaller = more requests
	// but each is fast and safe; larger = fewer requests but risk
	// hitting the model's effective context limit. 0 = auto-derive
	// from the loaded model's recipe_options.ctx_size via
	// /api/v1/health.
	BatchMaxAudioSeconds int `yaml:"batch_max_audio_seconds"`
	// CtxSize requests a specific Lemonade context-window size for
	// the loaded model. 0 (default) leaves Lemonade alone — uses
	// whatever ctx_size the server was started with (env var
	// LEMONADE_CTX_SIZE, recipe_options.json, CLI flag). A positive
	// value triggers a POST to /api/v1/load at daemon startup that
	// reloads the model with this ctx_size; the daemon skips the
	// reload when /api/v1/health reports the current ctx_size
	// already matches. Bigger ctx = larger batch budget (more audio
	// fits per request) but more NPU/GPU memory pinned per model.
	// Gemma 4 E2B/E4B's theoretical ceiling is 131072.
	CtxSize int `yaml:"ctx_size"`
	// BatchUntilRelease changes the chat-audio chunking policy: Silero
	// still trims dead air between speech episodes, but each
	// speech_stopped flush stashes the clip into the pending batch
	// instead of POSTing immediately. Only the trailing flush at
	// hotkey release (or a chunk_max_seconds force-cut spillover)
	// actually sends — as one multi-clip request covering the whole
	// utterance. Trade-off: no live overlay partials during dictation,
	// one paste at the end. Wins: model sees the full utterance as one
	// continuous thing so punctuation/casing don't break across
	// pauses, and history (which was the source of mid-phrase
	// regressions) is moot for the single request. Default false
	// preserves the per-pause request behavior.
	BatchUntilRelease bool `yaml:"batch_until_release"`
	// ContinuationRebatch keeps the per-pause POST cadence but, when
	// the previous chunk's transcript ends without terminal
	// punctuation (./?/!/…), sends the NEXT chunk as a multi-clip
	// request that prepends the prior chunk's audio. The model
	// returns one unified transcript covering both clips, which
	// replaces (not appends to) the prior history entry, and a
	// DictationEventReplaceSegment is emitted so the overlay/injector
	// can retract the broken prior segment and substitute the fix.
	// Mutually exclusive with BatchUntilRelease (which already sends
	// one POST per utterance so there's nothing to rebatch). Off by
	// default.
	ContinuationRebatch bool `yaml:"continuation_rebatch"`
	// Silero is the VAD hysteresis block used by the chat-audio
	// chunker. The realtime-WS backend (gone since the previous
	// commit) used to read these knobs from a top-level streaming:
	// section; now that chat-audio is the only consumer, they live
	// inline with the rest of its config.
	Silero SileroConfig `yaml:"silero"`
}

// SileroConfig holds the Silero VAD hysteresis knobs the chat-audio
// chunker uses to bound a speech episode. The semantic field names
// (silence_ms / speech_ms) replace the OpenAI-realtime-leaking names
// the old streaming: section used (silence_duration_ms /
// prefix_padding_ms).
type SileroConfig struct {
	// OnnxruntimeLibrary is an optional absolute path to
	// libonnxruntime.so. When empty, vocis auto-discovers the library
	// from common install locations. Supports $HOME / ${VAR} expansion
	// at runtime.
	OnnxruntimeLibrary string `yaml:"onnxruntime_library"`
	// SilenceMS is the minimum non-speech duration that closes a
	// speech episode. Was streaming.silence_duration_ms. Default 500.
	SilenceMS int `yaml:"silence_ms"`
	// SpeechMS is the minimum speech duration that opens a speech
	// episode. Was streaming.prefix_padding_ms. Default 150.
	SpeechMS int `yaml:"speech_ms"`
	// MinUtteranceMS is the minimum accumulated speech an episode
	// needs before it can be committed. Shorter episodes are dropped
	// to keep Gemma from hallucinating on sub-second clips. Default
	// 1000.
	MinUtteranceMS int `yaml:"min_utterance_ms"`
}

const (
	ChatAudioContextFewShot     = "few_shot"
	ChatAudioContextInlineClips = "inline_clips"
)


// DefaultChatAudioPrompt is the transcription instruction validated
// against gemma4-it-e2b-FLM via Lemonade. The {language} token
// expands to ChatAudioConfig.Language at request build time.
const DefaultChatAudioPrompt = "Transcribe the following speech segment in {language}. " +
	"Follow these specific instructions for formatting the answer:\n" +
	"* Only output the transcription, with no newlines.\n" +
	"* When transcribing numbers, write the digits, i.e. write 1.7 and not one point seven, and write 3 instead of three."

// DefaultBatchPrompt drives the one-shot multi-segment batch path used
// by `vocis recall last`. Each segment arrives as a labelled
// input_audio part of the form "[clip N captured at HH:MM:SS]:" and
// the model is asked to emit exactly one line per segment, prefixed
// with that timestamp. {language} expands to ChatAudioConfig.Language.
const DefaultBatchPrompt = "Transcribe each of the following speech segments in {language}. " +
	"Each segment is preceded by a label of the form \"[clip N captured at HH:MM:SS]:\". " +
	"Output one line per input segment, in input order, formatted exactly as:\n" +
	"  HH:MM:SS\\t<transcript>\n" +
	"where HH:MM:SS is copied verbatim from the segment's label and <transcript> is the cleaned speech.\n" +
	"Cleanup: remove fillers (um, uh), fix punctuation, write digits for numbers (1.7 not one point seven). " +
	"If a segment has no intelligible speech, output its timestamp followed by a tab and nothing else. " +
	"Never output preamble, commentary, bullet points, or anything beyond the requested lines."

type RecordingConfig struct {
	Backend            string  `yaml:"backend"`
	Device             string  `yaml:"device"`
	SampleRate         int     `yaml:"sample_rate"`
	Channels           int     `yaml:"channels"`
	MaxDurationSeconds int     `yaml:"max_duration_seconds"`
	DuckVolume         float64 `yaml:"duck_volume"`
}


type InsertionConfig struct {
	Mode             string   `yaml:"mode"`
	DefaultPasteKey  string   `yaml:"default_paste_key"`
	TerminalPasteKey string   `yaml:"terminal_paste_key"`
	RestoreClipboard bool     `yaml:"restore_clipboard"`
	TerminalClasses  []string `yaml:"terminal_classes"`
	// AutoSubmit defaults every dictation to "submit mode" — after the
	// transcript is pasted, an Enter/Return keypress is sent. Useful
	// when you dictate mostly into chat inputs that send on Enter.
	// Can still be toggled off per-session by tapping the hotkey.
	AutoSubmit bool `yaml:"auto_submit"`
	// KittyRemoteControl enables tab/pane-aware paste for kitty
	// terminals via `kitty @ ls` / `kitty @ focus-window`. When the
	// target window class is a known kitty class at recording start,
	// vocis records the focused kitty internal window id; at paste
	// time it focuses that exact tab/pane via kitty remote control
	// before sending the paste shortcut. If the original tab/pane has
	// been closed mid-recording, the transcript is written to the
	// clipboard and a warning is shown instead of pasting into
	// whatever happens to be focused. Requires `allow_remote_control`
	// (and ideally `listen_on`) to be configured in kitty.conf, or
	// vocis to be launched from inside kitty so KITTY_LISTEN_ON is
	// inherited. Set to false to skip the kitty enrichment entirely.
	KittyRemoteControl bool `yaml:"kitty_remote_control"`
	// KittyVerifyPaste runs an extra `kitty @ get-text --extent screen`
	// snapshot after a successful send-text and warns if neither the
	// payload's first 20 chars nor a `Pasted text` marker shows up in
	// the rendered screen. send-text exits 0 even when the receiving
	// program silently swallows the bytes; this probe is the only way
	// to distinguish "delivered" from "delivered and rendered." Adds
	// one extra kitty CLI shell-out per dictation when on. Default
	// true — disable only if the extra subprocess shows up in latency
	// profiles.
	KittyVerifyPaste bool `yaml:"kitty_verify_paste"`
}

type OverlayConfig struct {
	Width          int            `yaml:"width"`
	Height         int            `yaml:"height"`
	MarginTop      int            `yaml:"margin_top"`
	Opacity        float64        `yaml:"opacity"`
	AutoHideMillis int            `yaml:"auto_hide_millis"`
	Font           string         `yaml:"font"`
	FontSize       float64        `yaml:"font_size"`
	Branding       string         `yaml:"branding"`
	Ready          OverlayReady   `yaml:"ready"`
	Listening      OverlayListen  `yaml:"listening"`
	Finishing      OverlayFinish  `yaml:"finishing"`
	Success        OverlaySuccess `yaml:"success"`
	Error          OverlayError   `yaml:"error"`
	Warning        OverlayWarning `yaml:"warning"`
}

type OverlayReady struct {
	Title    string `yaml:"title"`
	Subtitle string `yaml:"subtitle"`
}

type OverlayListen struct {
	Title        string `yaml:"title"`
	Suffix       string `yaml:"suffix"`
	SubmitHint   string `yaml:"submit_hint"`
	Connecting   string `yaml:"connecting"`
	Reconnecting string `yaml:"reconnecting"`
	Connected    string `yaml:"connected"`
	// LoadingModel is the subtitle shown while vocis is forcing a
	// local transcription model into memory at session-start. Supports
	// {model} template expansion with the configured transcribe model
	// name. Only applies on the Lemonade backend — cloud backends
	// always have the model warm.
	LoadingModel string `yaml:"loading_model"`
}

type OverlayFinish struct {
	Title      string `yaml:"title"`
	CancelHint string `yaml:"cancel_hint"`
	WrappingUp string `yaml:"wrapping_up"`
	PPWait     string `yaml:"pp_wait"`
	PPStream   string `yaml:"pp_stream"`
	PhaseDone  string `yaml:"phase_done"`
}

type OverlaySuccess struct {
	Title    string `yaml:"title"`
	Subtitle string `yaml:"subtitle"`
}

type OverlayError struct {
	Title string `yaml:"title"`
}

type OverlayWarning struct {
	Title              string `yaml:"title"`
	NoSpeech           string `yaml:"no_speech"`
	Cancelled          string `yaml:"cancelled"`
	PostprocessSkipped string `yaml:"postprocess_skipped"`
	TargetGone         string `yaml:"target_gone"`
}

func Default() Config {
	return Config{
		Hotkey:     "ctrl+shift+space",
		HotkeyMode: "hold",
		Transcription: TranscriptionConfig{
			BaseURL:      "http://localhost:13305/api/v1",
			Model:        "gemma4-it-e2b-FLM",
			PromptHint:   DefaultPromptHint,
			RequestLimit: 45,
			HallucinationFilters: []string{
				"Thank you.",
				"Thanks.",
				"Thanks for watching.",
				"Thanks for watching!",
				"Please subscribe.",
				"Bye.",
				"you",
				".",
			},
			// 28s holds a 2s margin under the documented 30s cap so
			// rounding/header overhead can't trip the limit.
			ChunkMaxSeconds: 28,
			// 2 prior turns matches the user-tested payload. Adds
			// ~2.6 MB worst-case base64 to a request body — large
			// but well under any sane HTTP body limit.
			HistoryTurns: 2,
			Prompt:       DefaultChatAudioPrompt,
			Language:     "its original language",
			Stream:       true,
			ContextMode:  ChatAudioContextFewShot,
			// Energy gate matching recall's defaults. Rejects fan
			// hum / room tone but keeps quiet speech.
			MinChunkPeak: 0.02,
			MinChunkRMS:  0.005,
			BatchPrompt:  DefaultBatchPrompt,
			// 0 = auto: query Lemonade /api/v1/health for the loaded
			// model's recipe_options.ctx_size and compute a safe
			// audio-seconds budget from it (Gemma audio costs 6.25
			// tokens/s per the USM encoder spec). Override with a
			// positive number to pin the budget.
			BatchMaxAudioSeconds: 0,
			Silero: SileroConfig{
				// $HOME/opt/onnxruntime/lib/... is where the
				// onnxruntime release tarball lands when unpacked
				// into ~/opt — the documented install location.
				// resolveOnnxruntimeLibrary expands $HOME at runtime
				// so this is portable across machines; users who
				// installed system-wide can blank the field to fall
				// back to the auto-discovery candidates.
				OnnxruntimeLibrary: "$HOME/opt/onnxruntime/lib/libonnxruntime.so",
				SilenceMS:          500,
				SpeechMS:           150,
				MinUtteranceMS:     1000,
			},
		},
		Recording: RecordingConfig{
			Backend:            "auto",
			Device:             "default",
			SampleRate:         16000,
			Channels:           1,
			MaxDurationSeconds: 120,
			DuckVolume:         0.1,
		},
		Insertion: InsertionConfig{
			Mode:             "auto",
			DefaultPasteKey:  "ctrl+v",
			TerminalPasteKey: "ctrl+shift+v",
			RestoreClipboard: true,
			TerminalClasses: []string{
				"Alacritty",
				"kitty",
				"org.wezfurlong.wezterm",
				"WezTerm",
				"XTerm",
				"Gnome-terminal",
				"gnome-terminal-server",
				"code",
				"Cursor",
			},
			KittyRemoteControl: true,
			KittyVerifyPaste:   true,
		},
		Overlay: OverlayConfig{
			Width:          620,
			Height:         132,
			MarginTop:      44,
			Opacity:        0.94,
			AutoHideMillis: 1800,
			Branding:       "Vocis",
			Ready: OverlayReady{
				Title:    "Ready",
				Subtitle: "Voice typing is armed",
			},
			Listening: OverlayListen{
				Title:        "Listening",
				Suffix:       "— release to paste",
				SubmitHint:   "⏎ submit",
				Connecting:   "○ Connecting...",
				Reconnecting: "○ Reconnecting... (attempt {attempt}/{max})",
				Connected:    "● Ready to type into {window}",
				LoadingModel: "○ Loading {model}...",
			},
			Finishing: OverlayFinish{
				Title:      "Finishing",
				CancelHint: "— press {shortcut} to cancel",
				WrappingUp: "Wrapping up",
				PPWait:     "Wait",
				PPStream:   "Stream",
				PhaseDone:  "done",
			},
			Success: OverlaySuccess{
				Title:    "Typed",
				Subtitle: "Transcription inserted into your active app",
			},
			Error: OverlayError{
				Title: "Error",
			},
			Warning: OverlayWarning{
				Title:              "Heads up",
				NoSpeech:           "No speech detected",
				Cancelled:          "Cancelled — transcription discarded",
				PostprocessSkipped: "Raw text pasted — cleanup was skipped due to a timeout or error",
				TargetGone:         "Target window closed — transcript copied to clipboard",
			},
		},
		PostProcess: PostProcessConfig{
			Enabled: true,
			// Same model as transcription. On the chat-audio backend the
			// postprocess prompt is folded into the chat-audio system
			// message and the separate /chat/completions call is skipped
			// (see app.startRecordingLocked). On the realtime-WS backend
			// they're separate calls but both go to gemma — Lemonade's
			// llm slot only fits one model, so reusing the transcription
			// model avoids a 5-10 s slot swap on every dictation.
			Model:                "gemma4-it-e2b-FLM",
			Prompt:               DefaultPostProcessPrompt,
			MinWordCount:         10,
			FirstTokenTimeoutSec: 10,
			TotalTimeoutSec:      15,
			Temperature:          floatPtr(0.2),
		},
		Telemetry: TelemetryConfig{
			Enabled:  false,
			Endpoint: "localhost:4317",
		},
		Recall: RecallConfig{
			// 7 days of retention is a sensible "long history" default for
			// an always-on recall. Segments are small (~100-400 KB each);
			// 7 days at typical speaking pace stays well under 1 GB even
			// with persist.mode=disk. Set to 0 for unbounded time.
			RetentionSeconds:  604800,
			// 2000 keeps disk under ~800 MB worst-case. Set to 0 for
			// unbounded count (only the time bound applies).
			MaxSegments:       2000,
			SocketPath:        "",
			MinSilenceMS:      500,
			MinSpeechMS:       150,
			MinUtteranceMS:    500,
			PrerollMS:         300,
			MaxSegmentSeconds: 30,
			MinSegmentPeak:    0.02,
			MinSegmentRMS:     0.005,
			Persist: RecallPersistConfig{
				Mode: RecallPersistMemory,
				Dir:  defaultRecallStateDir(),
			},
		},
		Speak: SpeakConfig{
			// Empty BaseURL means "inherit transcription.base_url" at
			// load time — most users run a single local Lemonade and
			// don't want to repeat the URL. kokoro-v1 is the only TTS
			// model Lemonade ships as of 10.2.0 (per docs/lemonade.md).
			// shimmer is a known-working voice id; users can pick
			// others from the Kokoro model card and override here.
			BaseURL: "",
			Model:   "kokoro-v1",
			Voice:   "shimmer",
		},
	}
}

func floatPtr(v float64) *float64 { return &v }

// defaultRecallStateDir returns the default for `recall.persist.dir`.
// Emits the portable `$HOME/.local/state/vocis/recall` form rather
// than a machine-specific absolute path so generated configs travel
// between machines (laptop → desktop, home dir moves, etc.). The
// FilePersister expands $HOME and ~/ at open-time, so this works
// without any load-time transform.
func defaultRecallStateDir() string {
	return "$HOME/.local/state/vocis/recall"
}

func Path() (string, error) {
	if env := strings.TrimSpace(os.Getenv("VOCIS_CONFIG")); env != "" {
		return env, nil
	}

	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "vocis", fileName), nil
}

func Load() (Config, string, error) {
	path, err := Path()
	if err != nil {
		return Config{}, "", err
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := Save(path, Default()); err != nil {
			return Config{}, "", err
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, "", err
	}

	if err := rejectDeprecatedKeys(path, data); err != nil {
		return Config{}, "", err
	}
	data = stripRetiredKeys(data)

	cfg := Default()
	if err := decodeStrict(data, &cfg); err != nil {
		return Config{}, "", fmt.Errorf("decode %s: %w", path, err)
	}

	return cfg, path, cfg.Validate()
}

// decodeStrict unmarshals YAML into cfg with KnownFields(true), so any key
// that doesn't map to a struct field makes Load fail. This prevents silent
// drift: stale fields left in a user's config after a rename/removal will
// surface as a startup error demanding the user clean them up instead of
// being silently ignored.
func decodeStrict(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// retiredKeys lists nested config keys that vocis used to support and
// has since removed. The strict KnownFields decoder would otherwise
// reject a user's existing config when these keys are still present —
// stripRetiredKeys silently drops them with a deprecation log so old
// configs keep loading after a release that removes a knob.
//
// Format: "section.key" or "section.subsection.key". Top-level
// renames (whole sections) live in rejectDeprecatedKeys instead so we
// can give a clear "rename X to Y" error.
var retiredKeys = []struct{ path, since, reason string }{
	{"transcription.organization", "post-OpenAI-removal", "OpenAI realtime backend was removed; org/project headers no longer apply"},
	{"transcription.project", "post-OpenAI-removal", "OpenAI realtime backend was removed; org/project headers no longer apply"},
	{"transcription.realtime_url", "post-WS-removal", "realtime WebSocket backend was removed; chat-audio uses base_url only"},
	{"streaming.noise_reduction", "config-cull", "Lemonade ignored this field; it was dead config"},
	{"streaming.manual_commit", "post-WS-removal", "realtime WebSocket backend was removed; chat-audio always finalizes locally"},
	{"streaming.client_vad", "post-WS-removal", "realtime WebSocket backend was removed; chat-audio always runs Silero VAD"},
	{"streaming.show_partial_overlay", "post-WS-removal", "realtime WebSocket backend was removed; chat-audio partials always render"},
	{"streaming.threshold", "post-WS-removal", "realtime WebSocket backend was removed; Silero hysteresis replaces the energy threshold"},
	{"streaming.wait_final_seconds", "post-WS-removal", "realtime WebSocket backend was removed; chat-audio has no post-commit wait"},
	{"streaming.tail_silence_ms", "post-WS-removal", "realtime WebSocket backend was removed; chat-audio does not pad a WS audio buffer"},
	{"streaming.prefix_padding_ms", "streaming-fold", "moved to transcription.silero.speech_ms"},
	{"streaming.silence_duration_ms", "streaming-fold", "moved to transcription.silero.silence_ms"},
	{"streaming.min_utterance_ms", "streaming-fold", "moved to transcription.silero.min_utterance_ms"},
	{"streaming.onnxruntime_library", "streaming-fold", "moved to transcription.silero.onnxruntime_library"},
	{"transcription.backend", "post-WS-removal", "only one backend remains; transcription.backend is no longer read"},
	{"yaml_indent", "config-cull", "self-output indentation is now hardcoded to 2"},
	{"postprocess.min_p", "config-cull", "rarely-tuned sampler knob"},
	{"postprocess.frequency_penalty", "config-cull", "rarely-tuned sampler knob"},
	{"postprocess.presence_penalty", "config-cull", "rarely-tuned sampler knob"},
	{"postprocess.repetition_penalty", "config-cull", "rarely-tuned sampler knob"},
	{"postprocess.stop", "config-cull", "rarely-tuned sampler knob"},
	{"recall.batch_gap_ms", "one-shot-batch", "`recall last` no longer concatenates PCM; each segment is sent as its own input_audio part"},
	{"recall.batch_max_seconds", "one-shot-batch", "`recall last` no longer concatenates PCM; total audio is now bounded by Gemma's context window"},
}

// retiredSections lists whole YAML sections (top-level or nested via
// dotted path) that vocis used to consume and now no longer reads at
// all. Stripped wholesale after per-key removal so an old config
// carrying e.g. `streaming:` with any combination of subkeys is
// silently dropped instead of failing the strict decoder.
//
// `transcription.chat_audio` here is special-cased: dropping the whole
// nested block would silently lose every chat-audio knob the user had
// configured, so loadFailsOnRetiredNested below errors out with a
// migration message instead. The entry stays in this slice for
// documentation/discoverability — actual handling lives in that helper.
var retiredSections = []struct{ path, since, reason string }{
	{"streaming", "streaming-fold", "streaming: was folded into transcription.silero (silence_ms / speech_ms / min_utterance_ms / onnxruntime_library)"},
	{"transcription.chat_audio", "chat-audio-flatten", "every field on transcription.chat_audio.* was hoisted to transcription.* directly (chunk_max_seconds, history_turns, prompt, language, stream, context_mode, min_chunk_peak, min_chunk_rms, batch_prompt, batch_max_audio_seconds, ctx_size, batch_until_release, continuation_rebatch, silero)"},
}

// stripRetiredKeys parses the YAML, walks the retiredKeys list, and
// returns a copy with any matches removed. Logs a one-time
// deprecation notice per removed key so users notice their config is
// drifting from current shape. Whole sections from retiredSections
// are then removed in a second pass. retiredSections supports
// dotted paths (e.g. "transcription.chat_audio") so nested blocks
// can be retired the same way as top-level ones.
func stripRetiredKeys(data []byte) []byte {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return data
	}
	changed := false
	for _, k := range retiredKeys {
		if removeNestedKey(&doc, strings.Split(k.path, ".")) {
			changed = true
			sessionlog.Warnf("config: dropping retired key %q (removed %s — %s)", k.path, k.since, k.reason)
		}
	}
	for _, s := range retiredSections {
		if removeNestedKey(&doc, strings.Split(s.path, ".")) {
			changed = true
			sessionlog.Warnf("config: dropping retired section %q (removed %s — %s)", s.path, s.since, s.reason)
		}
	}
	if !changed {
		return data
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return data
	}
	return out
}

// removeNestedKey walks a yaml document tree and removes the leaf at
// the given key path. Returns true if a removal happened.
func removeNestedKey(node *yaml.Node, path []string) bool {
	if node == nil || len(path) == 0 {
		return false
	}
	target := node
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		target = node.Content[0]
	}
	if target.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i < len(target.Content); i += 2 {
		key := target.Content[i]
		val := target.Content[i+1]
		if key.Value != path[0] {
			continue
		}
		if len(path) == 1 {
			target.Content = append(target.Content[:i], target.Content[i+2:]...)
			return true
		}
		return removeNestedKey(val, path[1:])
	}
	return false
}

// rejectDeprecatedKeys fails loudly on config files that still use
// pre-rename top-level keys. Strict by design: silently accepting the
// old shape splits users across two spellings and hides the fact that
// the section no longer describes OpenAI specifically.
//
// Also catches `transcription.chat_audio:` — every chat-audio knob
// was hoisted up to transcription.* directly, and silently dropping
// the nested block (the default retiredSections behavior) would lose
// the user's tunings. Surface a clear migration message instead so
// users see exactly what to change.
func rejectDeprecatedKeys(path string, data []byte) error {
	var raw map[string]yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		// Bad YAML will surface on the real unmarshal below with a
		// better error — don't preempt it here.
		return nil
	}
	if _, ok := raw["openai"]; ok {
		return fmt.Errorf(
			"%s: top-level key `openai:` was renamed to `transcription:`. "+
				"Rename the section in your config and try again.",
			path,
		)
	}
	if transcription, ok := raw["transcription"]; ok && transcription.Kind == yaml.MappingNode {
		for i := 0; i < len(transcription.Content); i += 2 {
			if transcription.Content[i].Value == "chat_audio" {
				return fmt.Errorf(
					"%s: nested `transcription.chat_audio:` block was retired — every field "+
						"(chunk_max_seconds, history_turns, prompt, language, stream, context_mode, "+
						"min_chunk_peak, min_chunk_rms, batch_prompt, batch_max_audio_seconds, ctx_size, "+
						"batch_until_release, continuation_rebatch, silero) is now a direct property "+
						"of `transcription:`. Hoist the keys up one level (remove the `chat_audio:` "+
						"indentation) and try again.",
					path,
				)
			}
		}
	}
	return nil
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Hotkey) == "" {
		return errors.New("hotkey must not be empty")
	}

	if strings.TrimSpace(c.Transcription.Model) == "" {
		return errors.New("transcription.model must not be empty")
	}

	if c.Transcription.ChunkMaxSeconds < 1 || c.Transcription.ChunkMaxSeconds > 30 {
		return errors.New("transcription.chunk_max_seconds must be between 1 and 30")
	}
	if c.Transcription.HistoryTurns < 0 || c.Transcription.HistoryTurns > 8 {
		return errors.New("transcription.history_turns must be between 0 and 8")
	}
	if strings.TrimSpace(c.Transcription.Prompt) == "" {
		return errors.New("transcription.prompt must not be empty")
	}
	if strings.TrimSpace(c.Transcription.Language) == "" {
		return errors.New("transcription.language must not be empty")
	}
	switch c.Transcription.ContextMode {
	case "", ChatAudioContextFewShot, ChatAudioContextInlineClips:
	default:
		return fmt.Errorf(
			"transcription.context_mode must be %q or %q",
			ChatAudioContextFewShot, ChatAudioContextInlineClips,
		)
	}
	if c.Transcription.MinChunkPeak < 0 || c.Transcription.MinChunkPeak > 1 {
		return errors.New("transcription.min_chunk_peak must be between 0 and 1")
	}
	if c.Transcription.MinChunkRMS < 0 || c.Transcription.MinChunkRMS > 1 {
		return errors.New("transcription.min_chunk_rms must be between 0 and 1")
	}
	if strings.TrimSpace(c.Transcription.BatchPrompt) == "" {
		return errors.New("transcription.batch_prompt must not be empty")
	}
	if c.Transcription.BatchMaxAudioSeconds < 0 || c.Transcription.BatchMaxAudioSeconds > 600 {
		return errors.New("transcription.batch_max_audio_seconds must be between 0 and 600")
	}
	if c.Transcription.CtxSize < 0 || c.Transcription.CtxSize > 1048576 {
		return errors.New("transcription.ctx_size must be between 0 and 1048576 (0 = leave Lemonade's default)")
	}
	if c.Transcription.BatchUntilRelease && c.Transcription.ContinuationRebatch {
		return errors.New("transcription.batch_until_release and continuation_rebatch are mutually exclusive (batch_until_release already sends one POST per utterance, so there's nothing to rebatch)")
	}
	if c.Transcription.Silero.SilenceMS < 0 || c.Transcription.Silero.SilenceMS > 5000 {
		return errors.New("transcription.silero.silence_ms must be between 0 and 5000")
	}
	if c.Transcription.Silero.SpeechMS < 0 || c.Transcription.Silero.SpeechMS > 2000 {
		return errors.New("transcription.silero.speech_ms must be between 0 and 2000")
	}
	if c.Transcription.Silero.MinUtteranceMS < 0 || c.Transcription.Silero.MinUtteranceMS > 10000 {
		return errors.New("transcription.silero.min_utterance_ms must be between 0 and 10000")
	}

	switch c.HotkeyMode {
	case "hold", "toggle":
	default:
		return fmt.Errorf("hotkey_mode must be hold or toggle")
	}

	if c.Transcription.RequestLimit < 0 || c.Transcription.RequestLimit > 300 {
		return errors.New("transcription.request_timeout_seconds must be between 0 (disabled) and 300")
	}

	switch c.Insertion.Mode {
	case "auto", "clipboard", "type":
	default:
		return fmt.Errorf("insertion.mode must be auto, clipboard, or type")
	}

	if c.Recording.SampleRate <= 0 || c.Recording.SampleRate > 48000 {
		return errors.New("recording.sample_rate must be between 1 and 48000")
	}

	if c.Recording.Channels <= 0 || c.Recording.Channels > 2 {
		return errors.New("recording.channels must be 1 or 2")
	}

	if c.Recording.MaxDurationSeconds < 0 || c.Recording.MaxDurationSeconds > 600 {
		return errors.New("recording.max_duration_seconds must be between 0 and 600")
	}

	switch c.Recording.Backend {
	case "", "auto", "pulse":
	default:
		return fmt.Errorf("recording.backend must be auto or pulse")
	}


	if c.Overlay.Width < 200 || c.Overlay.Height < 80 {
		return errors.New("overlay dimensions are too small")
	}

	// 30-day ceiling on retention: keeps the validation bound sane
	// while leaving headroom for "long history" setups. At typical
	// conversational pace persisted to disk, 30 days easily exceeds
	// the max_segments ceiling below — the count bound kicks in first.
	if c.Recall.RetentionSeconds < 0 || c.Recall.RetentionSeconds > 30*86400 {
		return errors.New("recall.retention_seconds must be between 0 and 2592000 (30 days)")
	}
	if c.Recall.MaxSegments < 0 || c.Recall.MaxSegments > 10000 {
		return errors.New("recall.max_segments must be between 0 and 10000")
	}
	if c.Recall.RetentionSeconds == 0 && c.Recall.MaxSegments == 0 {
		return errors.New("recall: at least one of retention_seconds or max_segments must be > 0")
	}
	if c.Recall.MinSilenceMS < 0 || c.Recall.MinSilenceMS > 5000 {
		return errors.New("recall.min_silence_ms must be between 0 and 5000")
	}
	if c.Recall.MinSpeechMS < 0 || c.Recall.MinSpeechMS > 5000 {
		return errors.New("recall.min_speech_ms must be between 0 and 5000")
	}
	if c.Recall.MinUtteranceMS < 0 || c.Recall.MinUtteranceMS > 10000 {
		return errors.New("recall.min_utterance_ms must be between 0 and 10000")
	}
	if c.Recall.PrerollMS < 0 || c.Recall.PrerollMS > 5000 {
		return errors.New("recall.preroll_ms must be between 0 and 5000")
	}
	if c.Recall.MaxSegmentSeconds < 1 || c.Recall.MaxSegmentSeconds > 300 {
		return errors.New("recall.max_segment_seconds must be between 1 and 300")
	}
	if c.Recall.MinSegmentPeak < 0 || c.Recall.MinSegmentPeak > 1 {
		return errors.New("recall.min_segment_peak must be between 0 and 1")
	}
	if c.Recall.MinSegmentRMS < 0 || c.Recall.MinSegmentRMS > 1 {
		return errors.New("recall.min_segment_rms must be between 0 and 1")
	}
	switch c.Recall.Persist.Mode {
	case "", RecallPersistMemory, RecallPersistDisk:
	default:
		return fmt.Errorf("recall.persist.mode must be %q or %q", RecallPersistMemory, RecallPersistDisk)
	}
	if c.Recall.Persist.Mode == RecallPersistDisk && strings.TrimSpace(c.Recall.Persist.Dir) == "" {
		return errors.New("recall.persist.mode=disk requires recall.persist.dir to be set")
	}

	if strings.TrimSpace(c.Speak.Model) == "" {
		return errors.New("speak.model must not be empty")
	}
	if strings.TrimSpace(c.Speak.Voice) == "" {
		return errors.New("speak.voice must not be empty")
	}

	if err := c.PostProcess.validate(); err != nil {
		return err
	}

	c.validateOverlayTemplates()

	return nil
}

func (p PostProcessConfig) validate() error {
	if p.Temperature != nil && (*p.Temperature < 0 || *p.Temperature > 2) {
		return errors.New("postprocess.temperature must be between 0 and 2")
	}
	if p.TopP != nil && (*p.TopP <= 0 || *p.TopP > 1) {
		return errors.New("postprocess.top_p must be between 0 (exclusive) and 1")
	}
	return nil
}

func (c Config) validateOverlayTemplates() {
	templates := []struct {
		key      string
		template string
		expected []string
	}{
		{"overlay.listening.connected", c.Overlay.Listening.Connected, []string{"window"}},
		{"overlay.listening.reconnecting", c.Overlay.Listening.Reconnecting, []string{"attempt", "max"}},
		{"overlay.finishing.cancel_hint", c.Overlay.Finishing.CancelHint, []string{"shortcut"}},
	}
	for _, tt := range templates {
		for _, w := range ValidateTemplate(tt.template, tt.expected) {
			sessionlog.Warnf("config %s: %s", tt.key, w)
		}
	}
}
