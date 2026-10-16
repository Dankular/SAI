# Agent harness reliability and capability design

## Purpose

Improve the Go harness around the local 1B chat model so it can resolve natural follow-ups, select capabilities with less prompt overhead, preserve useful context beyond the model window, and expose enough evidence to diagnose bad live behavior. Add deterministic regression coverage for complete conversation flows.

## Research basis

- OpenAI Agents SDK testing uses scripted model steps, records model calls, injects failures, and checks both unexpected calls and unconsumed steps.
- OpenHands and LangGraph separate working context from retained conversation history, trim or condense older messages, and load specialized instructions progressively.
- ReAct-style harnesses expose tool results as observations before choosing the next stateful action.
- Magg demonstrates dynamic MCP discovery and tool loading. SovereignAI keeps its Go-owned MCP registry and marketplace boundary.

## Current foundation

- The Go runtime owns the agent loop and local model servers.
- EmbeddingGemma-2 runs locally through llama.cpp; the gateway already uses vector centroids for marketplace routing.
- chromem-go indexes workspace memory, conversation history, and evidence-backed memory events.
- The model gateway exposes pre-call, streamed, and post-call hooks.
- The dashboard uses a persisted `dashboard` session and publishes app state to clients.
- The existing Wikipedia follow-up regression passes, but it seeds a separate session and does not replay the full dashboard exchange.

## Design

1. **Observable turn decisions.** Give every harness plan a short route label. Record a bounded local diagnostic line with the session key, route, whether recent context matched the route, whether the harness answered directly, selected tool names, model call count, and approximate context size. Do not log raw prompts, tool arguments, or tool outputs.
2. **Regression runner.** Extend the fake model with strict scripted steps, explicit detection of unexpected calls, and an `assertComplete` check for unused replies. Exercise the real Go turn loop across the persistent dashboard session, including session reload and a configured fake Playwright MCP server.
3. **Capability and budget consistency.** Apply the harness tool allowlist before building schemas and calculating token budget. Execute only tools represented in that turn's allowed set. Keep explicit `AllowTools` and approval guards authoritative.
4. **Action-observation cycle.** When the model emits multiple calls in one response, execute one call, append its observation, and query the model again before any subsequent action. This lets the 1B model react to page changes, failures, and tool results.
5. **Context assembly.** Use recent user turns together with the current request for semantic memory retrieval and route continuation. Preserve complete tool exchanges where possible. If the current request alone exceeds the available history budget, clip it deterministically with a visible marker so the model request remains within the configured context estimate.
6. **Progressive capability loading.** Keep skills and MCP details discoverable by summary and load full schemas or instructions only for selected capabilities. Retain the existing local EmbeddingGemma centroid approach; do not add a second classifier runtime in this change.

## Acceptance criteria

- The full natural exchange is replayed in the persisted `dashboard` session. With Playwright configured, the harness returns a Wikipedia fact and source, and the final fact request cannot produce an unrelated model answer.
- The trace shows the selected route and whether direct handling occurred without retaining user prompt contents.
- A model cannot execute a tool excluded by the turn's allowlist, even if it invents that tool name.
- Context budgeting uses exactly the schemas offered to the model.
- For a multi-call model response, only the first action runs before the next model observation.
- Memory retrieval for a short follow-up includes recent user context in the search query.
- History fitting bounds the approximate message size even when the current user message exceeds the available budget.
- Scripted tests fail on both extra model calls and unused scripted replies.
- `go test -count=1 ./...`, `go vet ./...`, and `go build -buildvcs=false -o dist\\SovereignAI.exe .` pass.

## Constraints

- Keep orchestration in the existing Go process.
- Keep local inference and existing approval guards.
- Add no telemetry service and no new classifier runtime.
- Preserve unrelated working-tree changes.
