# Runtime Flow

This page is the detailed path for one dictation session.

## Startup

When `vocis serve` runs:

1. [`cmd/vocis/main.go`](/home/fred/git/vtt/cmd/vocis/main.go) starts a session log.
2. [`internal/config/config.go`](/home/fred/git/vtt/internal/config/config.go) loads config.
3. [`internal/securestore/keyring.go`](/home/fred/git/vtt/internal/securestore/keyring.go) resolves the OpenAI API key.
4. [`internal/app/app.go`](/home/fred/git/vtt/internal/app/app.go) creates the runtime dependencies and registers the hotkey.

## Record Start

When the hotkey starts dictation:

1. [`internal/app/app.go`](/home/fred/git/vtt/internal/app/app.go) shows the overlay immediately.
2. [`internal/recorder/recorder.go`](/home/fred/git/vtt/internal/recorder/recorder.go) starts local microphone capture immediately.
3. The injector captures the active target window after capture has already started so focus can be restored later.
4. [`internal/openai/transcribe.go`](/home/fred/git/vtt/internal/openai/transcribe.go) starts a `DictationSession`.
5. The `DictationSession` creates the realtime transcription session through the official OpenAI SDK and connects the websocket.
6. Audio chunks that arrive before the websocket is ready are buffered in memory inside the dictation session.
7. Once the websocket is ready, the buffered audio is flushed first, then live audio continues streaming normally.

Why this matters:

- the overlay can pop immediately
- the user can start speaking immediately
- the beginning of speech is less likely to be clipped by window lookup or connection setup time

## Record Stop

When the hotkey stops dictation:

1. [`internal/app/app.go`](/home/fred/git/vtt/internal/app/app.go) stops local recording.
2. The overlay switches to the "Finishing" state, showing the accumulated text, "Wrapping up the last few words...", and a hint that the user can press the hotkey again to cancel.
3. The user can press the hotkey during this state to cancel the in-flight transcription.
4. [`internal/openai/transcribe.go`](/home/fred/git/vtt/internal/openai/transcribe.go) finalizes the `DictationSession`.
5. The dictation session decides whether there is any trailing audio left that still needs a final commit.
6. If segmented mode already emitted the spoken chunks and there is no trailing audio left, finalization returns without forcing an extra commit.
7. Otherwise the OpenAI stream is committed and the final transcription event is awaited. The trailing timeout is proportional to the trailing audio duration (`max(3s, trailing_duration / 5)`).
7. The accumulated segment text plus any trailing finalize text is combined and inserted as a single paste.

## Insert

After transcription completes:

1. [`internal/injector/injector.go`](/home/fred/git/vtt/internal/injector/injector.go) restores focus to the original window.
2. The transcript is inserted via clipboard paste or direct typing depending on config.
3. Terminal windows use the configured terminal paste shortcut.
4. The overlay hides immediately.

## Segmented Streaming

When `streaming.mode = "segment"`:

1. OpenAI server VAD detects pauses while the hotkey is still held.
2. Completed phrases are emitted as segment events from the dictation session.
3. [`internal/app/app.go`](/home/fred/git/vtt/internal/app/app.go) accumulates segment text in `recordingState.liveText` (for pasting) and `recordingState.displayText` (for the overlay, with newlines between segments).
4. The overlay displays each segment on a separate line, growing vertically as text accumulates. Partial transcription text is prepended with the accumulated segments so previously completed text stays visible.
5. On release, the accumulated text plus any trailing finalize text is combined and pasted into the target window as a single insertion.

Segments are never typed into the target window during recording. This avoids corrupting the X11 keymap state with `xdotool keyup` while the user is still holding the hotkey.

## Overlay Animations

The overlay uses three animation modes for transitions:

- **First appearance** (e.g., hotkey pressed when overlay is hidden): slides down 28px while fading in over 320ms. Opacity ramps linearly; slide position uses ease-out cubic. If a state change arrives mid-slide (e.g., `ShowListening` updating the window class), the content updates without interrupting the slide.
- **State transitions** (e.g., Listening → Finishing): true pixel-level crossfade over 80ms. The previous frame is captured, the new state is applied, and the two frames are alpha-blended in software.
- **Final hide** (auto-hide timer or manual dismiss): slides up 28px while fading out over 320ms with ease-in cubic for the slide.

## Tracing

When telemetry is enabled, the following OpenTelemetry spans are emitted per dictation session:

- `vocis.dictation` — root span covering the full session lifecycle
  - `vocis.recorder.start` — PulseAudio client init and stream creation
  - `vocis.openai.connect` — WebSocket dial and realtime session setup
  - `vocis.recorder.stop` — stream stop and resource cleanup
  - `vocis.transcribe.finalize` — post-recording finalization
    - `vocis.transcribe.drain` — drain pending segment finals (250ms window)
    - `vocis.transcribe.commit` — commit trailing audio buffer to OpenAI
    - `vocis.transcribe.wait_final` — wait for OpenAI to return the trailing transcript
  - `vocis.inject` — text insertion into the target window
    - `vocis.inject.focus` — window activate and modifier key release
    - `vocis.inject.paste` or `vocis.inject.type` — clipboard paste or xdotool type

`vocis.inject.capture_target` runs before the dictation span to identify the active window.

## Short Recordings

Very short recordings are treated as a silent cancel:

- [`internal/recorder/recorder.go`](/home/fred/git/vtt/internal/recorder/recorder.go) returns `ErrRecordingTooShort`
- [`internal/app/app.go`](/home/fred/git/vtt/internal/app/app.go) catches that and hides the overlay
- no user-facing error is shown for that case

## Logging

Each `serve` session writes a log file under:

- `~/.local/state/vocis/sessions/`

The log is the best place to look for:

- realtime session setup failures
- streaming errors
- insertion failures
- hotkey fallback decisions

## Verification Standard

This repo intentionally uses a high bar before calling work done:

- unit tests where they make sense
- successful build
- local runtime verification for behavior changes whenever feasible

That rule is summarized in [`AGENTS.md`](/home/fred/git/vtt/AGENTS.md).
