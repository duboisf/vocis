# Architecture

This repo is intentionally small. Code is split by responsibility, with platform-specific code isolated behind interfaces.

## Layering

```
cmd/vocis/          ← CLI entrypoint, wires platform deps
    ↓
internal/app/       ← orchestration via interfaces (no platform imports)
    ↓
internal/hotkey/    ← state machine, parsing (platform-agnostic)
internal/ui/        ← drawing, text, easing (platform-agnostic)
internal/openai/    ← transcription (OpenAI + Lemonade backends) + post-processing
internal/recorder/  ← PulseAudio capture
internal/audio/     ← volume ducking
    ↓
internal/platform/
  target.go         ← shared Target type
  x11/              ← X11 implementations (xgbutil, xdotool)
```

The `app` package defines the interfaces it consumes (`OverlayUI`, `InjectorClient`, `HotkeySource`). It never imports platform-specific packages. `cmd/vocis/serve.go` creates the X11 implementations and injects them via `app.New(cfg, deps)`.

A future Wayland backend would add `internal/platform/wayland/` satisfying the same interfaces, with a `serve_wayland.go` that wires them up.

## Top-level entry points

- [`cmd/vocis/`](/home/fred/git/vtt/cmd/vocis/): Cobra-based CLI with one file per command (`root.go`, `serve.go`, `init_cmd.go`, `doctor.go`, `key.go`)
- [`README.md`](/home/fred/git/vtt/README.md): user-facing setup and usage
- [`config.example.yaml`](/home/fred/git/vtt/config.example.yaml): config shape example

## Platform-agnostic packages

- [`internal/app/app.go`](/home/fred/git/vtt/internal/app/app.go): orchestration — hotkey events, recorder lifecycle, overlay updates, submit mode, post-processing, insertion. Consumes platform deps through interfaces.
- [`internal/hotkey/state.go`](/home/fred/git/vtt/internal/hotkey/state.go): hotkey state machine — down/up/tap detection, auto-repeat filtering, lock/unlock, suppression. No X11 imports.
- [`internal/hotkey/parse.go`](/home/fred/git/vtt/internal/hotkey/parse.go): shortcut string parsing (`"ctrl+shift+space"` → key names), modifier mapping.
- [`internal/ui/draw.go`](/home/fred/git/vtt/internal/ui/draw.go): pixel-level drawing primitives — `WriteText`, `DrawRect`, `DrawBars`, `BlendFrames`, `HeartbeatPulse`, easing functions. Operates on `image.RGBA`, no X11 dependency.
- [`internal/ui/text.go`](/home/fred/git/vtt/internal/ui/text.go): text utilities — `Shorten`, `WrapLines`, `TextLimit`, `ShouldAnimatePartial`, `ListeningBody`.
- [`internal/config/config.go`](/home/fred/git/vtt/internal/config/config.go): default config, validation, load/save, template expansion for overlay text.
- [`internal/recorder/recorder.go`](/home/fred/git/vtt/internal/recorder/recorder.go): in-process PulseAudio/PipeWire microphone capture and live sample stream.
- [`internal/openai/transcribe.go`](/home/fred/git/vtt/internal/openai/transcribe.go): realtime transcription orchestration — connect with retries, buffered audio handoff, turn assembly, finalization. Backend-agnostic; differences live behind `Transport`.
- [`internal/openai/transport.go`](/home/fred/git/vtt/internal/openai/transport.go): `Transport` interface (Dial, SessionUpdate, SampleRate) — abstracts backend-specific connect, auth, payload shape, and PCM rate.
- [`internal/openai/transport_openai.go`](/home/fred/git/vtt/internal/openai/transport_openai.go): OpenAI realtime transport — ephemeral client secret auth, 24 kHz PCM, nested `session.audio.input.transcription` payload.
- [`internal/openai/transport_lemonade.go`](/home/fred/git/vtt/internal/openai/transport_lemonade.go): local Lemonade Server transport — no auth, 16 kHz PCM, flat `session.model` payload, model passed via `?model=` query.
- [`internal/openai/postprocess.go`](/home/fred/git/vtt/internal/openai/postprocess.go): LLM post-processing to clean up filler words and hesitations. Backend-agnostic — uses OpenAI-compatible `/chat/completions`, so it works against either backend via `base_url`.
- [`internal/audio/duck.go`](/home/fred/git/vtt/internal/audio/duck.go): lower speaker volume during recording via `wpctl`.
- [`internal/securestore/keyring.go`](/home/fred/git/vtt/internal/securestore/keyring.go): keyring-backed API key storage.
- [`internal/sessionlog/sessionlog.go`](/home/fred/git/vtt/internal/sessionlog/sessionlog.go): per-session logs on disk with DEBUG/INFO/WARN/ERROR levels.
- [`internal/telemetry/telemetry.go`](/home/fred/git/vtt/internal/telemetry/telemetry.go): OpenTelemetry tracing (OTLP/gRPC exporter, span helpers).

## Platform-specific packages

- [`internal/platform/target.go`](/home/fred/git/vtt/internal/platform/target.go): shared `Target` type (window ID, class, name) — platform-agnostic struct used by both app and platform backends.
- [`internal/platform/x11/overlay.go`](/home/fred/git/vtt/internal/platform/x11/overlay.go): X11 overlay — xgbutil window, xgraphics rendering, Xinerama monitor detection, Escape key grab. Uses `ui.*` for drawing and text.
- [`internal/platform/x11/hotkeys.go`](/home/fred/git/vtt/internal/platform/x11/hotkeys.go): X11 global hotkey — keybind grab, xevent loop. Thin wrapper (~100 lines) that feeds raw events into `hotkey.State`.
- [`internal/platform/x11/injector.go`](/home/fred/git/vtt/internal/platform/x11/injector.go): X11 text injection — xdotool for focus/paste/type/Enter, clipboard handling.

## Package relationships

- `serve.go` creates X11 implementations and injects them into `app.New()`
- `app` owns the runtime lifecycle through interfaces — never imports platform packages
- `hotkey.State` receives raw press/release events from any backend
- `ui.*` provides drawing primitives to any overlay backend
- `platform/x11/*` implements `OverlayUI`, `InjectorClient`, and `HotkeySource` for X11
- `openai.DictationSession` owns realtime stream readiness, connect retries, turn assembly
- `openai.PostProcess` handles LLM cleanup with timeout and fallback

## Useful rule of thumb

- if the bug is about when recording starts or stops → `app` and `hotkey`
- if the bug is about missed words or audio timing → `recorder` and `openai`
- if the bug is about pasting into the wrong place → `platform/x11/injector`
- if the bug is about what the user sees → `platform/x11/overlay` and `ui`
- if the bug is about post-processing quality → check the prompt in `config`
- if the bug is about volume levels → `audio`
- if the bug is about hotkey detection or auto-repeat → `hotkey/state.go`

If you need execution details rather than file ownership, continue to `runtime-flow.md`.
