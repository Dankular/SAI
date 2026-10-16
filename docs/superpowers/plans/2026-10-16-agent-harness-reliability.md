# Agent harness reliability implementation plan

## 1. Lock down model test behavior

- Add strict scripted-model helpers that report unexpected calls and unconsumed responses.
- Add a multi-turn dashboard-session regression using the current persistent session key, a configured fake Playwright MCP server, and a session reload between turns.
- Assert the sourced fact, direct route, model call count, and route diagnostic.

## 2. Add route diagnostics

- Add a stable route field to harness plans and set it on direct and tool-routed paths.
- Record a short, bounded diagnostic through the existing local command log. Include route, context-match flag, direct/model decision, selected tool names, and counts only; omit raw user text and tool arguments.
- Add tests that assert the route classification and that trace output does not contain user message contents.

## 3. Align visible tools, executable tools, and context budget

- Factor tool filtering into a single helper.
- Plan the turn before building tool schemas.
- Build schemas and calculate token reservation from the filtered list.
- Pass the same filtered tool list to execution so hidden tool names cannot be invoked.
- Add tests for schema exposure, hidden-tool rejection, and budget calculation.

## 4. Enforce action-observation sequencing

- Keep one tool call from a model response in the conversation at a time.
- Execute that action, append its result, then call the model again before another action.
- Add a two-action test proving the second action starts only after the first result appears in model input.

## 5. Improve context and retrieval for long conversations

- Build a retrieval query from the current request and recent user turns.
- Make history fitting enforce its approximate budget even for a single oversized current request, preserving the beginning and end around an explicit truncation marker.
- Test short follow-ups, oversized input, and tool-exchange boundary behavior.

## 6. Format, test, build, and inspect the diff

- Run `gofmt` on changed Go files.
- Run `go test -count=1 ./...` and `go vet ./...`.
- Build `dist\\SovereignAI.exe` with `go build -buildvcs=false`.
- Review the focused diff and confirm unrelated existing modifications remain intact.
