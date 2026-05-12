# Silero VAD

`vocis` runs [Silero VAD](https://github.com/snakers4/silero-vad) as
its voice-activity detector in two places:

- **Client VAD during a `serve` session** (optional): chunks a long
  hotkey-hold into multiple segments in manual-commit mode without
  waiting for server-side VAD. Enabled with
  `streaming.client_vad: true` when `streaming.manual_commit: true`.
  See [docs/lemonade.md](lemonade.md) for the surrounding realtime
  protocol.
- **Capture segmentation in `recall` mode** (always): the recall
  daemon has no server to lean on, so Silero draws the boundaries
  between utterances in the always-on stream.

Wrapper lives in
[`internal/transcribe/silero.go`](../internal/transcribe/silero.go).
Both call sites share the same `SileroVAD` type — one instance per
session, with its own hidden state and hysteresis.

## Why Silero

- **Tiny.** Model is ~2 MB, embedded via `go:embed` — no download or
  path config at runtime.
- **Self-contained.** Runs on CPU via
  [onnxruntime_go](https://github.com/yalue/onnxruntime_go). The only
  external dependency is `libonnxruntime.so`, which vocis
  auto-discovers under common install locations.
- **Fast.** Single-threaded inference on a 576-sample window
  completes in well under 1 ms on modern x86.
- **Accurate enough.** Clean speech scores consistently above 0.9;
  quiet rooms consistently below 0.1. The failure modes are in the
  margins — see *Tuning the hysteresis* below.

## How it's driven

Audio flows in as int16 chunks from the recorder (variable size per
chunk; recorder hands off whatever PulseAudio gives us). `SileroVAD.Feed`
buffers samples until it has 512 (32 ms at 16 kHz), then runs one
inference per window. Each window produces:

- a **probability** in `[0, 1]` — the model's confidence the window
  contains speech
- an **event**: `VADNone`, `VADSpeechStarted`, or `VADSpeechStopped`
  (the hysteresis layer's output, not Silero's raw score)

Feed works out to one inference per 32 ms of audio regardless of
chunk boundaries. The 64-sample "context" window (the tail of the
previous window prepended to the next one) is important — feeding
raw 512-sample chunks with no context makes Silero classify
everything as silence. Confirmed against the reference implementation.

## Two-threshold hysteresis

Silero's per-frame score is reliable at the extremes but noisy in the
0.4–0.6 band. If our state machine only used one threshold at 0.5, a
single frame scoring 0.51 every ~500 ms would be enough to keep a
segment open forever on low-level ambient noise (fans, HVAC,
background traffic). We saw exactly that: 30 s force-flushed segments
full of silence that Whisper then hallucinated "Thank you." on.

The reference
[snakers4/silero-vad](https://github.com/snakers4/silero-vad) uses
**two** thresholds, and we follow suit:

| Band | Condition | Effect |
|------|-----------|--------|
| Speech | `prob >= 0.5` | count as speech frame, reset silence counter |
| Silence | `prob < 0.35` | count as silence frame, tick silence counter |
| Ambiguous | `0.35 <= prob < 0.5` | hold state — do not open a new episode, do not reset silence |

The ambiguous band is the key. Once a speech episode is winding down
and the silence counter is ticking up, occasional frames at 0.45
won't reset it. The segment gets a chance to actually close.

Thresholds are compile-time constants in `silero.go`. The values
`0.5` / `0.35` match the reference implementation; changing them is
discouraged unless you're debugging specific environments.

## State machine fields

Per-instance state in `SileroVAD`:

- `speechMs` — accumulated speech time since the episode started.
  Reset when a silence event fires.
- `silenceMs` — accumulated silence time since the last speech frame.
  Reset by any speech-band frame.
- `inSpeech` — current episode status.
- Hysteresis thresholds: `minSpeechMs`, `minSilenceMs`,
  `minUtteranceMs` — passed at construction (see *Config knobs* below).

The ONNX hidden state (the model's internal LSTM carry) is **kept**
across segment boundaries. That's intentional — it represents the
recent acoustic history, not the hysteresis state, and resetting it
would drop VAD accuracy briefly after every utterance. Only the
hysteresis fields reset on episode end.

## Hysteresis knobs

Pinned as Go consts in each consumer package (used to be YAML knobs
under `transcription.silero.*` and `recall.*` — nobody tuned them in
practice). The values differ between the two consumers because they
serve different goals.

### `serve` chat-audio chunker (`internal/transcribe/chat_audio.go`)

- `defaultSileroSilenceMS` = **500**
- `defaultSileroSpeechMS` = **150**
- `defaultSileroMinUtteranceMS` = **1000**

### `recall` daemon (`internal/recall/daemon.go`)

- `defaultMinSilenceMS` = **500**
- `defaultMinSpeechMS` = **150**
- `defaultMinUtteranceMS` = **500**
- `defaultPrerollMS` = **300** — extra audio kept *before* the
  declared speech-start so word onsets aren't clipped

Recall uses a tighter `defaultMinUtteranceMS` than serve because it's
always-on and short utterances are still worth capturing. The preroll
is recall-specific.

## Single-threaded ONNX runtime

Both `intra_op_num_threads` and `inter_op_num_threads` are set to
**1** at session-creation time. ONNX Runtime's default is
`num_physical_cores` for each — which for a 2 MB model finishing in
under a millisecond means every 32 ms window pings N cores to do
microseconds of real work. On an otherwise-idle recall daemon that
surfaces as several cores pegged near 100%. Single-threaded is both
faster wall-clock and cooler.

## Defense in depth for recall

Silero + correct hysteresis will still occasionally mislabel noise
as speech (it's a 2 MB model classifying 32 ms windows — perfection
isn't on the table). Recall layers two post-capture filters to catch
what VAD misses:

- `defaultMinSegmentPeak` = **0.02** — drops segments whose peak
  absolute sample level is below the noise floor.
- `defaultMinSegmentRMS` = **0.005** — drops segments whose RMS
  energy (`sqrt(mean(sample²))/32768`) is too low. This is the one
  that catches 24 s of silence with one keyboard clack: peak is
  bumped up by the click, but RMS across the whole segment stays
  tiny. Real quiet speech has RMS roughly 10× higher.

Both decisions are logged at INFO level and recorded on the
`vocis.recall.capture` span as `segment.dropped_as_silence` /
`segment.drop_reason`. The values are pinned in
`internal/recall/daemon.go`; edit and rebuild to change them.

## Diagnosing stuck-in-speech segments

If you see suspicious long segments in `vocis recall pick` — especially
at 30 s with low `peak` / `rms` — the usual culprits, in order of
likelihood:

1. **VAD hit the ambiguous band more than you'd expect.** Real fans
   / HVAC / distant voices sometimes score in the 0.35–0.5 band for
   long stretches. The ambiguous band holds state, so once you're
   in-speech, staying in-speech can still happen if the ambiguous
   frames never dip below 0.35. Increase `defaultMinSilenceMS` (in
   `internal/recall/daemon.go`) to require a longer clean silence
   before closing.

2. **Hardware noise crossing 0.5.** Loud fans, noisy gain settings,
   phantom power issues. Silero scoring >0.5 on what you hear as
   silence means there's real speech-like energy in the signal.
   Check your mic gain and mute the input when not using recall.

3. **Room speech from elsewhere.** Background conversation, TV
   audio, or your own side of a phone call. Silero is doing the
   right thing here — it just can't tell it's not *your* speech.

Use `vocis recall replay --ids <n>` to hear the segment yourself.
That usually makes the cause obvious within a couple seconds.

If you want per-window probability diagnostics, `SileroVAD.Snapshot()`
returns session-wide min/max prob — but that's currently only
surfaced by client VAD debug logging. Adding per-segment prob stats
to the `vocis.recall.capture` span is a natural next step when
needed.
