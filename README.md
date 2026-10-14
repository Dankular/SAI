# Sovereign AI

A private, local AI assistant for Windows. It runs on your own computer: a local language model, a
voice you can talk to, and the [OpenClaw](https://openclaw.ai) agent platform for tools, skills and
memory. Nothing you say leaves the machine unless you choose an outside service.

You set it up once from a dashboard. After that you can type to it, or talk to it hands-free and
interrupt it mid-sentence.

## What it is

| Piece | What it does | Where it runs |
|---|---|---|
| **OpenClaw** | The agent: tools, skills, sessions, memory search | Private Node.js (installed for you) |
| **Agents-A1 4B** | The language model (InternScience, Apache-2.0, Qwen3.5-based) | `llama-server` on your GPU |
| **EmbeddingGemma-2** | Memory search embeddings | `llama-server` (second instance) |
| **OmniVoice** | The assistant's voice, cloned from a reference recording | `audiocpp_server` on your GPU |
| **Smart Turn v3.2** | Decides whether a pause means you have finished speaking | Same `audiocpp_server` |
| **TEN VAD** | Hears when you start and stop speaking | In the browser (WebAssembly) |
| **Whistle** | Turns your speech into text (7 languages) | In the browser (WebAssembly) |

The Go program (standard library only) owns the lifecycle: it downloads everything, starts and stops
the servers, and serves the dashboard on a random loopback port.

## Requirements

- Windows 10/11, x64
- An internet connection and **about 7 GB** of free disk space for the first setup
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
4. Wait for the stages to finish (about 7 GB of downloads; 10-30 minutes depending on your connection).
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
Stages 1-7 are one-time installs and downloads (skipped on retry once they succeed); 8-11 start things
and run every time.

1. Private Node.js and OpenClaw
2. Workspace (and turning off OpenClaw's unused Ollama provider)
3. OpenClaw's llama.cpp provider plugin
4. llama.cpp runtime for your GPU
5. Agents-A1 4B (2.6 GB)
6. Embedding model (0.3 GB)
7. Voice and hearing: speech runtime, OmniVoice (1.4 GB), Smart Turn, Whistle (18 MB)
8. Start the model servers and connect them to OpenClaw
9. Index memory
10. Write the persona files
11. Start the OpenClaw gateway

To re-run from a stage: `POST /api/install?from=N`.

## Using it

- **Wake up / Put to sleep** starts and stops the OpenClaw gateway. It greets you when it wakes and says
  goodnight when it sleeps. Waking takes about a minute.
- **Type** in the box, or press **Mic** for hands-free: just talk. A pause is checked with Smart Turn, so
  a thinking pause is not mistaken for the end of your turn. Talk over the assistant to interrupt it.
  Headphones are recommended, because speakers can echo into the microphone.
- **Speak replies** turns the voice on and off. With it on, replies are kept short and conversational.
- The sphere shows what the assistant is doing: idle, active, listening, working, speaking. It moves to
  the assistant's own voice, never to your microphone.

## Where things live

```
%LOCALAPPDATA%\SovereignAI\
  state.json            setup progress and settings
  gateway.token         secret for the local OpenClaw endpoint (never sent to the browser)
  workspace\            the assistant's persona and memory files
  voices\               reference voice (default.wav + default.txt)
  runtime\
    node\               private Node.js
    openclaw\           OpenClaw (npm prefix)
    home\               OpenClaw's own state (OPENCLAW_HOME)
    llama\<backend>\    llama.cpp for your GPU
    models\             chat and embedding models, and Whistle
    audio\<backend>\    speech runtime;  audio\models\ OmniVoice and Smart Turn
  *.log                 gateway, model server and speech server logs
```

Delete the folder to uninstall completely.

## Ports (loopback only)

| Port | Service |
|---|---|
| random | dashboard (printed at start, also in `server.url`) |
| 28615 | OpenClaw gateway |
| 19432 / 19433 | chat / embedding model servers |
| 19434 | speech server |

## Checking an install

`GET /api/diagnose` on the dashboard address returns a pass/fail list of everything a finished install
should have (runtimes, models, voice, persona files, OpenClaw settings, running servers). `GET /api/state`
shows live status, including which GPU backends were chosen.

## Troubleshooting

- **Waking is slow.** Windows Defender scanning Node's thousands of files is the usual cause. Excluding
  the data folder helps a lot (administrator PowerShell, undo with `Remove-MpPreference`):
  `Add-MpPreference -ExclusionPath "$env:LOCALAPPDATA\SovereignAI"`
- **Force a GPU backend.** Set `SOVEREIGNAI_LLAMA_BACKEND` (`cuda`, `sycl`, `vulkan`, `cpu`) and/or
  `SOVEREIGNAI_AUDIO_BACKEND` (`cuda13.3`, `cuda12.4`, `vulkan`, `cpu`) before starting.
- **No voice you like.** Put your own `default.wav` (5-15 s, clean speech) and `default.txt` (its exact
  words) in `%LOCALAPPDATA%\SovereignAI\voices\`. See `assets/voices/README.md`.
- **Microphone blocked.** Allow it in the browser's address bar, then press Mic again.

## Licences worth knowing

OmniVoice's weights are **CC-BY-NC 4.0: personal, non-commercial use only.** Agents-A1, Whistle, Smart
Turn, llama.cpp, audio.cpp and onnxruntime are permissively licensed. TEN VAD is Apache-2.0 with additional
conditions (`web/vendor/tenvad/LICENSE`). Vendored web libraries and their licences are in `web/vendor/`.

## Develop

```powershell
go vet ./...
go test ./...
go build -buildvcs=false -o dist\SovereignAI.exe .
```

`AGENTS.md` describes the code layout and conventions.
