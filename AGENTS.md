# Repository Guidelines

## Project Structure & Module Organization

A Go program (standard library only) that provisions and supervises a local assistant. Source files are in
the repository root, one concern per file:

- `main.go` — HTTP server, state, API handlers, gateway lifecycle
- `supervisor.go` — the process manager: starts, health-checks, restarts and orders every service; its `Agent()` is the only source of truth for the assistant's state
- `setup.go` — the first-run stage table (`setupStages`); the single source of truth for installer steps
- `runtime.go` — private Node.js and OpenClaw installation
- `llama.go` — GPU backend choice, llama.cpp download, chat/embedding model servers, OpenClaw registration
- `audio.go` — speech runtime (OmniVoice voice, Smart Turn), TTS and turn-detection endpoints
- `whistle.go` — serves the speech-recognition model files to the page
- `voice.go` — bridge from the dashboard to the OpenClaw gateway, gateway credentials, warm-up
- `persona.go` — the workspace files that define the assistant's identity (see below)
- `diagnose.go` — `/api/diagnose` install health report
- `web/` — the embedded dashboard (HTML, CSS, ES modules); `web/vendor/` holds third-party code with licences
- `assets/voices/` — optional reference voice, embedded at build time
- `docs/` — plans and notes

Keep model and runtime integration boundaries explicit: each external engine (llama.cpp, audio.cpp,
OpenClaw) is started, health-checked and stopped from one file.

## Build, Test, and Development Commands

```powershell
go vet ./...                                  # also checks Sprintf argument counts; run before committing
go test ./...                                 # unit tests (no network, no GPU)
go build -buildvcs=false -o dist\SovereignAI.exe .
go run -buildvcs=false .                      # starts the app and opens the dashboard
```

A running app serves `GET /api/state` (live status) and `GET /api/diagnose` (install checks) on its
loopback address, written to `%LOCALAPPDATA%\SovereignAI\server.url`. A full first-run setup downloads
about 6 GB; test it against a throwaway data directory by setting `LOCALAPPDATA` for that process.

## Coding Style & Naming Conventions

Run `gofmt`. Use descriptive names, keep files focused, and make state transitions and lifecycle
ownership clear. Child processes are started with `runtimeEnv()`: it deliberately replaces `PATH` (npm
hands lifecycle scripts an empty `PATH` on machines with a long one) and drops `PSModulePath`. Do not
add `powershell.exe` calls or system-wide installs; the app must stay self-contained under its data
directory.

## Assistant identity and prompts

OpenClaw documents no global system-prompt override, so identity lives in the workspace files it injects
every turn. `persona.go` regenerates `IDENTITY.md`, `SOUL.md` and a marked block at the top of
`AGENTS.md` from the profile on every start; `USER.md` and `MEMORY.md` are created once and never
overwritten (the agent maintains them). Small models need explicit point of view, worked examples and
short rules; change the text, then run `go test ./...` (the tests check names, placeholders and that
user-owned files survive).

## Testing Guidelines

Add tests with new logic, in `*_test.go` beside the code, using `newTestApp(t)` so nothing touches a real
install. Cover installer stage ordering, persona generation, backend selection, config parsing and zip
safety (see `app_test.go`). Anything that needs a GPU, the network or a browser is verified by running
the app and reading `/api/diagnose`.

## Commit & Pull Request Guidelines

There is no Git history from which to infer commit conventions. Use short, imperative commit subjects, such as `Add agent lifecycle handler`. Pull requests should explain the change and its motivation, list validation commands and results, link related issues, and include screenshots for user-visible interface changes.

## Security & Configuration

Do not commit credentials, private keys, or personal data. Keep local secrets in ignored environment files and provide sanitized examples for required configuration. Treat local model files and runtime endpoints as configurable environment-specific inputs.
