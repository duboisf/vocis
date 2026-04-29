# vocis

*vocis* — Latin genitive of *vox* ("of voice"), pronounced **WOH-kiss** in
classical Latin (the "v" is a "w", the "c" is hard like "k", and the "i" is
short).

`vocis` is a Linux voice-to-text desktop helper written in Go. Hold a global
hotkey, speak, release — the transcript is pasted back into the app you were
already using. Local-first by default (NPU-accelerated Whisper via Lemonade
Server, no API key, no network), with OpenAI Cloud as an opt-in backend. An
always-on-top X11 overlay gives you the same "record / transcribe / typed"
rhythm that keeps dictation feeling fast.

It was very much vibe-coded from scratch to... scratch an itch.

## What's in the box

`vocis` ships four subcommands, each a different way to turn audio into
text (or text into audio):

| Command | Mode | When to use |
|---|---|---|
| `vocis serve` | Push-to-talk | The classic. Hold the hotkey, speak, release, paste. Also the default when you run `vocis` with no subcommand. |
| `vocis recall` | Always-on capture | Long-form dictation. Daemon records continuously, segments speech with Silero VAD, and you transcribe on demand — useful when you want to remember what you said five minutes ago without holding a key the whole time. |
| `vocis speak` | Text-to-speech | Send any text through Lemonade's Kokoro TTS and play it back through `paplay`. Handy for proofreading transcripts by ear or piping `vocis recall last 10m \| vocis speak`. |
| `vocis transcribe` | One-shot CLI dictation | Records from the default mic, prints the transcript to stdout. No overlay, no hotkey, no paste — useful for iterating on transcription quality / latency from a terminal, or piping into shell tools. Press Enter to finish, Ctrl-C to abort. |

## Cool features

### Both modes

- **Local-first transcription.** Defaults to [Lemonade Server](https://github.com/lemonade-sdk/lemonade)
  ≥ 10.3.0 with `whisper-v3-turbo-FLM` running on NPU via FLM — no
  API key, no network. Flip to OpenAI Cloud (`vocis config backend openai`) if you'd
  rather use `gpt-4o-mini-transcribe`. Both backends speak the same realtime
  WebSocket protocol; the rest of the pipeline is identical.
- **LLM post-processing** that cleans up filler words ("um", "uh", "like")
  and false starts without changing your meaning. Few-shot prompted to
  *never* answer questions in the transcript — "what time is it?" stays a
  question. Backend-agnostic via OpenAI-compatible `/chat/completions`.
- **Mic preroll during model preflight.** On Lemonade with a cold model,
  vocis opens the mic *before* the 5–10 s NPU load and replays those samples
  into the realtime session once it's ready, so the first words after you
  press the hotkey aren't lost.
- **Pre-warm of the post-processing model** while you're still talking, so
  the first PP request doesn't pay Lemonade's `max_models.llm: 1` swap cost.
- **Hallucination filters** for stock phrases Whisper-class models love to
  emit on silence ("Thank you.", "Thanks for watching.", lone "you").
- **Audio ducking.** Speaker volume drops to 10% (configurable) while the
  mic is hot so you don't transcribe your own playback.
- **Strict config loader.** Unknown YAML keys fail at startup instead of
  drifting silently. Renamed sections fail with a clear "rename `X:` to `Y:`"
  message.

### `vocis serve` — push-to-talk dictation

- **Hold-to-record by default**, with a `toggle` mode for hands-off setups.
- **Submit mode.** Tap the hotkey while still holding it (release + repress
  the trigger key without letting go of the modifiers) to arm an
  auto-Enter-after-paste — the overlay shows a throbbing yellow `⏎ submit`
  indicator. Useful for dictating prompts directly into Claude Code, chat
  inputs, etc. Tap again to disarm. Works on both X11 and the GNOME Wayland
  extension backend.
- **Auto-submit option** via `insertion.auto_submit: true` for users who
  almost always want Enter — the toggle still works to disarm per-session.
- **Hotkey fallback.** If the configured shortcut is grabbed by another
  app, vocis tries `ctrl+alt+space`, `f8`, `f9`, `shift+f8` in order and
  warns in the log which one it ended up with.
- **Live partial overlay** showing in-flight transcription as you speak
  (cloud OpenAI only — Lemonade in manual-commit mode skips interim
  inference for lower end-of-utterance latency).
- **Client-side Silero VAD** for mid-hold segmentation on the local
  Lemonade backend: long monologues get chunked into multiple committed
  segments instead of one giant final inference. Configurable hysteresis,
  silence/speech minimums, ONNX runtime.
- **Tail silence padding** appended at finalize so Whisper-class models
  reliably segment the last word.
- **🎯 Kitty terminals: focus-free direct delivery.** When the target
  window is kitty, vocis records the *exact* tab/pane you were in (via
  `kitty @ ls`) at recording start, and at delivery time pushes the
  transcript straight into that pane via `kitty @ send-text`. **No focus
  change, no clipboard pollution, no paste shortcut.** Switch tabs or move
  to another app while dictating — the transcript still lands where you
  started, and your current keystrokes elsewhere are not disrupted.
  Submit-mode Enter is also routed through kitty remote control. If the
  original tab was closed mid-dictation, the transcript falls back to the
  clipboard with a "target gone" warning. If the kitty CLI is unreachable,
  vocis transparently falls back to OS-window focus + paste so dictation
  still completes.
- **Terminal-aware paste keys.** `Ctrl+V` for normal apps, `Ctrl+Shift+V`
  for terminals — the terminal class list is configurable.
- **Clipboard restore.** The previous clipboard contents come back ~250 ms
  after paste, so dictation doesn't eat what you had on it.
- **Modifier release.** The still-held hotkey modifiers are released
  programmatically before the synthesized paste, so `Ctrl+Shift+V` doesn't
  collide with the held `Ctrl+Shift`.
- **Connection retry.** WebSocket connect retries up to 3 times on
  transient failures, with the attempt counter visible in the overlay.
- **Escape to skip post-processing.** While the overlay shows "Finishing",
  Escape pastes the raw transcript immediately.
- **Configurable overlay** — every string, the font (resolved via
  `fc-match`, defaults to `monospace`), size, opacity, and auto-hide
  timer.
- **Loading-model overlay** during Lemonade's cold preflight, showing
  `○ Loading whisper-v3-turbo-FLM...` so you know why the session hasn't
  started yet.

### `vocis recall` — always-on capture

- **Bounded ring buffer** of speech-only segments (silence is dropped by
  Silero VAD before it ever hits memory). 7-day retention default,
  capped at 2000 segments, both bounds tunable.
- **Memory-only by default** — kill the daemon and the buffer is gone.
  Flip `recall.persist.mode: disk` to mirror each segment to JSON under
  `~/.local/state/vocis/recall/`, with the same retention applied on
  reload.
- **`recall pick`** with an `fzf`-based picker (auto-detected) showing
  segment age, duration, peak/RMS levels, and cached transcript previews.
  Multi-select with Tab, `Ctrl-P` plays the focused segment via
  `recall replay`, Enter confirms. Plain-table fallback when fzf isn't
  installed (or `--no-fzf` to force it). Range syntax for `--ids`: `3`,
  `3-5`, `3-`, `-5`, `all`, or comma-separated mixes like `3,5-7,10-`.
- **`recall last <duration>`** — concatenates every segment in the window
  through one realtime session with a configurable silence gap between
  them. The ASR keeps context across segment boundaries, which is
  cheaper and produces better transcripts than N independent sessions.
  No client-side timeout; Ctrl-C cleanly cancels.
- **`recall replay`** pipes raw segment PCM into `paplay` so you can
  hear what the daemon actually captured before deciding whether to drop
  it. Streamed through one persistent paplay process so back-to-back
  segments don't pop.
- **`recall drop`** removes segments (and their on-disk JSON when
  persistence is on).
- **Min peak / RMS filters** reject Silero-VAD false positives on fan
  hum, keyboard clicks, and room tone.
- **Min utterance / preroll knobs** so word onsets aren't clipped and
  micro-segments don't pollute the ring.

### `vocis speak` — text-to-speech

- Lemonade Kokoro TTS at `/audio/speech` with `response_format=pcm`,
  streamed straight into `paplay` — no intermediate WAV decode.
- `--voice` / `--model` per-call overrides, with sensible defaults
  (`shimmer` voice, `kokoro-v1` model).
- Reads text from CLI args or stdin, so you can pipe
  `vocis recall last 10m | vocis speak`.
- `--out PATH` writes a 24 kHz mono PCM16 WAV instead of playing,
  with `-` for stdout.

## Quick start

```bash
make build
./bin/vocis config init
./bin/vocis serve     # default backend is local Lemonade — no key needed
```

If you're on OpenAI Cloud instead:

```bash
./bin/vocis config backend openai
./bin/vocis key set     # paste your API key, stored in the system keyring
./bin/vocis serve
```

While `vocis serve` is running:

1. Focus any text field (or kitty pane).
2. Hold `Ctrl+Shift+Space`.
3. Speak.
4. Release the hotkey to stop and insert the transcript.

Always-on capture mode in another terminal:

```bash
./bin/vocis recall start                # foreground daemon
./bin/vocis recall pick                 # browse + transcribe (fzf or plain)
./bin/vocis recall last 10m             # batch the last 10 minutes
./bin/vocis recall last 1h --postprocess
./bin/vocis recall replay --ids=3-5     # hear what was captured
./bin/vocis recall drop --ids=3,7-9     # forget those segments
./bin/vocis recall stop                 # ask the daemon to exit
```

Speak text via Kokoro:

```bash
./bin/vocis speak "hello from vocis"
echo "from a pipe" | ./bin/vocis speak
./bin/vocis speak --voice fable --out /tmp/hi.wav "saved to file"
```

Generate shell completions:

```bash
./bin/vocis completion bash > ~/.local/share/bash-completion/completions/vocis
./bin/vocis completion zsh  > ~/.zfunc/_vocis
```

## Backends

`vocis` supports two transcription backends, configured under `transcription:`:

### Lemonade (local, default)

[Lemonade Server](https://github.com/lemonade-sdk/lemonade) **≥ 10.3.0**
exposes an OpenAI-compatible REST API plus a realtime-transcription
WebSocket. Defaults:

- `transcription.backend: lemonade`
- `transcription.base_url: http://localhost:13305/api/v1`
- `transcription.realtime_url: ws://localhost:9000`
- `transcription.model: whisper-v3-turbo-FLM` (NPU on Ryzen AI via FLM)
- `streaming.manual_commit: true` and `streaming.client_vad: true` (skip
  Lemonade's redundant interim inference; let Silero do client-side VAD
  for mid-hold chunking)

`vocis config backend` autodetects a running Lemonade on localhost and
writes the right URLs. `vocis config models` lists `tts`/`speech`/
`transcription`/`reasoning`-labelled models with download status so you
can pick one for transcription and one for post-processing.

**Lemonade Server 10.3.0+ required.** See `docs/lemonade.md` for
which 10.3 protocol/classification changes vocis depends on (the
`input_audio_buffer.cleared` finalize handshake, the
`gemma4-it-e2b-FLM` audio→llm reclassification, and the preflight
label guard).

### OpenAI Cloud

- `transcription.backend: openai`
- `transcription.model: gpt-4o-mini-transcribe`
- API key from the system keyring (`vocis key set`) or `OPENAI_API_KEY`.
- Org/project/language overrides via `transcription.organization` etc.

## GNOME Wayland

Wayland blocks third-party processes from grabbing global hotkeys via X11,
and GNOME 46 doesn't yet implement the `org.freedesktop.portal.GlobalShortcuts`
portal. The workaround is a small GNOME Shell extension that registers the
hotkey via Mutter's API and forwards press/release events to vocis over
D-Bus. It also implements the focus + paste primitives natively, so vocis
on Wayland doesn't need `xdotool` or `xclip`.

```bash
make install-extension
# Log out and log back in (gnome-shell only rescans on session start).
make enable-extension
```

Verify with `vocis doctor` — the `wayland-hk` line should report `ok`.

The extension exposes one D-Bus interface
(`io.github.duboisf.Vocis.Hotkey` at object path
`/io/github/duboisf/Vocis/Hotkey`) with methods for shortcut activation
signals, focused-window introspection, window activate, key synthesis,
clipboard read/write, and modifier release. The accelerator is currently
hardcoded to `ctrl+shift+space` (`SHORTCUT_LABEL` in
`extensions/vocis-gnome/extension.js`); change it there and keep the
`hotkey:` field in `config.yaml` in sync.

Caveats independent of the extension:

- The overlay window comes from XWayland and may not appear above
  Wayland-native windows in some compositors.

## Dependencies

`vocis` records audio in-process over PulseAudio / PipeWire (`jfreymuth/pulse`).
On X11 / XWayland it shells out to a few stable desktop tools:

- `xdotool` for focus restore, simulated paste, and Enter keypress (X11
  backend only — gnome-extension backend uses Mutter natively)
- `xclip` for clipboard read/write (X11 only — gnome-extension uses GDK)
- `wpctl` for audio ducking (PipeWire/PulseAudio volume control)
- `paplay` for `recall replay` and `vocis speak` audio playback
- `kitty` for tab/pane-aware paste — only when the target window is kitty
  and `insertion.kitty_remote_control: true` (the default). Requires
  `allow_remote_control` in `kitty.conf`, or vocis launched from inside
  kitty so `KITTY_LISTEN_ON` is inherited.
- `fzf` for the `recall pick` UI (optional; falls back to a plain table)

For local Lemonade transcription you also need
[Lemonade Server](https://github.com/lemonade-sdk/lemonade) running, plus
ONNX Runtime if you've enabled `streaming.client_vad` (default on
Lemonade) — see `docs/silero.md` for the exact discovery rules.

## Config

The first run creates `~/.config/vocis/config.yaml`. A sample lives at
`config.example.yaml`. Running `vocis config init` when a config exists
opens Neovim in diff mode so you can merge new defaults.

`vocis config init` opens **`nvim`** in diff mode (hardcoded — install
`nvim` to use this flow, or `--force` to overwrite without diffing).

Other `config` subcommands:

- `vocis config backend` — interactively pick `openai` or `lemonade`;
  autodetects a running Lemonade on localhost and rewrites the URLs and
  default model.
- `vocis config models` — interactive picker for transcription and
  post-processing models from the configured backend. **Important on
  Lemonade**: `postprocess.model` defaults to `gpt-4o-mini` (an OpenAI
  model name) which Lemonade doesn't have; use `config models` to pick
  a Lemonade-resident LLM, or set `postprocess.enabled: false`.
- `vocis config edit` — open the config file in `$VISUAL` / `$EDITOR`
  (falls back to `nvim`/`vim`/`nano`).

A few useful fields (see `config.example.yaml` for everything):

- `hotkey`: global shortcut, e.g. `ctrl+shift+space`
- `hotkey_mode`: `hold` or `toggle`
- `transcription.backend`: `lemonade` (default) or `openai`
- `transcription.request_timeout_seconds`: HTTP timeout, `0` to disable
  (useful for cold local model loads)
- `streaming.manual_commit` / `streaming.client_vad`: see "Backends" above
- `streaming.tail_silence_ms`: pad with silence at finalize so Whisper
  segments the last word reliably (default `800`)
- `insertion.mode`: `auto`, `clipboard`, or `type`
- `insertion.auto_submit`: every dictation submits by default
- `insertion.kitty_remote_control`: focus-free kitty delivery (default
  `true`)
- `recall.retention_seconds` / `recall.max_segments`: ring-buffer bounds
- `recall.persist.mode`: `in_memory` (default) or `disk`
- `recall.batch_gap_ms` / `recall.batch_max_seconds`: `recall last`
  concatenation knobs
- `speak.model` / `speak.voice`: Kokoro defaults
- `postprocess.enabled` / `postprocess.model` / `postprocess.prompt`:
  LLM cleanup
- `postprocess.temperature` / `top_p` / `min_p` / `repetition_penalty`:
  sampling knobs forwarded to `/chat/completions`
- `overlay.*`: every overlay string is templated and configurable
- `telemetry.enabled` / `telemetry.endpoint`: OpenTelemetry tracing

Config is reloaded on each recording start, so most changes take effect
without restarting `serve`.

## Secrets

Use the system keyring so the API key is not stored in plain text:

```bash
./bin/vocis key set
```

For one-off sessions you can also export `OPENAI_API_KEY`. Lemonade is
unauthenticated and ignores any key.

## Troubleshooting

### Tracing with Jaeger

Tracing is the first place to look when something goes wrong. Enable it
in config:

```yaml
telemetry:
  enabled: true
  endpoint: localhost:4317
```

Start Jaeger locally:

```bash
docker run -d --name jaeger \
  -p 16686:16686 \
  -p 4317:4317 \
  jaegertracing/all-in-one:latest
```

Open http://localhost:16686, select the `vocis` service, and search for
traces. Each dictation session produces one trace with a hierarchy like:

```
vocis.dictation                       ← root span (entire session lifecycle)
├── vocis.recorder.start              ← PulseAudio init
├── vocis.capture_target              ← focused-window lookup (xdotool / gnome)
├── vocis.kitty.focused_window_id     ← only when target is a kitty class
├── vocis.recording.active            ← user speaking
├── vocis.transcribe.connect          ← realtime WS + TLS
├── vocis.recorder.stop
├── vocis.transcribe.finalize
│   ├── vocis.transcribe.commit
│   └── vocis.transcribe.wait_final
├── vocis.postprocess
└── vocis.inject
    ├── vocis.inject.kitty_direct     ← preferred path on kitty targets
    │   ├── vocis.kitty.exists
    │   └── vocis.kitty.send_text
    ├── vocis.inject.focus            ← compositor fallback path
    └── vocis.inject.paste
```

Span attributes worth knowing:

- `kitty.delivered=true` on `vocis.inject.kitty_direct` — focus-free
  delivery succeeded
- `kitty.target_gone=true` — original tab closed; transcript on clipboard
- `commit.skipped=true` on `vocis.transcribe.commit` — all audio
  consumed by mid-hold segments (normal)
- `trailing.skipped=true` on `vocis.transcribe.wait_final` — no trailing
  audio (normal)
- `skipped=true` on `vocis.postprocess` — PP timed out or was Escape'd

```bash
# Get a specific trace as JSON
curl -s http://localhost:16686/api/traces/<traceID> | python3 -m json.tool

# List recent traces
curl -s 'http://localhost:16686/api/traces?service=vocis&limit=10&lookback=1h'
```

### Session logs

Each `serve` / `recall start` session writes to
`~/.local/state/vocis/sessions/`. Tail the latest:

```bash
tail -50 "$(ls -t ~/.local/state/vocis/sessions/*.log | head -1)"
```

### Doctor

`vocis doctor` runs a one-shot health check covering display, xdotool,
xclip, audio, config, log dir, keyring, the gnome extension, and (on
Lemonade) whether the configured transcribe + post-processing models are
downloaded and currently resident.

### Common issues

| Symptom | Cause | Fix |
|---|---|---|
| Overlay appears but nothing happens | WS connect failed | `vocis.transcribe.connect` ERROR in Jaeger; check Lemonade is up |
| First few words missing on cold Lemonade | Model preflight took longer than mic preroll buffer | Pre-warm by running an empty dictation, or wait for the "ready" subtitle |
| Kitty paste landed in wrong tab | `kitty @ ls` not reachable from vocis | Check `KITTY_LISTEN_ON`; configure `allow_remote_control yes` and `listen_on` in `kitty.conf` |
| Submit-mode Enter goes to wrong window | Compositor focus path used instead of kitty | Confirm `target.KittyWindowID` is set on the trace; if empty, see above |
| Post-processing too aggressive | Prompt removes too much | Edit `postprocess.prompt` |
| Recall daemon eating disk | `persist.mode: disk` + long retention | Drop old segments, lower `retention_seconds` / `max_segments` |
| Overlay stuck on "Finishing" | Backend hang | Press hotkey to cancel; check trace for missing `vocis.transcribe.finalize` |

## Repo layout

If you're contributing, start with the docs:

1. [`docs/overview.md`](docs/overview.md) — what the product does and key constraints
2. [`docs/architecture.md`](docs/architecture.md) — which packages own which behavior
3. [`docs/runtime-flow.md`](docs/runtime-flow.md) — detailed execution path
4. [`docs/debugging.md`](docs/debugging.md) — logs, tracing, diagnostic tips
5. [`docs/lemonade.md`](docs/lemonade.md) — Lemonade API notes and quirks
6. [`docs/silero.md`](docs/silero.md) — Silero VAD design

Project rules live in [`AGENTS.md`](AGENTS.md). The most important one:
**Do not report work as done until verified locally.**

## Notes

- The overlay is intentionally small and non-interactive (Escape only
  during finishing).
- Clipboard restore is enabled by default after paste.
- The app assumes an unlocked desktop session with access to your keyring.
- Audio ducking uses `wpctl` and requires PipeWire or PulseAudio.
- "Wokis" — a name that sometimes shows up — is just the classical-Latin
  pronunciation of `vocis` written phonetically. Same thing.
