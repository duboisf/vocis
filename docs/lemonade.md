# Lemonade API notes

[Lemonade Server](https://github.com/lemonade-sdk/lemonade) exposes an
OpenAI-compatible REST API plus a realtime-transcription WebSocket on a
separate port. This doc captures what vocis actually relies on — defaults,
quirks, and places the API diverges from OpenAI's.

Everything here was learned empirically against Lemonade 10.3.0 running
locally; later versions may differ. Refresh by running the probe
(`go run ./scripts/lemonade-probe`) or hitting `/health` directly.

## Required version: 10.3.0+

vocis assumes Lemonade Server **10.3.0** or newer. Two protocol /
classification changes between 10.2 and 10.3 are load-bearing:

### `input_audio_buffer.cleared` after a finalize-time commit

In 10.2, vocis sent `input_audio_buffer.commit` at finalize and got
back `input_audio_buffer.committed` plus a (frequently empty)
`conversation.item.input_audio_transcription.completed` for the
trailing audio. In 10.3, when client/server VAD has already drained
every speech segment mid-stream, the server replies with
`input_audio_buffer.cleared` instead — meaning "the buffer is empty,
no transcription incoming." vocis treats `cleared` as "trailing
skipped, finalize immediately"; without that recognition the
`wait_final` block sits out the full
`streaming.wait_final_seconds` (15 s default) before pasting.

When the buffer still has un-transcribed audio at commit time (e.g.
the user kept speaking past `speech_stopped`), 10.3 still returns
the old `committed` + final `transcription.completed` pair. So the
`cleared` path is conditional, not unconditional — both code paths
matter.

Implementation in
[`internal/transcribe/transcribe.go`](../internal/transcribe/transcribe.go):
the WS read loop emits a `StreamEventTrailingSkipped` when `cleared`
arrives after `Stats.CommitAt` is non-zero, and the dictation
session pushes an empty result on `s.finals` (only when
`liveSegments=false`, i.e. we're past Finalize) so `waitForFinal`
returns immediately. The wait_final span gets a
`realtime.buffer_cleared` event so Jaeger triage shows why finalize
was so fast.

Upstream change:
[lemonade-sdk/lemonade#1739](https://github.com/lemonade-sdk/lemonade/pull/1739)
"Fix realtime stop commit after VAD final" (merged 2026-04-27,
shipped in 10.3.0).

### `gemma4-it-e2b-FLM` reclassified `audio` → `llm`

In 10.2, `gemma4-it-e2b-FLM` was loaded as `type=audio` and served
realtime transcription. In 10.3 it's `type=llm` — `/chat/completions`
works (good for `postprocess.model` on NPU), but
`/audio/transcriptions` rejects with
`"Audio transcription not supported by FLM llm model"`. The model's
labels still include `audio` and `transcription`, but Lemonade's
endpoint router now ignores those for FLM-llm builds.

The realtime WS still *accepts* a `session.update` naming
gemma4-it-e2b-FLM but silently emits empty `delta` / empty
`transcript` events — the user sees vocis "appear to work" (overlay
listening, mic level animating) but no text gets pasted. To turn
that silent failure into a clear startup error, vocis runs a
**preflight label guard** in
[`EnsureTranscribeModelLoaded`](../internal/transcribe/warm_transcription.go):
it fetches `/api/v1/models`, looks up the configured
`transcription.model`, and refuses to start a dictation when the
catalog entry doesn't carry the `transcription` label. Models that
aren't in the catalog (user-pulled customs) skip the check with a
debug log — we can't validate what we can't see.

### Older versions

vocis may still work against pre-10.3 Lemonade for basic
transcription, but every dictation that fully drains the VAD buffer
mid-stream will pay the full `wait_final` timeout at finalize. The
`cleared` short-circuit is a no-op when the event never arrives, so
nothing breaks — it's just slow. Upgrading to 10.3 is the fix.

## Port layout

| Port   | Protocol | Purpose                                    |
| ------ | -------- | ------------------------------------------ |
| 13305  | HTTP     | REST API: models, chat, health, audio HTTP |
| 9000   | WS       | Realtime transcription                     |

The WS port is not fixed — `/health` returns the current `websocket_port`.
`vocis config backend` probes health and writes both URLs into the config.

**Base URL convention:** `http://localhost:13305/api/v1` (the `/api/v1`
prefix is required; there is no unversioned root).

## `/health`

```
GET http://localhost:13305/api/v1/health
```

Returns a JSON document describing the running Lemonade instance. Fields
vocis cares about:

- `status` — `"ok"` when Lemonade is ready to serve requests.
- `version` — e.g. `"10.2.0"`. Use this when filing bugs.
- `websocket_port` — the port the realtime WS is bound to (9000 by
  default). Prefer this over hardcoding.
- `model_loaded` — the last model that was loaded. Not reliable for "is
  X ready" because a request can swap it out.
- `all_models_loaded[]` — one entry per loaded model. Each has
  `model_name`, `type` (`audio`/`llm`/`tts`/`embedding`/…), `device`
  (`npu`/`cpu`), `recipe` (`flm`/`kokoro`/`llamacpp`/…), and a per-model
  `backend_url` pointing at the sub-server Lemonade is proxying.
- `max_models` — slot counts per model type. **Crucially `llm: 1`,
  `audio: 1`, `tts: 1`.** Only one model of each type can be resident.
  Switching models triggers an unload + load (~5s latency spike).

## `/models`

```
GET http://localhost:13305/api/v1/models             # downloaded models only
GET http://localhost:13305/api/v1/models?show_all=true  # full registry
```

Without `show_all`, Lemonade only reports models that are already
downloaded on disk. Useful for "what can I use right now"; use
`show_all=true` when building a picker that needs to show downloadable
options (`vocis config models` does this).

Per-model fields worth knowing:

- `id` — the name you pass as `model` in other endpoints (e.g.
  `whisper-v3-turbo-FLM`).
- `labels[]` — capability tags. Observed values: `tts`, `speech`,
  `transcription`, `embeddings`, `reranking`, `image`, `esrgan`,
  `vision`, `reasoning`. Use labels to filter pickers: transcription
  candidates have `transcription`; post-processing candidates are LLMs
  and typically carry `reasoning` or no audio/image/embedding labels.
- `composition.recipe` / top-level `checkpoint` — describes the backend
  recipe Lemonade uses to run the model:
  - `flm` — [FastFlowLM](https://github.com/FastFlowLM/FastFlowLM),
    NPU-accelerated (`device: npu`). Fast on Ryzen AI hardware.
  - `llamacpp` — CPU-only llama.cpp recipe.
  - `kokoro` — [Kokoro TTS](https://github.com/hexgrad/kokoro),
    CPU-only.
  - `whisper.cpp` — CPU Whisper for transcription fallback.
- `checkpoints.npu_cache` — present when the model has an NPU-optimized
  variant (typical for FLM recipes).

## Realtime transcription

```
ws://localhost:9000/realtime?model=<model_id>
```

The WS endpoint implements a subset of OpenAI's realtime transcription
protocol. The model is passed as a query parameter *and* can be set via
`session.update` — vocis sets both; they should agree.

### Protocol flow

1. Client opens WS.
2. Server emits `session.created`, then `session.updated` once.
3. Client sends `session.update` with model + `turn_detection` config.
4. Client streams `input_audio_buffer.append` with base64 PCM16 chunks.
   Lemonade expects 16 kHz mono PCM16 LE.
5. Server emits `input_audio_buffer.speech_started` /
   `input_audio_buffer.speech_stopped` as VAD triggers.
6. Client sends `input_audio_buffer.commit` (or relies on VAD to
   auto-commit; see below).
7. Server emits one or more
   `conversation.item.input_audio_transcription.delta` messages as the
   Whisper engine produces partial text.
8. Server emits **one**
   `conversation.item.input_audio_transcription.completed` with the
   canonical transcript.

### `session.update` shape (honored)

```json
{
  "type": "session.update",
  "session": {
    "model": "whisper-v3-turbo-FLM",
    "turn_detection": {
      "threshold": 0.02,
      "silence_duration_ms": 500,
      "prefix_padding_ms": 300
    }
  }
}
```

Observations:

- Only `model` and `turn_detection` are honored. Other OpenAI realtime
  session fields (response modalities, voice, tools) are silently
  ignored.
- `turn_detection: null` disables server VAD and the interim
  transcription work that otherwise runs in parallel with commit
  ([lemonade #1607](https://github.com/lemonade-sdk/lemonade/pull/1607)).
  vocis opts into this via `streaming.manual_commit: true`. In this
  mode Lemonade buffers audio until the client sends
  `input_audio_buffer.commit` and never emits `speech_started`,
  `speech_stopped`, or delta events — so `streaming.show_partial_overlay`
  must be false (config validation enforces this).
- To still chunk a long hold into multiple segments in manual-commit
  mode, enable `streaming.client_vad: true`. vocis then runs Silero
  VAD client-side via ONNX Runtime (see
  [`internal/transcribe/silero.go`](../internal/transcribe/silero.go)
  and [docs/silero.md](silero.md) for the design) and sends
  `input_audio_buffer.commit` whenever the model reports
  `silence_duration_ms` of non-speech, producing one `completed` per
  utterance without waiting for hotkey release. Requires
  `manual_commit: true` and `libonnxruntime.so` installed (vocis
  auto-discovers the library or takes an explicit path via
  `streaming.onnxruntime_library`).

### Known quirks

- **Redundant final inference.** After the last delta, Lemonade runs a
  second full Whisper pass over the entire audio buffer before emitting
  `completed`. This adds ~2–3 s per turn on current hardware and the
  args used in the second pass are identical to the first — you cannot
  tune it away via `session.update`. See
  [`internal/transcribe/transcribe.go`](../internal/transcribe/transcribe.go)
  spans `vocis.transcribe.wait_final` for the live gap (`first_event_ms`
  vs `completed_ms`).
- **Cumulative deltas under Whisper (not incremental).** Per OpenAI's
  [realtime docs](https://developers.openai.com/api/docs/guides/realtime-transcription#handling-transcriptions):
  `whisper-1` emits each `transcription.delta` as the full turn
  transcript (same text as `completed`), while `gpt-4o-transcribe`
  and `gpt-4o-mini-transcribe` emit incremental new text. Lemonade's
  Whisper models (e.g. `whisper-v3-turbo-FLM`) follow the same
  cumulative pattern — so the delta semantics are model-specific, not
  backend-specific. Naïve concatenation (`partial += delta`) against
  cumulative deltas produces junk like `"OkOK IOK I see"`. The
  package-level `deltaStrategyForModel` helper (in `delta.go`) picks
  `mergeCumulativeDelta` when the model name contains `"whisper"`
  (case-insensitive) and `mergeIncrementalDelta` otherwise. `Stream`
  is wired with this strategy at construction; neither `Transport`
  implementation knows about delta semantics.
- **Commit race.** If the client sends `commit` immediately after the
  last audio append, VAD may not have scheduled the
  `speech_stopped` event yet, and Lemonade will run an "empty commit"
  path. vocis pads with a short sleep and falls back to
  `ErrInputAudioBufferCommitEmpty` handling.

## Non-realtime transcription

```
POST http://localhost:13305/api/v1/audio/transcriptions
Content-Type: multipart/form-data
  model=<model_id>
  file=<audio file>
```

OpenAI-compatible. Good for batch experiments where you want a single
JSON response instead of streamed deltas. Passing `file=@/dev/null`
returns a 500 with `Could not open audio data from memory buffer` — the
multipart body must be a valid audio file.

## TTS (`/audio/speech`)

```
POST http://localhost:13305/api/v1/audio/speech
Content-Type: application/json
{
  "model":           "kokoro-v1",
  "input":           "hello world",
  "voice":           "af_heart",
  "response_format": "pcm"
}
```

Fields and quirks:

- `model` — must be a model with label `tts`; as of 10.2.0 that's
  `kokoro-v1`. Other voices may appear in future versions.
- `voice` — observed working voice ids include `fable` and `af_heart`.
  There is no `/audio/voices` endpoint (`404 not_found`), so voice
  discovery is out-of-band (check the Kokoro model card). **Unknown
  voice ids silently return HTTP 200 with a zero-byte body** instead of
  a 4xx — always sanity-check `Content-Length` or the decoded sample
  count before assuming TTS succeeded.
- `response_format`:
  - Omitted or `mp3` → returns `audio/mpeg` (MP3).
  - `"wav"` → 24 kHz mono **IEEE-float** (32-bit) WAV. Most WAV loaders
    that assume PCM16 will fail; convert or prefer `pcm` below.
  - `"pcm"` → raw PCM16 LE at 24 kHz with
    `Content-Type: audio/l16;rate=24000;endianness=little-endian`.
    Simplest for programmatic use — no header parsing, no float decode.

The `lemonade-probe -text` mode uses `pcm` and parses the rate from the
content-type so it automatically adapts if Lemonade's TTS sample rate
changes.

## Chat completions (post-processing)

```
POST http://localhost:13305/api/v1/chat/completions
```

Standard OpenAI-compatible chat API. vocis uses this for post-processing
transcripts (cleaning filler words, fixing casing) via the existing
`transcribe.Client.PostProcess` path.

Streaming (`"stream": true`) works and emits standard `data:` SSE
frames. To stream with `curl`, prefer `curl -N`; with `httpie` (`http`
command), set `PYTHONUNBUFFERED=1` or use `--stream` — otherwise the
client buffers until the response completes.

### Model-swap cost

Because `max_models.llm: 1`, switching between chat models (e.g. picking
`lfm2.5-it-1.2b-FLM` for PP after `qwen3-*` served the previous request)
triggers an unload + load inside Lemonade. First request after a swap
can take 2–5 s extra. Symptoms:

- Transient 500 from `/chat/completions` while the old model is still
  unloading (rare race).
- The first streamed token arrives well after any normal latency
  budget.

Pre-warming: send an empty/short chat request at vocis startup to get
the target model resident before the user's first dictation.

## Session-start preflight

The audio slot has the same `max_models.audio: 1` constraint, so
`whisper-v3-turbo-FLM` can be downloaded but not resident — e.g. when
something else (a different audio model like `gemma4-it-e2b-FLM` with
the `transcription` label) stole the slot. Without a check, the WS
realtime path lazy-loads the model while audio is already streaming,
which shows up as "no transcript ever arrives" and a context timeout.

vocis runs a two-layer defense:

1. **Startup warm (fire-and-forget).**
   [`transcribe.EnsureLemonadeModelsLoaded`](../internal/transcribe/warm_transcription.go)
   runs once when `vocis start` boots. It calls `/health`, and if the
   configured transcribe or PP model isn't resident, POSTs a 100 ms
   silent WAV to `/audio/transcriptions` (or an empty chat request for
   the PP model) to force a load in the background.
2. **Transcribe-time preflight (blocking).**
   [`transcribe.EnsureTranscribeModelLoaded`](../internal/transcribe/warm_transcription.go)
   runs every time the user starts a dictation session (inside
   `startRecordingLocked`). It re-checks `/health`; if the transcribe
   model isn't resident, it fires the warm POST inline before the
   recorder starts. While the warm is running, the overlay subtitle
   switches to `overlay.listening.loading_model` (default
   `"○ Loading {model}..."`) so the user knows why the session hasn't
   started yet. Typical warm path: ~1 ms (health check only).
   Cold-load path: ~5–10 s (warm POST blocking on NPU load).

The transcribe-time preflight covers the cases the startup warm can't:

- vocis was launched before Lemonade (or while Lemonade was still
  booting) and the startup warm failed silently.
- Another client loaded a different audio model after vocis started
  and evicted whisper from the slot.
- The user changed `transcription.model` in their config between
  sessions (vocis reloads config on every session start).

Errors from the preflight (Lemonade unreachable, warm POST returned
5xx, ctx cancelled) surface as a session-start error overlay. Running
`vocis` against the OpenAI backend skips the preflight entirely — it's
a no-op for non-Lemonade backends.

### Sampling knobs

The `postprocess.*` config exposes the sampling params vocis forwards
to `/chat/completions`. OpenAI-standard (`temperature`, `top_p`,
`frequency_penalty`, `presence_penalty`, `stop`) ride on the request
body directly. Non-standard (`min_p`, `repetition_penalty`) are sent
as extra JSON fields — Lemonade's OGA backend honors them; the OpenAI
Cloud API silently ignores them.

`seed` is intentionally not exposed: Lemonade accepts the field but
does not thread it to the sampler, so same-seed calls at temp>0 still
produce different outputs. Treat Lemonade chat as non-reproducible.

## The lemonade-probe script

[`scripts/lemonade-probe`](../scripts/lemonade-probe/main.go) is a
standalone Go tool that exercises the realtime WS directly, bypassing
vocis. Three input modes:

```bash
# synthesize text via Lemonade kokoro-v1 and stream through realtime
go run ./scripts/lemonade-probe -text "hello world"

# pre-bake a fixture WAV from text (no streaming)
go run ./scripts/lemonade-probe -text "hello" -out /tmp/hello.wav

# record from mic (16 kHz mono) and stream
go run ./scripts/lemonade-probe -mic 5 -save_wav /tmp/probe.wav

# replay any WAV — rate is read from the RIFF header
go run ./scripts/lemonade-probe -wav /tmp/hello.wav
```

The probe logs every inbound event with elapsed ms and prints a
`SUMMARY` line with `post_commit_delta`, `completed`, and
`text_matches`. Pair with `VOCIS_WS_DUMP=1 ./bin/vocis transcribe` to
diff what vocis does against the minimal reference flow.
