# Sovereign AI

Sovereign AI is a local Windows companion for OpenClaw. It collects a short user and assistant profile, writes the profile into the OpenClaw workspace, installs OpenClaw on first setup, configures the Agents-A1 4B Q4_K_M model and local embeddings, and provides start/stop controls for the Gateway.

The linked Agents-A1 base repository contains the 35B model; Sovereign AI uses its separate 4B Q4_K_M quantization at [InternScience/Agents-A1-4B-Q4_K_M-GGUF](https://huggingface.co/InternScience/Agents-A1-4B-Q4_K_M-GGUF). OpenClaw and its managed llama.cpp provider own inference and memory indexing. The default memory backend is OpenClaw `memory-core` with local embeddings; profile and memory files stay in `%LOCALAPPDATA%\SovereignAI\workspace`, under OpenClaw's separate `a1` profile.

The dashboard can also install Honcho, LanceDB, or Mem0 integrations. Honcho supplements core memory and may use a hosted or self-hosted service. LanceDB replaces the active memory slot and uses local embeddings. Mem0's open-source wizard offers local and hosted components; some choices require separate Ollama, Qdrant, database, or API services. Review those choices if all data must stay on this device.

## Run locally

```powershell
go run -buildvcs=false .
```

The app opens a loopback-only dashboard in your browser. Submit the first-run profile once; Sovereign AI then installs and configures OpenClaw automatically in the background. Setup stages and available download progress appear in the dashboard, with a retry action if a stage fails. OpenClaw runs as a hidden child process; Sovereign AI captures its output and handles setup confirmations without opening a terminal. The initial install needs an internet connection and several gigabytes of free disk space.

## Build

```powershell
go build -buildvcs=false -o dist\SovereignAI.exe .
```

The Go app uses only the standard library and embeds its web UI. It is self-contained: nothing is installed system-wide, and no system `PATH`, PowerShell profile, or remote installer script is used. On first setup the app resolves its dependency chain at run time into `%LOCALAPPDATA%\SovereignAI\runtime`:

- `node` - a portable Node.js LTS (22+) downloaded from nodejs.org and verified against its published SHA-256.
- `openclaw` - OpenClaw installed with the private npm into its own prefix, and launched as `node.exe <entry>`.
- `npm-cache` - the private npm cache.
- `home` - `OPENCLAW_HOME`, holding OpenClaw state, the llama.cpp runtime, model weights, and the embedding model.

Deleting `%LOCALAPPDATA%\SovereignAI` removes everything. Child process output is captured by the app and shown in the dashboard. Existing installs that used the old system-wide OpenClaw will run setup again into the private runtime.
