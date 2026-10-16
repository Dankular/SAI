# Repository Guidelines

## Project Structure & Module Organization

A Go program that provisions and supervises a local assistant. It uses chromem-go as its embedded,
persistent vector store. Source files are in the repository root, one concern per file:

- `main.go` — HTTP server, state, API handlers
- `supervisor.go` — the process manager: starts, health-checks, restarts and orders every service; its `Agent()` is the only source of truth for the assistant's state
- `setup.go` — the first-run stage table (`setupStages`); the single source of truth for installer steps
- `runtime.go` — data directory layout, child-process environment, HTTP and zip helpers
- `llama.go` — GPU backend choice, llama.cpp download, chat/embedding model servers (with the DSpark draft)
- `audio.go` — speech runtime (OmniVoice voice, Smart Turn), TTS and turn-detection endpoints
- `whistle.go` — serves the speech-recognition model files to the page
- `voice.go` — the dashboard's chat endpoint (streams an agent turn), agent warm-up
- `agent.go` — the agent loop: prompt assembly, history fitting, streaming model calls, tool execution, approvals, saved sessions
- `ai_gateway.go` — local model-call hooks, streamed generation events, and local marketplace intent routing
- `agent_tools.go` — the tools the model can call; each has a `Guard` that decides when the user must approve
- `harness.go` — what the program settles before the model sees a message: exact answers, memory writes (save, update, forget), facts the user volunteers, tool selection, compound requests, web prefetch; `harness_reply.go` tidies the streamed reply; `harness_recall.go` answers questions from the notes and maps mangled paths into the workspace; `harness_web.go` is web search (several sources) and weather. Prefer adding logic here over adding prose to the prompt
- `skills.go` — `SKILL.md` discovery and loading
- `memory.go` — persistent semantic search over workspace text, saved conversations and memory events, with a word-match fallback
- `memory_events.go` — evidence-checked event extraction and the append-only memory event history
- `state_events.go` — Go-owned work state and the server-sent event stream shared by dashboard tabs
- `shell_windows.go`, `shell_other.go` — how the `shell` tool starts a command
- `persona.go` — the workspace files that define the assistant's identity (see below)
- `diagnose.go` — `/api/diagnose` install health report
- `web/` — the embedded dashboard (HTML, CSS, ES modules); `web/vendor/` holds third-party code with licences
- `assets/voices/` — optional reference voice, embedded at build time
- `docs/` — plans and notes

Keep model and runtime integration boundaries explicit: each external engine (llama.cpp, audio.cpp)
is started, health-checked and stopped from one file.

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

The agent loop injects the workspace files into every turn (`systemPrompt` in `agent.go`), so identity
lives there. `persona.go` regenerates `IDENTITY.md`, `SOUL.md` and a marked block at the top of
`AGENTS.md` from the profile on every start; `USER.md` and `MEMORY.md` are created once and never
overwritten (the agent maintains them through the `remember` tool). Small models need explicit point of
view, worked examples and short rules; change the text, then run `go test ./...` (the tests check names,
placeholders and that user-owned files survive).

The chat model is MiniCPM-V 4.7 1B, a very small model: keep tool descriptions short and the tool count low,
and put anything the model must not get wrong in a tool `Guard` or in code rather than in prose. Agent tests
use `useFakeModel` (see `agent_test.go`) to script model replies, so they need no GPU.

## Testing Guidelines

Add tests with new logic, in `*_test.go` beside the code, using `newTestApp(t)` so nothing touches a real
install. Cover installer stage ordering, persona generation, backend selection, config parsing and zip
safety (see `app_test.go`). Anything that needs a GPU, the network or a browser is verified by running
the app and reading `/api/diagnose`.

## Commit & Pull Request Guidelines

There is no Git history from which to infer commit conventions. Use short, imperative commit subjects, such as `Add agent lifecycle handler`. Pull requests should explain the change and its motivation, list validation commands and results, link related issues, and include screenshots for user-visible interface changes.

## Security & Configuration

Do not commit credentials, private keys, or personal data. Keep local secrets in ignored environment files and provide sanitized examples for required configuration. Treat local model files and runtime endpoints as configurable environment-specific inputs.
