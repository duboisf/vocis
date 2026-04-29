# Overview

`vocis` is a Linux X11 voice-to-text helper written in Go.

At a high level:

- a global hotkey starts dictation
- the overlay appears immediately with connection status
- local microphone capture starts first
- target-window capture happens after local recording has already started
- early audio is buffered while the OpenAI realtime transcription session connects (retries up to 3 times)
- buffered audio is flushed into the realtime session as soon as it is ready
- audio is streamed to OpenAI after that
- completed phrases accumulate in the overlay as you speak (one line per segment)
- on release or stop, the dictation session collects any trailing audio
- the text is optionally cleaned up by an LLM (post-processing via gpt-4o-mini)
- the accumulated text plus any trailing transcript is inserted back into the previously focused app as a single paste
- if submit mode was toggled on during recording, Enter is pressed after paste

Important constraints:

- Linux X11 only for now
- PulseAudio / PipeWire input capture
- OpenAI API key stored in the system keyring or provided by `OPENAI_API_KEY`
- overlay is intentionally lightweight and non-interactive (except Escape during finishing)

Core product choices:

- `hold` mode is the default hotkey behavior
- `toggle` mode is also supported by config
- very short recordings are silently discarded
- terminal windows use a terminal-safe paste shortcut
- transcription is realtime-streamed, not uploaded from a WAV file
- turn assembly and trailing-flush decisions live in the OpenAI dictation session, not in the app layer
- post-processing cleans up filler words and hesitations but never answers questions or changes intent
- config is reloaded on each recording start — no restart needed for most changes
- all overlay text is configurable via named templates in the config file
- audio ducking lowers speaker volume during recording to avoid mic feedback

## Modes

`vocis` has two dictation modes plus a one-shot text-to-speech command,
run as separate subcommands:

- **`vocis serve`** — the push-to-talk default described above: hotkey
  starts/stops a dictation session, overlay is shown, transcript is
  pasted into the focused app.
- **`vocis speak [text]`** — sends text to Lemonade's
  OpenAI-compatible `/audio/speech` endpoint (Kokoro TTS) and streams
  the resulting PCM through `paplay`. Reads text from args or stdin,
  so you can pipe `vocis recall last 5m | vocis speak`. Voice and
  model live under `speak:` in the config; `--voice` / `--model`
  override per call. With `--out PATH`, the audio is written as a
  24 kHz mono PCM16 WAV instead of being played.
- **`vocis recall`** (Wokis Recall) — always-on capture. The daemon
  (`vocis recall start`) continuously records the mic, segments speech
  with Silero VAD, and keeps a bounded ring buffer of recent utterances.
  `vocis recall pick` shows the recent segments in a terminal picker and
  transcribes the chosen one on demand. `vocis recall last <duration>`
  concatenates every segment whose start falls within the window
  (`10m`, `2h`, `1h30m`) and sends the combined audio through a single
  transcription request — one realtime session instead of N, with the
  ASR keeping context across segment boundaries. No live transcription
  happens until you pick or run `last` — cheap when idle, slight latency
  on pick.

  The ring buffer is memory-only by default — kill the daemon and the
  buffer is gone, which keeps an always-on mic from leaving audio on
  disk unintentionally. Flip `recall.persist.mode` to `disk` to opt
  into on-disk persistence; each segment mirrors to
  `<recall.persist.dir>/seg-<id>.json`, with retention applied on
  reload. `recall.persist.dir` defaults to
  `$XDG_STATE_HOME/vocis/recall` (or `~/.local/state/vocis/recall`).

  Retention, ring-buffer size, and persistence live under `recall:` in
  the config.

If you only need the "what is this thing" version of the repo, stop here.
