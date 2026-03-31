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
- XDG config file at `~/.config/vocis/config.json`
- OpenAI API keys stored in the system keyring by default
- Focus restore and paste back into the app that was active when recording
- Segmented streaming mode: live transcription appears in the overlay as you speak, pasted once on release
- Terminal-aware paste key support
- Overlay with system monospace font, smooth vertical resize, and segment-per-line display

## Dependencies

`vocis` records audio in-process over PulseAudio / PipeWire. It still shells out
to a few stable desktop tools for X11 automation:

- `xdotool` for focus restore and simulated paste
- `xclip` for clipboard-based insertion

The app currently targets Linux X11. Wayland support is not wired up yet.

## Quick start

```bash
make build
./bin/vocis init
./bin/vocis key set
./bin/vocis serve
```

While `vocis serve` is running:

1. Focus any text field.
2. Hold `Ctrl+Shift+Space`.
3. Speak.
4. Release the hotkey to stop and insert the transcript.

Expected startup behavior on key-down:

1. The overlay appears immediately.
2. Local microphone capture starts right away.
3. The app looks up the current target window while audio is already being captured.
4. If the OpenAI realtime websocket is not ready yet, audio is buffered in memory.
5. Once the realtime session is ready, buffered audio is flushed first and live audio continues streaming after it.

## Config

The first run creates `~/.config/vocis/config.json`. A sample lives at
`config.example.json`.

Important fields:

- `hotkey`: global shortcut, for example `ctrl+shift+space`
- `hotkey_mode`: `hold` or `toggle`, defaults to `hold`
- `openai.model`: defaults to `gpt-4o-mini-transcribe`
- `openai.organization`: optional org id when your key can access multiple orgs
- `openai.project`: optional project id when your key should bill a specific project
- `openai.language`: optional ISO-639-1 hint such as `en`
- `openai.prompt_hint`: optional terminology and style hint for transcription
- `recording.backend`: currently `auto` or `pulse`
- `recording.device`: PulseAudio source name, or `default`
- `streaming.mode`: `release` or `segment`
- `streaming.show_partial_overlay`: show live partial text in the overlay while speaking
- `streaming.silence_duration_ms`: pause length that ends a segment in `segment` mode
- `streaming.prefix_padding_ms`: audio kept ahead of each detected phrase in `segment` mode
- `streaming.threshold`: VAD sensitivity from `0.0` to `1.0` in `segment` mode
- `insertion.mode`: `auto`, `clipboard`, or `type`

## Secrets

Use the system keyring so the API key is not stored in plain text:

```bash
./bin/vocis key set
```

For one-off sessions you can also export `OPENAI_API_KEY`.

## Notes

- The overlay is intentionally small and non-interactive.
- Clipboard restore is enabled by default after paste.
- The app assumes an unlocked desktop session with access to your keyring.
