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
- `memory_meta.go` — what the assistant used and what the user pinned (`brain/salience.json`); feeds ranking, fades with a 90-day half-life
- `memory_tools.go` — the model's memory tools: `memory_search` (kind/days filters), `memory_get`, `memory_edit` (forget, update, pin)
- `consolidate.go` — deterministic daily tidy-up: merges duplicate/outdated notes and repeated events, backs up first to `brain/backups`
- `profile.go` — the evidence-backed "what you have learned about the user" block injected into the system prompt
- `cron.go` — reminders and unattended scheduled tasks (`schedule` tool, scheduler, notices); `harness.go` handles "remind me …" without the model
- `memory_api.go` — `/api/memory`, the dashboard's memory drawer (`web/memory.js`): inspect, pin, forget, tidy up
- `schemas.go` — JSON schemas for the harness's own model calls (event extraction, dreaming, skill drafts); never combined with tools
- `fact_history.go` — `fact_changed` events keep the old value when a note is corrected, so "what was it before?" has an answer
- `archive.go` — notes over their prompt budget move to `memory/archive.md` (still searchable) instead of being cut off
- `heartbeat.go` — every 30 minutes, if `HEARTBEAT.md` has bullets and the assistant is idle, run them; quiet unless something needs attention
- `dream.go` — once a day while idle, the model proposes note merges/additions; Go applies only those it can ground in events, and logs to `memory/dreams.md`
- `skill_proposals.go` — drafts a skill after a hard task into `<data>/skill-proposals`; only usable once the user approves it in the Memory drawer
- `harness_correct.go` — corrections, "what was it before?", and scheduling from any sentence with a repeat phrase
- `toolchain.go` — bundled portable Node.js (npx) and uv (uvx), pinned by version and SHA-256 under `runtime/tools`; MCP servers and the shell tool get them on PATH, with private caches. Installed by a setup stage, an `installPieces` entry for existing installs, and lazily on first use
- `mcp_find.go` — `extension action=find`: restates the job (schema-constrained, rejected if off-topic), checks installed tools first, then merges marketplace searches and re-ranks by embedding similarity
- `mcp_install.go` — `action=install`: reads the README, extracts launch commands in Go (never model-written), refuses installer lines and non-npx/uvx launchers, asks the user for placeholders and credentials, tries each option and rolls back one that does not start
- `mcp_use.go` — `action=use`: tool arguments decoded under the tool's own input schema, filler removed, missing required values asked for; successes are remembered (`mcp-usage.json`) and draft a skill for approval
- `builtin_skills.go` — the shipped `find-mcp-tool` skill, written once to the shared skills folder
- `legacy.go` — cleanup of earlier releases. Inside the app folder (OpenClaw runtime, private Node.js, the Agents-A1 model in any quantisation) things are deleted; outside it (`~\.clawdbot`, `%LOCALAPPDATA%\openclaw`, the global npm package and shims) each item is verified, then *retired* to `<data>etired` and deleted after 14 days, never while an OpenClaw gateway answers, never with `SOVEREIGNAI_KEEP_LEGACY` set
- `orb_mesh.go` — the desktop orb's sphere, ported from `web/orb.js` so the two look alike: a geodesic icosphere (three's IcosahedronGeometry(2, 9)) displaced by the same simplex noise, shaded with the web shaders' own formulas, every triangle edge drawn, an analytic halo. `orb_render.go` draws the layers in the web scene's order (surface, neuron, mesh, halo, then the galaxy additively on top) with the web camera (46 degrees, 15 units back), so the sphere is a fixed fraction of the frame HEIGHT. Compare the two with a PNG export of a native frame and a headless-Edge screenshot of the web page; do not trust a flat side-by-side look
- `widget_settings.go` — what the Settings page controls for the desktop widget (width, height, spiral on/off and size, a colour per state), validated and saved in `state.json`; `GET /api/widget` reads them, `POST` changes them, `GET /api/widget/preview` draws one frame with the renderer. A new size restarts the widget (`supervisor.Restart`), because its drawing surface is made at one size. `web/settings.js` is the page
- `orb_draw.go` — how the orb is drawn fast: row bands spread over a few threads (a band never writes outside its rows, so one thread and many give the identical picture; tested), lookup tables instead of per-pixel `pow`/`exp`, and per-frame worker counts (`orbWorkersFor`). The widget draws calm at 12-20 fps and busy (voice, a pulse, thinking, a state change) at 60 (`orbWantsFullRate`), presents at 30/60 Hz whatever the monitor does, and reuses its frame buffers. Measure CPU with the process cycle counter (`QueryProcessCycleTime`), not `Get-Process` CPU time, which is sampled at ~15 ms ticks and badly undercounts a loop like this
- `profile_debug.go` — set `SOVEREIGNAI_PROFILE_DIR` and the app writes a 20 s CPU profile of itself (from 25 s after launch) to `cpu.pprof`; read it with `go tool pprof SovereignAI.exe cpu.pprof`
- The orb's simplex noise (`orbSimplex`) must do its hash arithmetic in float32 like the GPU: in float64, exact multiples of 7 and 49 round the other way and give noise of +-4 instead of +-1, which made the sphere explode at high voice levels. It is checked against values read back from the real shader (`orb_fidelity_test.go`). To check a port against the GPU, render the shader in headless Edge with its own `--user-data-dir`, a `file:///C:/...` page (not an MSYS `/c/...` path) and `--timeout`, and read pixels back
- Every script the dashboard loads must be on the `publicPath` allowlist in `main.go`; `TestEveryAssetTheDashboardNamesIsServed` checks it. A file that is embedded but not listed is a silent 404 and its feature never starts
- `processimage_*.go` — a process's program path; `evictOrphans` (in `supervisor.go`) only stops a leftover server whose program lives inside this install's `runtime` folder, so a second copy of the app can never kill the first one's servers

The system prompt is deliberately stable (no clock, no retrieved memory) so llama-server can reuse its prompt cache; per-turn context goes through `volatileContext`. Unattended work (scheduled tasks, heartbeat) gets `unattendedTools` and a step cap via `turnOptions.AllowTools/MaxSteps`.
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

The default chat model is MiniCPM5 2B (with its DSpark draft, `chatModels[].Draft`). Settings can also select MiniCPM5 1B, Nanbeige4.1 3B, Qwen3.5-4B, Ministral 3 3B, Phi-4-mini,
or LFM2.5-2.6B. Keep tool descriptions short and put anything a local model must not get wrong in a tool
`Guard` or in code rather than in prose. Agent tests use `useFakeModel` (see `agent_test.go`) to script
model replies, so they need no GPU.

## Testing Guidelines

Add tests with new logic, in `*_test.go` beside the code, using `newTestApp(t)` so nothing touches a real
install. Cover installer stage ordering, persona generation, backend selection, config parsing and zip
safety (see `app_test.go`). Anything that needs a GPU, the network or a browser is verified by running
the app and reading `/api/diagnose`.

## Commit & Pull Request Guidelines

There is no Git history from which to infer commit conventions. Use short, imperative commit subjects, such as `Add agent lifecycle handler`. Pull requests should explain the change and its motivation, list validation commands and results, link related issues, and include screenshots for user-visible interface changes.

## Security & Configuration

Do not commit credentials, private keys, or personal data. Keep local secrets in ignored environment files and provide sanitized examples for required configuration. Treat local model files and runtime endpoints as configurable environment-specific inputs.
