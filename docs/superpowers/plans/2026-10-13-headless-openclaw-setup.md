# Headless OpenClaw Setup and Progress Plan

**Goal:** Make Sovereign AI configure OpenClaw in the background and report setup progress in its own dashboard.

**Architecture:** Sovereign AI owns the first-run flow and drives OpenClaw with noninteractive child processes. The backend persists named setup stages and streams safe child output into state; the web UI renders stage progress, current activity, and retry actions.

**Files:** `main.go` for setup lifecycle and child-process handling; `web/index.html`, `web/app.js`, and `web/style.css` for the one-submit setup and progress bar; `README.md` for the supported headless workflow. Leave the existing `AGENTS.md` untouched.

**Constraints:** Keep OpenClaw out of the user's terminal. Preserve the required profile fields. Show truthful stage progress and any available download percentage. Use only the Go standard library. Do not add or run tests unless requested; build and inspect local HTTP responses.

## Tasks

1. Make profile submission start setup automatically; retain the existing setup endpoint for an explicit retry after failure.
2. Add persisted setup stage counters and capture child output without inheriting the user's terminal or accepting input from the user. Make installer and OpenClaw commands run unattended.
3. Render an accessible progress bar and current operation in onboarding. Remove copy that directs users to a terminal, and update the run instructions.
4. Build the executable and inspect the dashboard, setup state, and static assets over loopback.
