# Debugging

## Logs

Each `serve` session writes a log file under `~/.local/state/vocis/sessions/`. Files are named by timestamp (e.g. `20260410-103747.log`) and cleaned up after 7 days.

Log levels: `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR`. Structured fields use key=value format (e.g. `duration=5.4s`, `chars=53`).

### Convention

Every meaningful branch leaves a trail. New features and guards must log when they fire — if a user pastes their session log into a bug report, the transcript should show which code paths ran. Level guide:

- `TRACE` — high-volume protocol events (every WS frame, audio tail pad). Cheap, verbose.
- `DEBUG` — routine state transitions, one-shot protocol decisions, internal handoffs.
- `INFO` — user-visible decisions (filtered hallucination, submit mode toggle, config reload, dictation started/stopped).
- `WARN` — recoverable problems (postprocess timeout, empty transcript, unknown event type).
- `ERROR` — fatal to the current operation (dial failed, commit refused with non-empty reason).

If an event is filtered from the existing trace machinery (e.g. `dumpWSFrame` skipping outbound `input_audio_buffer.append` to keep the log readable), add an explicit log so the behavior stays visible.

### Useful things to look for

- `ws ←` / `ws →` — every inbound / outbound WS frame except audio append spam. Cumulative deltas, commits, VAD events all appear here.
- `dropped hallucinated final:` — the hallucination filter caught a Whisper/Gemma stock phrase ("Thank you.", etc.). See `transcription.hallucination_filters`.
- `tail silence pad:` — the pre-commit silence appended so the last word doesn't get eaten. See `streaming.tail_silence_ms`.
- `realtime: session.updated payload=` — what the backend echoed back after our `session.update`. Shows which fields the server actually honored (Lemonade silently discards unknown keys like `prompt` and `noise_reduction`).
- `realtime: delta event has empty text under 'delta' key` — a backend emitted a transcription delta with no text under the spec-standard field. Diagnostic for backend field-name mismatches.
- `realtime: input_audio_buffer.committed` — server-side commit acknowledged. Combined with the fast-path decision in `collectTrailing`, lets you see whether a release-time commit was skipped.
- `postprocess` — input/output text (DEBUG), timeouts, errors.
- `finalization` — trailing transcript assembly, commit errors.
- `duck` — audio volume ducking/restore.
- `hotkey` — fallback decisions, registration failures.
- `submit mode:` — Enter-after-paste decision. See `insertion.auto_submit`.

## Tracing (Jaeger)

When `telemetry.enabled: true` in the config, OpenTelemetry spans are exported via OTLP/gRPC to `telemetry.endpoint` (default `localhost:4317`). Jaeger UI is at `http://localhost:16686`.

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
| `vocis.transcribe.connect` | Connection time — slow means network or backend issues. `transcribe.backend` = `openai` or `lemonade`. |
| `vocis.transcribe.finalize` | Total finalization time. The "Wrapping up" overlay phase counts up from 0 for as long as this span runs — there is no outer deadline, so a slow finalize will keep ticking rather than time out. |
| `vocis.transcribe.wait_final` | Drain-loop wait for the backend to finish all in-flight transcriptions. Key attrs: `wait_final.exit_reason` (`no_inflight_work` / `timeout` / `error` / `stream_error_with_text`), `wait_final.drained_segments` (how many trailing transcripts the loop pulled), `wait_final.elapsed_ms`, `wait_final.text_chars`, `wait_final.{started,completed}_count_at_{entry,exit}` (server-VAD speech_started vs transcription.completed counts), `wait_final.commit_skipped_buffer_drained` (server VAD already drained — no commit sent), `wait_final.commit_empty_swallowed` (commit raced server VAD; benign "buffer empty" error swallowed). Also `wait_final.first_event_type` / `first_event_ms` / `first_delta_text` for first-token latency. Inline events: `realtime.delta` (once per delta), `realtime.completed`, `realtime.failed`, each with `since_commit_ms`. |
| `vocis.postprocess` | `skipped` attribute, `first_token_timeout` vs `first_token_received` events, `elapsed` timings. Inline events: `postprocess.input` (with `input.text`) and `postprocess.output` (with `output.text` + `skipped`/`reason` when PP fell back). Text is truncated to 500 chars. |
| `vocis.inject` | Paste vs type, terminal detection, target window. |

### Recall-mode spans

Each captured utterance and each pick is its own root trace (spans are
started with `context.Background()` so they don't chain to the daemon
lifetime). If the recall daemon feels slow or CPU-heavy after use,
inspect these first:

| Span | What to look for |
|------|-----------------|
| `vocis.recall.capture` | One per VAD-bounded utterance (kept **or** dropped). `segment.id` (0 if dropped), `segment.duration_ms`, `segment.peak_level`, `segment.avg_level` (RMS), `segment.force_flushed` (true when `max_segment_seconds` cut it short), `segment.dropped_as_silence` (true when peak/RMS filter fired), `segment.drop_reason` (empty or e.g. `"rms=0.003 < min_rms=0.005"`), plus the threshold values for context. |
| `vocis.recall.transcribe` | One per daemon transcribe call. `segment.id`, `cache_hit`, `postprocess`, `transcript.length`, `runtime.goroutines_delta` (should be 0 — non-zero means we're leaking). Child spans: `…transcribe.feed`, `…transcribe.finalize`, `…transcribe.postprocess`. |

`recall: transcribe id=N goroutines M→K (Δ=±X)` also lands in the
daemon log per transcribe — use it as a quick sanity check when you
don't want to spin up Jaeger. A positive Δ that doesn't come back down
means goroutines are accumulating.

For VAD-specific debugging (stuck-in-speech segments, hysteresis
tuning, peak vs RMS filter decisions), see [docs/silero.md](silero.md).
