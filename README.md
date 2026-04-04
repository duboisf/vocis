# vocis

*vocis* — Latin genitive of *vox* ("of voice"), pronounced **WOH-kiss** in
classical Latin (the "v" is a "w", the "c" is hard like "k", and the "i" is
short).

`vocis` is a Linux voice-to-text desktop helper written in Go. It stays in the
background, listens for a global hotkey, streams your microphone audio to
OpenAI for realtime transcription, and pastes the result back into the app
you were already using. An always-on-top X11 overlay gives you the same
"record / transcribe / typed" rhythm that keeps dictation feeling fast.

It was very much vibe-coded from scratch to... scratch an itch.

## What it does

- Global hotkey driven dictation on Linux X11
- Hold-to-record by default, with optional toggle mode
- Overlay appears immediately on hotkey press
- Local recording starts before target-window lookup finishes
- Early audio is buffered locally until the OpenAI realtime session is ready
- WebSocket connect retries up to 3 times on transient failures
- Live segmented transcription appears in the overlay as you speak, pasted once on release
- LLM post-processing cleans up filler words and hesitations before pasting
- Submit mode: tap Space during recording to auto-press Enter after paste
- Audio ducking: lowers speaker volume while recording
- Connection status shown in overlay (connecting, reconnecting, connected)
- Configurable overlay text, font, and font size
- Shell completion for bash, zsh, fish, and powershell
- Terminal-aware paste key support
- Focus restore and paste back into the app that was active when recording
- XDG config file at `~/.config/vocis/config.yaml`
- OpenAI API keys stored in the system keyring by default

## Dependencies

`vocis` records audio in-process over PulseAudio / PipeWire. It still shells out
to a few stable desktop tools for X11 automation:

- `xdotool` for focus restore, simulated paste, and Enter keypress
- `xclip` for clipboard-based insertion
- `wpctl` for audio ducking (PipeWire/PulseAudio volume control)

The app currently targets Linux X11. Wayland support is not wired up yet.

## Quick start

```bash
make build
./bin/vocis init
./bin/vocis key set
./bin/vocis serve
```

Generate shell completions:

```bash
./bin/vocis completion bash > ~/.local/share/bash-completion/completions/vocis
# or for zsh:
./bin/vocis completion zsh > ~/.zfunc/_vocis
```

While `vocis serve` is running:

1. Focus any text field.
2. Hold `Ctrl+Shift+Space`.
3. Speak.
4. Release the hotkey to stop and insert the transcript.

### Submit mode

While recording (Ctrl+Shift held), tap Space to toggle submit mode. The
overlay shows a throbbing yellow "⏎ submit" indicator. When you release,
the text is pasted and Enter is pressed automatically — useful for submitting
prompts in tools like Claude Code.

### Escape to skip post-processing

While the overlay shows "Finishing", press Escape to skip post-processing
and paste the raw transcription immediately.

## Config

The first run creates `~/.config/vocis/config.yaml`. A sample lives at
`config.example.yaml`. Running `vocis init` when a config exists opens
Neovim in diff mode so you can merge new defaults.

Important fields:

- `hotkey`: global shortcut, for example `ctrl+shift+space`
- `hotkey_mode`: `hold` or `toggle`, defaults to `hold`
- `log_window_title`: log the title of the target window (default `false`)
- `openai.model`: defaults to `gpt-4o-mini-transcribe`
- `openai.organization`: optional org id when your key can access multiple orgs
- `openai.project`: optional project id when your key should bill a specific project
- `openai.language`: optional ISO-639-1 hint such as `en`
- `openai.prompt_hint`: terminology and style hint for transcription (has a sensible default)
- `recording.backend`: currently `auto` or `pulse`
- `recording.device`: PulseAudio source name, or `default`
- `recording.duck_volume`: lower speaker volume to this level while recording (0.0–1.0, default `0.1`; set to `0` to disable)
- `streaming.show_partial_overlay`: show live partial text in the overlay while speaking
- `streaming.silence_duration_ms`: pause length that ends a segment
- `streaming.prefix_padding_ms`: audio kept ahead of each detected phrase
- `streaming.threshold`: VAD sensitivity from `0.0` to `1.0`
- `insertion.mode`: `auto`, `clipboard`, or `type`
- `overlay.font`: font name resolved via `fc-match` (default: system monospace)
- `overlay.font_size`: font size in points (default: `13`)
- `overlay.branding`: text shown in top-right corner (default: `"Vocis"`)
- `overlay.*`: all overlay text is configurable — see `config.example.yaml` for the full structure
- `postprocess.enabled`: enable LLM cleanup of transcription (default: `true`)
- `postprocess.model`: model for cleanup (default: `gpt-4o-mini`)
- `postprocess.prompt`: system prompt for cleanup (has a sensible default)
- `postprocess.min_word_count`: skip cleanup for short phrases (default: `10`)

Config is reloaded on each recording start, so changes take effect without restarting.

## Secrets

Use the system keyring so the API key is not stored in plain text:

```bash
./bin/vocis key set
```

For one-off sessions you can also export `OPENAI_API_KEY`.

## Troubleshooting

### Tracing with Jaeger

Tracing is the first place to look when something goes wrong. Enable it in config:

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

Open http://localhost:16686, select the `vocis` service, and search for traces.

Each dictation session produces one trace with these spans:

```
vocis.dictation                    ← root span (entire session lifecycle)
├── vocis.recorder.start           ← PulseAudio init (~10ms)
├── vocis.inject.capture_target    ← window ID lookup (~6ms)
├── vocis.recording.active         ← user speaking (variable)
├── vocis.openai.connect           ← WebSocket + TLS (~600ms, retries shown separately)
├── vocis.recorder.stop            ← stream flush (~120ms)
├── vocis.transcribe.finalize      ← post-recording finalization
│   ├── vocis.transcribe.drain     ← collect pending segments (250ms window)
│   ├── vocis.transcribe.commit    ← commit trailing audio to OpenAI
│   └── vocis.transcribe.wait_final ← wait for trailing transcript
├── vocis.postprocess              ← LLM cleanup (~1-2s)
└── vocis.inject                   ← text insertion
    ├── vocis.inject.focus         ← window activate + modifier release
    └── vocis.inject.paste         ← clipboard paste or xdotool type
```

**What to look for:**

- `vocis.openai.connect` with ERROR → network issue connecting to OpenAI
- `vocis.transcribe.commit` with `commit.skipped=true` → all audio consumed by segments (normal)
- `vocis.transcribe.wait_final` with `trailing.skipped=true` → no trailing audio to transcribe (normal)
- `vocis.postprocess` with `skipped=true` → post-processing failed or was cancelled
- Missing spans → the process hung or was cancelled before reaching that phase
- Missing `vocis.dictation` root span → `finishRecording` never completed (check if the process was stuck)

**Fetching traces via API:**

```bash
# Get a specific trace as JSON
curl -s http://localhost:16686/api/traces/<traceID> | python3 -m json.tool

# List recent traces
curl -s 'http://localhost:16686/api/traces?service=vocis&limit=10&lookback=1h'
```

### Session logs

Each `serve` session writes a log file under `~/.local/state/vocis/sessions/`. Check the latest:

```bash
tail -50 "$(ls -t ~/.local/state/vocis/sessions/*.log | head -1)"
```

Key log patterns:

- `realtime transcription stream ready` → WebSocket connected successfully
- `connect attempt 2/3 failed` → retrying after transient failure
- `transcribe failed` → finalization error (check the trace for details)
- `postprocess skipped words=N min=M` → text too short for cleanup
- `submit mode enabled/disabled` → Space tap toggle
- `transcription cancelled by user` → user pressed hotkey during finishing
- `voice command detected` → `[ENTER]` token found (legacy, currently disabled)

### Common issues

| Symptom | Cause | Fix |
|---------|-------|-----|
| Overlay appears but nothing happens | WebSocket connect failed | Check Jaeger trace for `vocis.openai.connect` error |
| Text pasted in wrong window | Focus shifted during finalization | Target window ID is logged; check if xdotool activated it |
| Post-processing too aggressive | Prompt removes too much | Edit `postprocess.prompt` in config |
| Mic volume keeps dropping | External app adjusting gain | Check `wpctl status` for Zoom or other apps |
| Overlay stuck on "Finishing" | Deadlock or timeout | Press hotkey to cancel; check trace for missing spans |
| Enter not pressed in submit mode | Focus on wrong window | Check log for `submit mode: pressing Enter on window=` |

## Notes

- The overlay is intentionally small and non-interactive (except Escape during finishing).
- Clipboard restore is enabled by default after paste.
- The app assumes an unlocked desktop session with access to your keyring.
- Audio ducking uses `wpctl` and requires PipeWire or PulseAudio.
