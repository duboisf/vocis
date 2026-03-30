# vtt

`vtt` is a Linux voice-to-text desktop helper written in Go. It stays in the
background, listens for a global hotkey, streams your microphone audio to
OpenAI for realtime transcription, and pastes the result back into the app
you were already using. An always-on-top X11 overlay gives you the same
"record / transcribe / typed" rhythm that keeps dictation feeling fast.

It was very much vibe-coded from scratch to... scratch an itch.

## What it does

- Global hotkey driven dictation on Linux X11
- Hold-to-record by default, with optional toggle mode
- XDG config file at `~/.config/vtt/config.json`
- OpenAI API keys stored in the system keyring by default
- Focus restore and paste back into the app that was active when recording
- Terminal-aware paste key support
- Lightweight overlay for listening, transcribing, success, and errors

## Dependencies

`vtt` records audio in-process over PulseAudio / PipeWire. It still shells out
to a few stable desktop tools for X11 automation:

- `xdotool` for focus restore and simulated paste
- `xclip` for clipboard-based insertion

The app currently targets Linux X11. Wayland support is not wired up yet.

## Quick start

```bash
make build
./bin/vtt init
./bin/vtt key set
./bin/vtt serve
```

While `vtt serve` is running:

1. Focus any text field.
2. Hold `Ctrl+Shift+Space`.
3. Speak.
4. Release the hotkey to stop and insert the transcript.

## Config

The first run creates `~/.config/vtt/config.json`. A sample lives at
[`config.example.json`](/home/fred/git/vtt/config.example.json).

Important fields:

- `hotkey`: global shortcut, for example `ctrl+shift+space`
- `hotkey_mode`: `hold` or `toggle`, defaults to `hold`
- `openai.model`: defaults to `gpt-4o-mini-transcribe`
- `openai.organization`: optional org id when your key can access multiple orgs
- `openai.project`: optional project id when your key should bill a specific project
- `openai.language`: optional ISO-639-1 hint such as `en`
- `openai.vocabulary`: domain-specific spellings to bias transcription
- `recording.backend`: currently `auto` or `pulse`
- `recording.device`: PulseAudio source name, or `default`
- `insertion.mode`: `auto`, `clipboard`, or `type`

## Secrets

Use the system keyring so the API key is not stored in plain text:

```bash
./bin/vtt key set
```

For one-off sessions you can also export `OPENAI_API_KEY`.

## Notes

- The overlay is intentionally small and non-interactive.
- Clipboard restore is enabled by default after paste.
- The app assumes an unlocked desktop session with access to your keyring.
