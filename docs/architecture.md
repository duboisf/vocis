# Architecture

This repo is intentionally small. The main runtime path is split by responsibility rather than by many layers.

Top-level entry points:

- [`cmd/vocis/main.go`](/home/fred/git/vtt/cmd/vocis/main.go): CLI entrypoint for `serve`, `init`, `doctor`, and key management
- [`README.md`](/home/fred/git/vtt/README.md): user-facing setup and usage
- [`config.example.yaml`](/home/fred/git/vtt/config.example.yaml): config shape example

Primary runtime packages:

- [`internal/app/app.go`](/home/fred/git/vtt/internal/app/app.go): orchestration for hotkeys, recorder lifecycle, overlay updates, and insertion
- [`internal/config/config.go`](/home/fred/git/vtt/internal/config/config.go): default config, validation, load/save logic
- [`internal/hotkeys/hotkeys.go`](/home/fred/git/vtt/internal/hotkeys/hotkeys.go): global X11 hotkey registration and hold/toggle behavior
- [`internal/overlay/overlay.go`](/home/fred/git/vtt/internal/overlay/overlay.go): always-on-top X11 overlay rendering
- [`internal/recorder/recorder.go`](/home/fred/git/vtt/internal/recorder/recorder.go): in-process PulseAudio/PipeWire microphone capture and live sample stream
- [`internal/openai/transcribe.go`](/home/fred/git/vtt/internal/openai/transcribe.go): OpenAI realtime transcription session setup, buffered audio handoff, turn assembly, and final flush decisions
- [`internal/injector/injector.go`](/home/fred/git/vtt/internal/injector/injector.go): focus restore, clipboard handling, and paste/type insertion
- [`internal/securestore/keyring.go`](/home/fred/git/vtt/internal/securestore/keyring.go): keyring-backed API key storage
- [`internal/sessionlog/sessionlog.go`](/home/fred/git/vtt/internal/sessionlog/sessionlog.go): per-session logs on disk
- [`internal/telemetry/telemetry.go`](/home/fred/git/vtt/internal/telemetry/telemetry.go): OpenTelemetry tracing (OTLP/gRPC exporter, span helpers)

Package relationships:

- `main` loads config and starts `app`
- `app` owns the runtime lifecycle
- `app` depends on `hotkeys`, `overlay`, `recorder`, `openai`, `injector`, `securestore`, and `telemetry`
- `openai.DictationSession` owns realtime stream readiness, early-audio buffering, segmented turn assembly, and release-time finalization
- `recorder` and `openai` are the audio/transcription edge
- `injector` is the desktop automation edge

Useful rule of thumb:

- if the bug is about when recording starts or stops, start in `app` and `hotkeys`
- if the bug is about missed words, segmented turns, or audio timing, start in `recorder` and `openai`
- if the bug is about pasting into the wrong place or wrong shortcut, start in `injector`
- if the bug is about what the user sees, start in `overlay`

If you need execution details rather than file ownership, continue to `runtime-flow.md`.
