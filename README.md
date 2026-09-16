# vocis

*vocis* — Latin genitive of *vox* ("of voice"), pronounced **WOH-kiss** in
classical Latin (the "v" is a "w", the "c" is hard like "k", and the "i" is
short).

`vocis` is a Linux voice-to-text desktop helper written in Go. Hold a global
hotkey, speak, release — the transcript is pasted back into the app you were
already using. Local-first: NPU-accelerated transcription via Lemonade Server
running Gemma 4, no API key, no network. An always-on-top X11 overlay gives
you the same "record / transcribe / typed" rhythm that keeps dictation
feeling fast.

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

## How a dictation works

1. Hotkey down: the mic opens immediately, the model is preflighted on
   Lemonade, and the focused window is captured as the paste target.
2. While you hold the key, Silero VAD cuts the audio into clips at
   pauses (and at a 28 s per-clip cap, Gemma's 30 s audio limit with
   margin). Nothing is sent yet.
3. Release: silent clips are dropped, the rest are merged back together
   under the cap and sent in **one** `/chat/completions` request, each
   clip as its own `input_audio` part. The reply streams into the
   overlay as it arrives.
4. The transcript is pasted once into the target window, and appended to
   `~/.local/state/vocis/transcripts.jsonl` with the path of the captured
   audio.

The whole dictation hits the model exactly once. There is no cleanup pass
and nothing is re-transcribed; punctuation, casing, digits and vocabulary
come from `transcription.prompt` / `prompt_hint`.

## Cool features

### Both modes

- **Local-first transcription.** [Lemonade Server](https://github.com/lemonade-sdk/lemonade)
  with `gemma4-it-e2b-FLM` running on NPU via FLM — no API key, no
  network. Gemma's native audio mode is driven through the
  OpenAI-compatible `/chat/completions` endpoint.
- **Prompt-driven output.** The system prompt asks for punctuated prose,
  digits for numbers and a short developer vocabulary (so "pull request"
  never comes out as "poll request"). Edit `transcription.prompt` and
  `prompt_hint` to taste; both reload on every dictation.
- **Mic preroll during model preflight.** On a cold model, vocis opens
  the mic *before* the 5–10 s NPU load and replays those samples into the
  session once it's ready, so the first words after you press the hotkey
  aren't lost.
- **Energy gate that trusts VAD.** Silent clips are dropped before the
  request (peak / RMS thresholds), but when Silero saw speech only the
  peak arm applies — a long pause before a short phrase no longer drags
  the phrase below the floor.
- **Hallucination filters** for stock phrases audio models love to emit
  on silence ("Thank you.", "Thanks for watching.", lone "you").
- **Audio ducking.** Speaker volume drops to 10% while the mic is hot so
  you don't transcribe your own playback.
- **Audio capture for replay.** Every request's WAV is mirrored to
  `~/.local/state/vocis/audio/` for one hour so a bad transcript can be
  replayed against a new prompt or model.
- **Strict config loader.** Unknown YAML keys fail at startup instead of
  drifting silently. Keys removed in a release are stripped with a `WARN`
  that names the replacement.

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
- **Cancel while finishing.** Press the hotkey again during "Finishing"
  to discard the in-flight transcription.
- **Streaming overlay.** A dB level meter while you talk, a heartbeat
  and elapsed counter while the model answers, and the reply typed into
  the overlay word by word as the SSE deltas arrive.
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
- **Loading-model overlay** during Lemonade's cold preflight, showing
  `○ Loading gemma4-it-e2b-FLM...` so you know why the session hasn't
  started yet.
- **Transcript history.** One JSON line per delivered dictation in
  `transcription.history_file` (time, text, window class, audio duration,
  captured WAV path) — the record to audit transcription quality against.

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
- **`recall last <duration>`** — sends every segment in the window as its
  own labelled `input_audio` part in one `/chat/completions` request and
  gets back one `HH:MM:SS<TAB>transcript` line per segment. Long windows
  are packed into sub-batches sized from the model's context window.
  No client-side timeout; Ctrl-C cleanly cancels.
- **`recall replay`** pipes raw segment PCM into `paplay` so you can
  hear what the daemon actually captured before deciding whether to drop
  it. Streamed through one persistent paplay process so back-to-back
  segments don't pop.
- **`recall drop`** removes segments (and their on-disk JSON when
  persistence is on).
- **Min peak / RMS filters** reject Silero-VAD false positives on fan
  hum, keyboard clicks, and room tone.

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
./bin/vocis serve     # local Lemonade — no key needed
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

## Backend

[Lemonade Server](https://github.com/lemonade-sdk/lemonade) exposes an
OpenAI-compatible REST API. vocis talks to `/chat/completions` with the
audio embedded as `input_audio` content parts. Defaults:

- `transcription.base_url: http://localhost:13305/api/v1`
- `transcription.model: gemma4-it-e2b-FLM` (NPU on Ryzen AI via FLM)
- `transcription.language: its original language` — substituted into the
  prompt; set `en`, `fr`, ... to force one

`vocis config backend` autodetects a running Lemonade on localhost and
writes `base_url` and the default model. `vocis config models` lists the
audio-capable models Lemonade knows with download status so you can pick
one. Lemonade keeps one LLM loaded at a time; vocis force-loads the
configured model at startup and re-checks before every dictation.

On the same NPU, `gemma4-it-e4b-FLM` transcribes about 2× slower for a
modest word-error-rate gain and `whisper-v3-turbo-FLM` takes no prompt;
E2B is the default for a reason.

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
  Wayland-native windows in some compositors. When no X server is
  reachable at all, `serve` runs without an overlay and logs
  `overlay backend: none`.

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

You also need [Lemonade Server](https://github.com/lemonade-sdk/lemonade)
running, and ONNX Runtime for Silero VAD — see `docs/silero.md` for the
discovery rules and `transcription.silero.onnxruntime_library` to pin the
path. Without ONNX Runtime, dictation still works; clips are only cut at
the 28 s cap.

## Config

The first run creates `~/.config/vocis/config.yaml`. A sample lives at
`config.example.yaml`. Running `vocis config init` when a config exists
opens Neovim in diff mode so you can merge new defaults.

`vocis config init` opens **`nvim`** in diff mode (hardcoded — install
`nvim` to use this flow, or `--force` to overwrite without diffing).

Other `config` subcommands:

- `vocis config backend` — probe a running Lemonade on localhost and
  write `base_url` plus the default model.
- `vocis config models` — interactive picker for the transcription model.
- `vocis config edit` — open the config file in `$VISUAL` / `$EDITOR`
  (falls back to `nvim`/`vim`/`nano`).

A few useful fields (see `config.example.yaml` for everything):

- `hotkey`: global shortcut, e.g. `ctrl+shift+space`
- `hotkey_mode`: `hold` or `toggle`
- `transcription.prompt` / `transcription.prompt_hint`: the system
  message, reloaded every dictation
- `transcription.language`: substituted into the prompt
- `transcription.hallucination_filters`: exact-match phrases to drop
- `transcription.min_chunk_peak` / `min_chunk_rms`: energy gate
- `transcription.ctx_size`: pin Lemonade's context window for the model
- `transcription.audio_capture.*`: WAV mirroring and its TTL
- `transcription.history_file`: transcript history, empty to disable
- `insertion.mode`: `auto`, `clipboard`, or `type`
- `insertion.auto_submit`: every dictation submits by default
- `insertion.kitty_remote_control`: focus-free kitty delivery (default
  `true`)
- `recall.retention_seconds` / `recall.max_segments`: ring-buffer bounds
- `recall.persist.mode`: `in_memory` (default) or `disk`
- `speak.model` / `speak.voice`: Kokoro defaults
- `telemetry.enabled` / `telemetry.endpoint`: OpenTelemetry tracing

`transcription.*`, `recording.device` and `log_window_title` reload on
each recording start. Hotkey, insertion and telemetry settings need a
restart of `serve`. Overlay text and layout are Go constants in
`internal/ui`.

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
├── vocis.recorder.stop
├── vocis.transcribe.finalize
│   └── vocis.transcribe.chat_audio.chunk   ← the single /chat/completions POST
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
- `chunk.clip_count`, `chunk.duration_ms`, `chunk.request_bytes` and
  `chunk.response_text` on `vocis.transcribe.chat_audio.chunk`

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

`docs/debugging.md` lists the log lines that mark each code path.

### Replaying a bad transcript

Find the dictation in `~/.local/state/vocis/transcripts.jsonl`, take its
`audio` path (kept for `transcription.audio_capture.ttl_seconds`, one
hour by default), and POST that WAV to Lemonade with a changed prompt or
model. Same audio, different prompt, is the only honest way to tell a
prompt problem from a model problem.

### Doctor

`vocis doctor` runs a one-shot health check covering display, xdotool,
xclip, audio, config, log dir, the gnome extension, and whether the
configured transcription model is downloaded and currently resident on
Lemonade.

### Common issues

| Symptom | Cause | Fix |
|---|---|---|
| `serve` exits at startup with "lemonade not reachable" | Lemonade Server not running | `lemonade-server serve`, then restart vocis |
| First few words missing on cold Lemonade | Model preflight took longer than mic preroll buffer | Wait for the "Ready" subtitle before speaking, or dictate once to warm the model |
| Second half of a dictation missing | Energy gate dropped a mostly-silent clip | Check the log for `dropped silent clip`; lower `min_chunk_rms`, or update — newer builds skip the RMS arm when VAD saw speech |
| Transcript has no punctuation or capitalization | Prompt does not ask for it | Keep the default `transcription.prompt`, or add the "proper prose" rule to yours |
| A word keeps coming out wrong ("poll request") | Homophone | Add it to the vocabulary list in `prompt_hint` |
| Kitty paste landed in wrong tab | `kitty @ ls` not reachable from vocis | Check `KITTY_LISTEN_ON`; configure `allow_remote_control yes` and `listen_on` in `kitty.conf` |
| Submit-mode Enter goes to wrong window | Compositor focus path used instead of kitty | Confirm `target.KittyWindowID` is set on the trace; if empty, see above |
| Recall daemon eating disk | `persist.mode: disk` + long retention | Drop old segments, lower `retention_seconds` / `max_segments` |
| Overlay stuck on "Finishing" | Backend hang | Press hotkey to cancel; check trace for missing `vocis.transcribe.finalize` |

## Repo layout

If you're contributing, start with the docs:

1. [`docs/overview.md`](docs/overview.md) — what the product does and key constraints
2. [`docs/architecture.md`](docs/architecture.md) — which packages own which behavior
3. [`docs/runtime-flow.md`](docs/runtime-flow.md) — detailed execution path
4. [`docs/debugging.md`](docs/debugging.md) — logs, tracing, diagnostic tips
5. [`docs/silero.md`](docs/silero.md) — Silero VAD design

Project rules live in [`AGENTS.md`](AGENTS.md). The most important one:
**Do not report work as done until verified locally.**

## Notes

- The overlay is intentionally small and non-interactive.
- Clipboard restore is enabled by default after paste.
- Audio ducking uses `wpctl` and requires PipeWire or PulseAudio.
- "Wokis" — a name that sometimes shows up — is just the classical-Latin
  pronunciation of `vocis` written phonetically. Same thing.
