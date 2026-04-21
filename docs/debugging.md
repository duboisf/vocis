# Debugging

## Logs

Each `serve` session writes a log file under `~/.local/state/vocis/sessions/`. Files are named by timestamp (e.g. `20260410-103747.log`) and cleaned up after 7 days.

Log levels: `DEBUG`, `INFO`, `WARN`, `ERROR`. Structured fields use key=value format (e.g. `duration=5.4s`, `chars=53`).

Useful things to look for:

- `postprocess` — input/output text (DEBUG), timeouts, errors
- `finalization` — trailing transcript assembly, commit errors
- `duck` — audio volume ducking/restore
- `hotkey` — fallback decisions, registration failures

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
| `vocis.transcribe.finalize` | Total finalization time — if this is slow, the "Wrapping up" countdown may expire. |
| `vocis.transcribe.wait_final` | `segment_count`, `trailing.skipped`. Also carries inline events: `realtime.delta` (once per delta), `realtime.completed`, `realtime.failed` — each with `since_commit_ms` so you can see when Whisper's first pass landed vs when the redundant second pass finished. |
| `vocis.postprocess` | `skipped` attribute, `first_token_timeout` vs `first_token_received` events, `elapsed` timings. Inline events: `postprocess.input` (with `input.text`) and `postprocess.output` (with `output.text` + `skipped`/`reason` when PP fell back). Text is truncated to 500 chars. |
| `vocis.inject` | Paste vs type, terminal detection, target window. |

### Recall-mode spans

Each captured utterance and each pick is its own root trace (spans are
started with `context.Background()` so they don't chain to the daemon
lifetime). If the recall daemon feels slow or CPU-heavy after use,
inspect these first:

| Span | What to look for |
|------|-----------------|
| `vocis.recall.capture` | One per VAD-bounded utterance the daemon kept. `segment.id`, `segment.duration_ms`, `segment.peak_level`, `segment.force_flushed` (true when `max_segment_seconds` cut it short). |
| `vocis.recall.transcribe` | One per daemon transcribe call. `segment.id`, `cache_hit`, `postprocess`, `transcript.length`, `runtime.goroutines_delta` (should be 0 — non-zero means we're leaking). Child spans: `…transcribe.feed`, `…transcribe.finalize`, `…transcribe.postprocess`. |

`recall: transcribe id=N goroutines M→K (Δ=±X)` also lands in the
daemon log per transcribe — use it as a quick sanity check when you
don't want to spin up Jaeger. A positive Δ that doesn't come back down
means goroutines are accumulating.
