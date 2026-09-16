# Debugging

## Logs

Each `serve` session writes a log file under `~/.local/state/vocis/sessions/`. Files are named by timestamp (e.g. `20260410-103747.log`) and cleaned up after 7 days.

Log levels: `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR`. Structured fields use key=value format (e.g. `duration=5.4s`, `chars=53`).

### Convention

Every meaningful branch leaves a trail. New features and guards must log when they fire — if a user pastes their session log into a bug report, the transcript should show which code paths ran. Level guide:

- `TRACE` — high-volume protocol events (every SSE delta, every chat-audio request body). Cheap, verbose.
- `DEBUG` — routine state transitions, one-shot protocol decisions, internal handoffs.
- `INFO` — user-visible decisions (filtered hallucination, submit mode toggle, config reload, dictation started/stopped).
- `WARN` — recoverable problems (empty transcript, unknown event type).
- `ERROR` — fatal to the current operation (dial failed, commit refused with non-empty reason).

If an event is filtered from the existing trace machinery (e.g. the audio payload redaction in `redactedRequestJSON` to keep request-body dumps readable), add an explicit log so the behavior stays visible.

### Useful things to look for

- `chat-audio: cut clip reason=vad_stopped|force_cut|release clip_ms=…` (DEBUG) — one per clip the pump cut while the hotkey was held.
- `chat-audio: release with N clip(s), …ms of audio, vad_speech=…` — the pump handed its clips to Finalize.
- `chat-audio: posting chunk clips=N wav=…B req=…B` — the single `/chat/completions` POST at release.
- `chat-audio: response %q` — the transcript.
- `chat-audio: request body (audio redacted)` (DEBUG) — the exact JSON message structure (system prompt, multi-clip layout) with audio bytes replaced by `<wav N bytes>` placeholders.
- `chat-audio: SSE delta` (TRACE) — every streamed delta from the model.
- `chat-audio: forced cut at Ns` — a long monologue without a VAD pause hit the per-clip cap; the clip is stored and sent with the rest at release.
- `chat-audio: dropped silent clip peak=… rms=…` — energy gate filtered a clip before posting. Compare against `min_chunk_peak` / `min_chunk_rms`.
- `chat-audio: energy gate kept clip on VAD verdict` — the clip's RMS was under `min_chunk_rms` (long pause before a short phrase) but Silero saw speech, so only the peak arm applied and the clip was posted.
- `dropped hallucinated final:` — the hallucination filter caught a Whisper/Gemma stock phrase ("Thank you.", etc.). See `transcription.hallucination_filters`.
- `finalization completed` — elapsed time and transcript length returned by `Finalize`.
- `chat-audio: no clips survived the energy gate` — every clip was judged silent; nothing was sent and the overlay shows "No speech detected".
- `duck` — audio volume ducking/restore.
- `overlay backend:` — `x11` when the on-screen overlay is live, `none` when `x11.NewOverlay()` failed and the no-op overlay took over (preceded by an `overlay: cannot create X11 overlay` WARN with the underlying error). `none` means dictation works but there is no visual feedback.
- `hotkey` — fallback decisions, registration failures.
- `submit mode:` — Enter-after-paste decision. See `insertion.auto_submit`.
- `kitty capture state:` / `kitty post-send state:` — pre/post `kitty @ ls` snapshot of the targeted window's title, foreground process, focus, alt-screen, and at-prompt flags. Compare the two when triaging "transcript landed in the wrong window."
- `audio capture: wrote <path> bytes=N` (INFO) — one per dictation, written just before the POST. Pair with the `chat-audio: posting chunk clips=…` line to find the WAV that was sent to the model. See "Audio capture (chunk replay)" below.
- `audio capture: gc deleted N files older than …` (INFO) / `gc swept dir=… (0 stale)` (DEBUG) — periodic prune of the audio dir.
- `kitty verify-paste id=N screen.len=…` (DEBUG) and `kitty verify-paste id=N: payload head … NOT visible …` (WARN) — post-send `kitty @ get-text --extent screen` probe. The WARN means `send-text` returned 0 but the program in the window appears to have swallowed the bytes (alt-screen TUI in an odd input mode, claude mid-stream, shell with bracketed-paste off). Disable with `insertion.kitty_verify_paste: false`.

## Transcript history

Every delivered dictation appends one JSON line to
`transcription.history_file` (default
`~/.local/state/vocis/transcripts.jsonl`): `time`, `text`,
`window_class`, `audio_ms` and `audio` (the captured WAV path while it
still exists, see below). This is the record to audit transcription
quality against; pair a line's `audio` with its `text` and replay the
WAV through `/chat/completions` to compare. `INFO  transcript history:
appended N chars` confirms the write.

## Audio capture (chunk replay)

Every chunk POSTed to `/chat/completions` is mirrored to
`~/.local/state/vocis/audio/` as a WAV file (or `$XDG_STATE_HOME/vocis/audio`
when that env var is set). Use this when a transcript looks wrong and
you want to hear what the model actually received.

Filenames are `<session-ts>-chunkNNN-<reason>.wav` where:
- `session-ts` matches the timestamp prefix of the matching session log
  in `~/.local/state/vocis/sessions/`.
- `NNN` is a monotonic per-session counter (so chunk001 is the first
  POST of that dictation session, etc.).
- `reason` is `release`: the WAV is the concatenation of every clip
  that survived the energy gate, i.e. exactly what the model heard.

Each write also leaves an `INFO  audio capture: wrote <path> bytes=N`
line in the session log so you can grep either side to find the other.

A long-lived goroutine sweeps the dir every
`transcription.audio_capture.gc_interval_seconds` (default 600s) and
deletes any WAV whose mtime is older than
`transcription.audio_capture.ttl_seconds` (default 3600s). Disable the
whole feature with `transcription.audio_capture.enabled: false`.

## Tracing (Jaeger)

When `telemetry.enabled: true` in the config, OpenTelemetry spans are exported via OTLP/gRPC to `telemetry.endpoint` (default `localhost:4317`). Jaeger UI is at `http://localhost:16686`.

Spans are buffered per-trace and flushed as one batch when the trace's root span ends (see `internal/telemetry/processor.go`). This is unlike the SDK's default `BatchSpanProcessor`, which flushes every 2 s and would cause Jaeger to log `"parent span ID=N is not in the trace; skipping clock skew adjustment"` warnings on every child of a long-lived root (`vocis.dictation`, `vocis.transcribe.finalize`) — short children flush before their parent finishes, the collector's adjuster sees them parentless, and the warning is persisted on the child span. Ending a trace's root flushes the entire trace in one wire batch so Jaeger always sees the parent. Process exit must call `Shutdown` (already deferred by the entry-point) or in-flight traces are lost.

### Fetching traces via the API

Fetch a specific trace by ID:

```bash
curl -s 'http://localhost:16686/api/traces/<traceID>' | python3 -m json.tool
```

Find recent traces for the vocis service:

```bash
curl -s 'http://localhost:16686/api/traces?service=vocis&limit=5&lookback=1h'
```

The JSON response contains all spans with their tags (attributes) and logs (events).

### Key spans to inspect

| Span | What to look for |
|------|-----------------|
| `vocis.dictation` | Root span. `hotkey.backend` tells you whether the session used `x11` or `gnome-extension`. |
| `vocis.capture_target` | `capture.source` = `xdotool` (X11 path) or `extension` (gnome path). If the extension path, look for the nested `vocis.gnome.get_focused_window` span with the D-Bus call timing/error. |
| `vocis.transcribe.finalize` | Total finalization time. The "Wrapping up" overlay phase counts up from 0 for as long as this span runs — there is no outer deadline, so a slow finalize will keep ticking rather than time out. |
| `vocis.transcribe.chat_audio.chunk` | The single `/chat/completions` POST at release. Attributes: `chunk.clip_count`, `chunk.duration_ms`, `chunk.wav_bytes`, `chunk.request_bytes`, `chunk.response_text`. |
| `vocis.inject` | Paste vs type, terminal detection, target window. |

### Recall-mode spans

Each captured utterance and each pick is its own root trace (spans are
started with `context.Background()` so they don't chain to the daemon
lifetime). If the recall daemon feels slow or CPU-heavy after use,
inspect these first:

| Span | What to look for |
|------|-----------------|
| `vocis.recall.capture` | One per VAD-bounded utterance (kept **or** dropped). `segment.id` (0 if dropped), `segment.duration_ms`, `segment.peak_level`, `segment.avg_level` (RMS), `segment.force_flushed` (true when `max_segment_seconds` cut it short), `segment.dropped_as_silence` (true when peak/RMS filter fired), `segment.drop_reason` (empty or e.g. `"rms=0.003 < min_rms=0.005"`), plus the threshold values for context. |
| `vocis.recall.transcribe` | One per daemon transcribe call. `segment.id`, `cache_hit`, `transcript.length`, `runtime.goroutines_delta` (should be 0 — non-zero means we're leaking). Child spans: `…transcribe.feed`, `…transcribe.finalize`. |

`recall: transcribe id=N goroutines M→K (Δ=±X)` also lands in the
daemon log per transcribe — use it as a quick sanity check when you
don't want to spin up Jaeger. A positive Δ that doesn't come back down
means goroutines are accumulating.

For VAD-specific debugging (stuck-in-speech segments, hysteresis
tuning, peak vs RMS filter decisions), see [docs/silero.md](silero.md).
