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

## Run

```powershell
go run -buildvcs=false .
```

or build once and double-click:

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
