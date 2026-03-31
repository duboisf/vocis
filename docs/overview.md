# Overview

`vocis` is a Linux X11 voice-to-text helper written in Go.

At a high level:

- a global hotkey starts dictation
- the overlay appears immediately
- local microphone capture starts first
- target-window capture happens after local recording has already started
- early audio is buffered while the OpenAI realtime transcription session connects
- buffered audio is flushed into the realtime session as soon as it is ready
- audio is streamed to OpenAI after that
- in segmented mode, completed phrases accumulate in the overlay as you speak (one line per segment)
- on release or stop, the dictation session decides whether there is trailing audio left to commit
- the accumulated text plus any trailing transcript is inserted back into the previously focused app as a single paste

Important constraints:

- Linux X11 only for now
- PulseAudio / PipeWire input capture
- OpenAI API key stored in the system keyring or provided by `OPENAI_API_KEY`
- overlay is intentionally lightweight and non-interactive

Core product choices:

- `hold` mode is the default hotkey behavior
- `toggle` mode is also supported by config
- very short recordings are silently discarded
- terminal windows use a terminal-safe paste shortcut
- transcription is realtime-streamed, not uploaded from a WAV file
- turn assembly and trailing-flush decisions live in the OpenAI dictation session, not in the app layer

If you only need the "what is this thing" version of the repo, stop here.
