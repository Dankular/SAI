# Sovereign AI

A private, local AI assistant for Windows. It runs on your own computer: a local language model, a
voice you can talk to, and a built-in agent that can use tools, load skills and remember what matters. Nothing you say leaves the machine unless you choose an outside service.

You set it up once from a dashboard. After that you can type to it, or talk to it hands-free and
interrupt it mid-sentence.

## What it is

| Piece | What it does | Where it runs |
|---|---|---|
| **Agent loop** | Tool calling, skills, sessions, memory, approvals (`agent*.go`, `skills.go`, `memory.go`) | Inside the app itself; no extra process |
| **Local chat model** | MiniCPM5 2B by default; Settings can also select MiniCPM5 1B, Nanbeige4.1 3B, Qwen3.5-4B, Ministral 3 3B, Phi-4-mini, or LFM2.5-2.6B | `llama-server` on your GPU |
| **DSpark draft** | Predicts several words ahead for faster replies when MiniCPM5 2B is selected | Same `llama-server` (speculative decoding) |
| **EmbeddingGemma-2** | Memory search embeddings | `llama-server` (second instance) |
| **OmniVoice** | The assistant's voice, cloned from a reference recording | `audiocpp_server` on your GPU |
| **Smart Turn v3.2** | Decides whether a pause means you have finished speaking | Same `audiocpp_server` |
| **TEN VAD** | Hears when you start and stop speaking | In the browser (WebAssembly) |
| **Whistle** | Turns your speech into text (7 languages) | In the browser (WebAssembly) |

The Go program owns the lifecycle: it downloads everything, starts and stops the servers, manages
shared assistant state, and serves the dashboard on a random loopback port. An embedded chromem-go
database keeps a persistent semantic index of workspace text, saved conversations, and memory events.

## Requirements

- Windows 10/11, x64
- An internet connection and **about 5 GB** of free disk space for the first setup
- A GPU is strongly recommended. **NVIDIA (CUDA) is the tested path.** Intel (SYCL for the chat model,
  Vulkan for speech) and AMD (Vulkan) are implemented but **not yet verified on real hardware**, and
  everything falls back to the CPU if a GPU backend fails to start
- An 8 GB GPU runs the whole stack. Less VRAM works but leaves little headroom
- A modern Chromium-based browser (Edge or Chrome) for microphone support

Nothing is installed system-wide. Everything lives in `%LOCALAPPDATA%\SovereignAI`.

## Install

1. Download `SovereignAI.exe` from the [latest release](https://github.com/Dankular/SAI/releases/latest).
   It is a single file with the dashboard and reference voice built in. There is no installer.
2. Put it anywhere (for example `C:\Tools\SovereignAI\`) and double-click it. Windows SmartScreen may
   warn about an unsigned app: choose **More info**, then **Run anyway**.
3. Your browser opens the dashboard. Enter your name, a name for the assistant, what you want to use it
   for, and a reply style, then press **Begin setup**.
4. Wait for the stages to finish (about 5 GB of downloads; 10-30 minutes depending on your connection).
   If one fails, press retry. It resumes from the first unfinished stage.
5. When setup completes the assistant wakes up and greets you. Next time, run the exe again and press
   **Wake up**.

To uninstall, close the app and delete `%LOCALAPPDATA%\SovereignAI` and the exe.

### Installing from a script or an agent

The same steps work without a browser. The app is driven by a local HTTP API:

```powershell
$env:SOVEREIGNAI_NO_BROWSER = "1"
Start-Process .\SovereignAI.exe
Start-Sleep 5
$base = (Get-Content "$env:LOCALAPPDATA\SovereignAI\server.url").Trim()   # e.g. http://127.0.0.1:61159

# 1. start setup (replyStyle: concise | balanced | detailed)
$profile = @{ userName = "Alex"; agentName = "Sovereign"; purpose = "Daily planning and research"; replyStyle = "balanced" } | ConvertTo-Json
Invoke-RestMethod "$base/api/setup" -Method Post -Body $profile -ContentType "application/json" -Headers @{ Origin = $base }

# 2. poll until setupComplete is true (setupRunning false and a non-empty error means it failed)
do { Start-Sleep 15; $s = Invoke-RestMethod "$base/api/state" } until ($s.setupComplete -or (-not $s.setupRunning -and $s.error))

# 3. verify: every check should pass
(Invoke-RestMethod "$base/api/diagnose") | ConvertTo-Json -Depth 5
```

To retry after a failure, `POST $base/api/install` (add `?from=N` to restart from stage N). Requests must
come from loopback; POSTs need an `Origin` header matching the server address, as above. The app must stay
running for the assistant to stay awake. The exe can be launched at logon with a Startup-folder shortcut.

### Intel GPUs (Arc and integrated)

Setup is identical: download, run, fill in the profile. The app detects Intel graphics from the driver
files in `System32` (`ze_intel_gpu64.dll`, `igc64.dll`) and picks the backends itself:

| Part | Backend used | Download |
|---|---|---|
| Chat and memory models | llama.cpp **SYCL** (Intel XPU, via Level Zero) | about 140 MB; bundles the oneAPI runtime, so no oneAPI install is needed |
| Voice and Smart Turn | audio.cpp **Vulkan** (audio.cpp has no SYCL build) | about 60 MB |

Before you start: install the current Intel graphics driver, since SYCL needs its Level Zero component. If
the machine also has an NVIDIA GPU, NVIDIA (CUDA) is chosen instead. If a backend fails to start, the app
falls back to the CPU automatically and says so in the activity log (**View terminal**). To force a choice:
`SOVEREIGNAI_LLAMA_BACKEND=sycl` (or `vulkan`, `cpu`) and `SOVEREIGNAI_AUDIO_BACKEND=vulkan`.

After setup, `GET /api/state` shows which backends were chosen, and `GET /api/diagnose` confirms the servers
are running. **Status: the download and backend selection are checked against the published llama.cpp and
audio.cpp releases, but the Intel path has not been run on real Intel hardware.** Reports are welcome.

## Run from source

```powershell
go run -buildvcs=false .
```

or build once and double-click (Go 1.25+ required):

```powershell
go build -buildvcs=false -o dist\SovereignAI.exe .
```

The dashboard opens in your browser. Fill in the first-run profile once; setup then runs on its own and
shows each stage. If something fails, the retry button resumes from the first unfinished stage.

### Setup stages

The list lives in `setup.go` (`setupStages`) and the dashboard renders whatever the server sends.
The first stages are one-time installs and downloads (skipped on retry once they succeed); the rest start
things and run every time. See `setup.go` for the current order. In short: workspace folders and the private
POSIX shell, **portable Node.js and uv (so extensions can run with `npx` and `uvx`, about 50 MB, nothing
installed system-wide)**, the llama.cpp runtime for your GPU, the default MiniCPM5 2B (2.7 GB) with its DSpark
draft (0.2 GB), the embedding model (0.3 GB), voice and hearing (OmniVoice 1.4 GB, Smart Turn, Whistle), then
starting the model servers, writing the persona files, indexing memory and waking the assistant.

Other chat models can be selected in Settings; each is downloaded on demand and kept in your local models folder.

Installs made by an earlier release fetch only what is new (the dashboard shows a small "Updating" strip).

To re-run from a stage: `POST /api/install?from=N`.

## Using it

- **Wake up / Put to sleep** starts and stops the assistant. It greets you when it wakes and says
  goodnight when it sleeps. Waking takes a few seconds once the model has loaded.
- **Type** in the box, or press **Mic** for hands-free: just talk. A pause is checked with Smart Turn, so
  a thinking pause is not mistaken for the end of your turn. Talk over the assistant to interrupt it.
  Headphones are recommended, because speakers can echo into the microphone.
- **Speak replies** turns the voice on and off. With it on, replies are kept short and conversational.
- The sphere shows the assistant's shared server state in every dashboard tab and on the desktop widget.
  The native widget follows the display's reported refresh rate. Voice activity animates the orb but
  does not change the shared agent state.

## The assistant's mind

The agent loop (`agent.go`) sends each message to the local model together with a prompt built from the
files in your workspace, every turn, so edits take effect immediately:

| File | Purpose |
|---|---|
| `IDENTITY.md`, `SOUL.md` | Who it is, its character and how it speaks (regenerated from your profile; delete the first line to take over editing) |
| `AGENTS.md` | Standing rules. Our block sits at the top; add your own below it |
| `USER.md` | What it knows about you (preferences, family, habits). It adds to this itself |
| `MEMORY.md`, `memory\*.md` | Things it was asked to remember, searchable by meaning |
| `memory\events.jsonl` | Evidence-linked conversation outcomes and decisions, extracted automatically after a turn |
| `skills\<name>\SKILL.md` | Skills: written guides for particular jobs |

The persistent vector index also searches text files throughout the workspace and every saved conversation.
Relevant passages are ranked and brought into later prompts, so older context remains searchable after it
falls out of the model's active context. `GET /api/events` streams shared state changes to open tabs.

Every model request also passes through an in-process Go gateway (`ai_gateway.go`). Its hooks can adjust a
request before inference, inspect streamed text and partial tool calls, then inspect the final result or error.
The pre-call route check uses the local Hugging Face EmbeddingGemma model with its classification task prefix
and a compact sample of recent user context. Only a confident missing-capability route triggers a read-only
marketplace search; installing or configuring a result still follows the normal approval rules.

**Tools** the model can call: `shell`, `process`, `read_file`, `write_file`, `edit_file`, `apply_patch`,
`list_dir`, `calculator`, `web_search`, `web_fetch`, `memory_search`, `memory_get`, `memory_edit`, `remember`,
`schedule`, `read_skill` and `extension`. The harness offers only the few that fit each message.

**Approvals.** Running a command, and reading or writing outside the workspace folder, always pauses and asks
you to **Allow** or **Don't allow** in the conversation. Web tools can never reach this computer or your local
network. `web_search` sends only the search words to DuckDuckGo.

**Skills.** A skill is a folder with a `SKILL.md`: a short front-matter block, then instructions.

```
---
name: weekly-plan
description: Plan my week from my calendar and open tasks
---
# Weekly plan
1. Ask which days are free ...
```

Put it in `workspace\skills\weekly-plan\` (or the shared `skills\` folder). The model sees only each skill's
one-line description and loads the full text with `read_skill` when it matches the request. The assistant can
also write skills for itself with `write_file`. Skills in the OpenClaw/Claude `SKILL.md` format work as they are.

**Memory.** Durable conversation events are extracted after completed turns and kept with their evidence in
`memory/events.jsonl`. `remember` remains available for explicit saves. `memory_search` ranks workspace files,
all saved conversations, and memory events with the local embedding model, and falls back to matching words if
the embedding server is not running.

**The harness: what the program does so the model doesn't have to.** The model is only about a billion
parameters, so the app does the exact and mechanical jobs itself, in plain code (`harness*.go`):

- **Exact answers without the model:** the time and date, arithmetic ("what's 18% of 250, then 12 times 12"),
  and the current weather for a named place are answered directly and instantly.
- **Memory by code:** "remember that my car service is on 12 December", "actually it's the 14th" (it replaces the old
  note), "forget about the car service" (removes the note *and* the earlier chat about it), and facts you simply
  mention ("I'm allergic to shellfish, and my dog is called Biscuit") are saved, updated and removed by the program.
  Questions the notes can answer ("what is my dog called?") are answered from the notes.
- **Right tools only:** each message gets only the tools that make sense for it, so the model rarely picks a wrong one.
- **Step by step:** "do this, then do that" runs as separate turns, one task at a time.
- **Web lookups done for it:** for "latest / current / news" questions the program runs the search and hands over
  the results. If the search fails the model is told so and says it could not check. Nothing secret-looking from your
  notes is ever put in a search.
- **A tidy answer:** narration ("I'll look that up"), repeats, markdown in spoken answers, and claims of having saved
  something that was not saved are removed as the reply streams.

**Speed.** Replies use speculative decoding (the DSpark draft) when the model server supports it; if it does
not, the app logs why and runs without it. `SOVEREIGNAI_NO_DRAFT=1` turns it off.

## Where things live

```
%LOCALAPPDATA%\SovereignAI\
  state.json            setup progress and settings
  workspace\            the assistant's files: IDENTITY, SOUL, USER, MEMORY, AGENTS (.md), memory\ notes, skills\
  skills\               skills shared by every workspace (optional)
  sessions\             saved conversations
  voices\               reference voice (default.wav + default.txt)
  runtime\
    llama\<backend>\    llama.cpp for your GPU
    models\             chat and embedding models, and Whistle
    audio\<backend>\    speech runtime;  audio\models\ OmniVoice and Smart Turn
  *.log                 model server and speech server logs
```

Delete the folder to uninstall completely.

## Ports (loopback only)

| Port | Service |
|---|---|
| random | dashboard (printed at start, also in `server.url`) |
| 19432 / 19433 | chat / embedding model servers |
| 19434 | speech server |

## Checking an install

`GET /api/diagnose` on the dashboard address returns a pass/fail list of everything a finished install
should have (runtimes, models, voice, persona files, skills, running servers). `GET /api/state`
shows live status, including which GPU backends were chosen.

## Troubleshooting

- **Force a GPU backend.** Set `SOVEREIGNAI_LLAMA_BACKEND` (`cuda`, `sycl`, `vulkan`, `cpu`) and/or
  `SOVEREIGNAI_AUDIO_BACKEND` (`cuda13.3`, `cuda12.4`, `vulkan`, `cpu`) before starting.
- **No voice you like.** Put your own `default.wav` (5-15 s, clean speech) and `default.txt` (its exact
  words) in `%LOCALAPPDATA%\SovereignAI\voices\`. See `assets/voices/README.md`.
- **Microphone blocked.** Allow it in the browser's address bar, then press Mic again.

## Licences worth knowing

OmniVoice's weights are **CC-BY-NC 4.0: personal, non-commercial use only.** MiniCPM5 and its DSpark draft, Whistle, Smart
Turn, llama.cpp, audio.cpp and onnxruntime are permissively licensed. TEN VAD is Apache-2.0 with additional
conditions (`web/vendor/tenvad/LICENSE`). Vendored web libraries and their licences are in `web/vendor/`.

## Develop

```powershell
go vet ./...
go test ./...
go build -buildvcs=false -o dist\SovereignAI.exe .
```

`AGENTS.md` describes the code layout and conventions.

## Adaptive memory

Everything below is plain files plus code you can read; nothing is sent anywhere.

- **Corrections keep history.** "Actually my gym day is Thursday" replaces the note and keeps the old value, so
  "what was my gym day before?" has an answer.
- **It learns what matters.** Memories it actually uses rank higher and fade over time; pinned ones stay.
  Notes over their prompt budget move to `memoryrchive.md` (still searchable) instead of being cut off.
- **It tidies itself.** A daily pass merges duplicate and outdated notes (backed up first to `brainackups`).
  Once a day while idle it also *reflects*: the model proposes tidy-ups and the program applies only those it can
  ground in recorded events, logging them in `memory\dreams.md`.
- **A profile it can show its work for.** What it has learned about you comes from events with the quote that
  produced them. The **Memory** drawer in the dashboard lets you read, pin, correct and forget any of it.
- **Reminders and routines.** "Remind me to call mum tomorrow at 9", "check the weather every morning at 7".
  Unattended tasks get lookup-and-read tools only and a step cap. `HEARTBEAT.md` holds an optional checklist it
  runs every 30 minutes while idle (`every: 15 minutes` changes that); it only speaks up when something matters.
- **Skills it drafts.** After a hard task it can write a skill from what worked. Drafts wait in the Memory
  drawer; nothing is usable until you approve it.

## Extending itself with MCP tools

When a job needs something it cannot do (a service, a database, a file format), the assistant uses
`extension`:

1. **find** restates the job, checks the tools already installed first, then searches the marketplaces
   (awesome-mcp-servers and mcpservers.org) and ranks candidates by meaning, not just keywords.
2. **install** reads the chosen project's README, takes the launch command it documents (never one the small
   model wrote), and only accepts servers that run on the bundled `npx` or `uvx`. It asks you for any credential
   or path it needs, starts the server to check it works, and removes it again if it does not. You approve it.
3. **use** fills in the tool's arguments under the tool's own input schema, asks you for anything missing instead
   of guessing, and calls it. The first success is remembered and drafts a skill for next time.

A shipped `find-mcp-tool` skill teaches the same steps. Marketplace listings and tool output are treated as
untrusted data.

## Settings

The **Settings** page (sidebar) controls the desktop widget: its width and height, whether the spiral shows and
how big it is, and one colour for each state (asleep, awake, listening, thinking, speaking). A preview drawn by
the same renderer the widget uses follows every change before it is saved; a new size restarts the widget, and
everything else changes at once. "Reset" puts the standard look back.

The desktop orb and the dashboard's sphere are drawn by two different programs (a CPU renderer in Go for the
transparent desktop window, WebGL for the page), so they are kept alike by porting the web scene to Go: the same
camera, noise, mesh, shading and layer order. They are close, not pixel-identical: colours in particular differ a
little.
