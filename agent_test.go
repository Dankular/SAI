package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeModel plays the chat model: each request gets the next scripted reply, sent as the
// server-sent events llama-server would send. It records what it was asked.
type fakeModel struct {
	mu       sync.Mutex
	replies  []string // each is a list of SSE data payloads joined by newlines
	requests []map[string]any
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	n := len(f.requests) - 1
	reply := ""
	if n < len(f.replies) {
		reply = f.replies[n]
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, line := range strings.Split(reply, "\n") {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func useFakeModel(t *testing.T, replies ...string) *fakeModel {
	t.Helper()
	f := &fakeModel{replies: replies}
	server := httptest.NewServer(f)
	old := chatEndpoint
	chatEndpoint = server.URL + "/v1/chat/completions"
	t.Cleanup(func() { chatEndpoint = old; server.Close() })
	return f
}

func textChunk(s string) string {
	b, _ := json.Marshal(s)
	return `{"choices":[{"delta":{"content":` + string(b) + `}}]}`
}

func toolChunks(id, name string, argParts ...string) string {
	var lines []string
	for i, part := range argParts {
		b, _ := json.Marshal(part)
		head := ""
		if i == 0 {
			head = `"id":"` + id + `","function":{"name":"` + name + `","arguments":` + string(b) + `}`
		} else {
			head = `"function":{"arguments":` + string(b) + `}`
		}
		lines = append(lines, `{"choices":[{"delta":{"tool_calls":[{"index":0,`+head+`}]}}]}`)
	}
	return strings.Join(lines, "\n")
}

func collect(events *[]agentEvent) func(agentEvent) {
	return func(e agentEvent) { *events = append(*events, e) }
}

func replyText(events []agentEvent) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(e.Delta)
	}
	return b.String()
}

func TestAgentLoopRunsToolsThenAnswers(t *testing.T) {
	a := newTestApp(t)
	_ = os.MkdirAll(a.workspace, 0700)
	_ = os.WriteFile(filepath.Join(a.workspace, "notes.txt"), []byte("the code word is pelican\n"), 0600)
	model := useFakeModel(t,
		textChunk("Let me look. ")+"\n"+toolChunks("call_1", "read_file", `{"pa`, `th":"notes.txt"}`),
		textChunk("The code word is pelican."),
	)
	var events []agentEvent
	err := a.runTurn(context.Background(), turnOptions{Session: "t", Text: "what is the code word?", Hint: "\n[hint]"}, collect(&events))
	if err != nil {
		t.Fatal(err)
	}
	if got := replyText(events); got != "The code word is pelican." { // the harness drops the narration "Let me look."
		t.Errorf("reply = %q", got)
	}
	var tool *agentEvent
	for i := range events {
		if events[i].Tool != "" {
			tool = &events[i]
		}
	}
	if tool == nil || tool.Tool != "read_file" || tool.Detail != "notes.txt" {
		t.Errorf("tool event = %+v", tool)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model called %d times, want 2", len(model.requests))
	}
	// The second request carries the tool call and its result.
	messages := model.requests[1]["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	if last["role"] != "tool" || !strings.Contains(last["content"].(string), "pelican") {
		t.Errorf("tool result was not sent back: %v", last)
	}
	// The hint rode on the model's copy only; the saved conversation is clean.
	s := a.session("t")
	if s.msgs[0].Content != "what is the code word?" {
		t.Errorf("saved user message = %q", s.msgs[0].Content)
	}
	first := model.requests[0]["messages"].([]any)
	if u := first[len(first)-1].(map[string]any)["content"].(string); !strings.HasSuffix(u, "[hint]") {
		t.Errorf("hint missing from the model's copy: %q", u)
	}
	if len(s.msgs) != 4 { // user, assistant+call, tool, assistant
		t.Errorf("saved %d messages, want 4", len(s.msgs))
	}
	// The conversation survives a restart.
	a.agent.sessions = nil
	if got := a.session("t"); len(got.msgs) != 4 {
		t.Errorf("reloaded %d messages, want 4", len(got.msgs))
	}
}

func TestShellWaitsForApproval(t *testing.T) {
	a := newTestApp(t)
	_ = os.MkdirAll(a.workspace, 0700)
	useFakeModel(t,
		toolChunks("c1", "shell", `{"command":"echo hi"}`),
		textChunk("Okay, I won't."),
	)
	asked := make(chan *approvalAsk, 1)
	done := make(chan error, 1)
	go func() {
		done <- a.runTurn(context.Background(), turnOptions{Session: "s", Text: "say hi in a shell"}, func(e agentEvent) {
			if e.Approval != nil {
				asked <- e.Approval
			}
		})
	}()
	select {
	case ask := <-asked:
		if !strings.Contains(ask.Summary, "echo hi") {
			t.Errorf("summary = %q", ask.Summary)
		}
		if !a.resolveApproval(ask.ID, false) {
			t.Fatal("approval not pending")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no approval was requested")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s := a.session("s")
	var toolResult string
	for _, m := range s.msgs {
		if m.Role == "tool" {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "did not allow") {
		t.Errorf("a refused command must tell the model so, got %q", toolResult)
	}
}

func TestShellRequiresWorkspaceUserland(t *testing.T) {
	a := newTestApp(t)
	out, err := toolShell(context.Background(), a, map[string]any{"command": "echo sovereign"})
	if out != "" || err == nil || !strings.Contains(err.Error(), "workspace POSIX userland is not installed") {
		t.Errorf("out=%q err=%v; expected the missing private userland error", out, err)
	}
}

func TestUnknownToolAndBadArgumentsAreReportedToTheModel(t *testing.T) {
	a := newTestApp(t)
	tools := a.agentTools()
	var call toolCall
	call.Function.Name = "no_such_tool"
	if got := a.execTool(context.Background(), tools, call, func(agentEvent) {}); !strings.Contains(got, "no tool called") {
		t.Errorf("got %q", got)
	}
	call.Function.Name, call.Function.Arguments = "read_file", "{not json"
	if got := a.execTool(context.Background(), tools, call, func(agentEvent) {}); !strings.Contains(got, "not valid JSON") {
		t.Errorf("got %q", got)
	}
}

func TestFitHistoryNeverSplitsAToolExchange(t *testing.T) {
	var msgs []chatMsg
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			chatMsg{Role: "user", Content: strings.Repeat("q", 300)},
			chatMsg{Role: "assistant", ToolCalls: []toolCall{{ID: "x", Type: "function"}}},
			chatMsg{Role: "tool", ToolCallID: "x", Content: strings.Repeat("r", 3000)},
			chatMsg{Role: "assistant", Content: strings.Repeat("a", 300)},
		)
	}
	got := fitHistory(msgs, 1500)
	if len(got) == 0 || got[0].Role != "user" {
		t.Fatalf("history must start at a user message: %+v", got)
	}
	if len(got) >= len(msgs) {
		t.Error("nothing was dropped")
	}
	if got[len(got)-1].Content != msgs[len(msgs)-1].Content {
		t.Error("the newest message must be kept")
	}
	// Tool output from earlier turns is shortened.
	for i, m := range got {
		if m.Role == "tool" && i < len(got)-4 && len(m.Content) > 600 {
			t.Errorf("old tool output not shortened (%d bytes)", len(m.Content))
		}
	}
	// A single oversized request is still kept.
	if one := fitHistory([]chatMsg{{Role: "user", Content: strings.Repeat("z", 100000)}}, 100); len(one) != 1 {
		t.Errorf("the current request must always be kept, got %d", len(one))
	}
}

func TestSystemPromptCarriesIdentityPreferencesAndSkills(t *testing.T) {
	a := newTestApp(t)
	if err := a.writePersona(Profile{UserName: "Ada", AgentName: "Sam", Purpose: "planning", ReplyStyle: "concise"}); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(a.workspace, "skills", "weekly-plan")
	_ = os.MkdirAll(skillDir, 0700)
	_ = os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: weekly-plan\ndescription: \"Plan the week from the calendar\"\n---\n# Weekly plan\nStep one.\n"), 0600)
	prompt := a.systemPrompt(time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC))
	for _, want := range []string{"=== IDENTITY.md ===", "=== SOUL.md ===", "=== AGENTS.md ===", "=== USER.md ===", "You are Sam", "Name: Ada", "weekly-plan: Plan the week from the calendar", "Friday 16 October 2026, 09:30"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt is missing %q", want)
		}
	}
	if strings.Contains(prompt, managedMarker) {
		t.Error("the managed-file marker should not reach the model")
	}
	if strings.Contains(prompt, "=== MEMORY.md ===") {
		t.Error("an empty memory file should not take up room in the prompt")
	}
	if len(prompt) > 4500 {
		t.Errorf("the prompt is %d characters; a one-billion-parameter model needs it short", len(prompt))
	}
	body, err := a.readSkill("Weekly-Plan")
	if err != nil || !strings.Contains(body, "Step one.") {
		t.Errorf("readSkill: %q, %v", body, err)
	}
	if _, err := a.readSkill("nope"); err == nil {
		t.Error("an unknown skill should be an error")
	}
}

func TestRememberWritesToUserAndMemoryFilesOnce(t *testing.T) {
	a := newTestApp(t)
	_ = a.writePersona(Profile{UserName: "Ada", AgentName: "Sam", Purpose: "planning"})
	ctx := context.Background()
	if _, err := toolRemember(ctx, a, map[string]any{"text": "Prefers tea over coffee", "file": "user"}); err != nil {
		t.Fatal(err)
	}
	if out, _ := toolRemember(ctx, a, map[string]any{"text": "Prefers tea over coffee", "file": "user"}); !strings.Contains(out, "Already") {
		t.Errorf("duplicate not detected: %q", out)
	}
	if _, err := toolRemember(ctx, a, map[string]any{"text": "Dentist on 3 November"}); err != nil {
		t.Fatal(err)
	}
	user, _ := os.ReadFile(filepath.Join(a.workspace, "USER.md"))
	memory, _ := os.ReadFile(filepath.Join(a.workspace, "MEMORY.md"))
	if strings.Count(string(user), "Prefers tea") != 1 || !strings.Contains(string(memory), "Dentist on 3 November") {
		t.Errorf("USER.md:\n%s\nMEMORY.md:\n%s", user, memory)
	}
}

func TestMemorySearchFallsBackToWordsWithoutTheEmbeddingServer(t *testing.T) {
	a := newTestApp(t)
	_ = a.writePersona(Profile{UserName: "Ada", AgentName: "Sam", Purpose: "planning"})
	old := embedEndpoint
	embedEndpoint = "http://127.0.0.1:1/v1/embeddings" // nothing listens here
	t.Cleanup(func() { embedEndpoint = old })
	_, _ = toolRemember(context.Background(), a, map[string]any{"text": "The spare key is under the blue flowerpot"})
	hits, err := a.searchMemory(context.Background(), "where is the spare key", 3)
	if err != nil || len(hits) == 0 || !strings.Contains(hits[0].Text, "flowerpot") {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if hits, _ := a.searchMemory(context.Background(), "submarine", 3); len(hits) != 0 {
		t.Errorf("unrelated query matched: %+v", hits)
	}
}

func TestMemorySearchUsesEmbeddingsWhenAvailable(t *testing.T) {
	a := newTestApp(t)
	_ = a.writePersona(Profile{UserName: "Ada", AgentName: "Sam", Purpose: "planning"})
	_, _ = toolRemember(context.Background(), a, map[string]any{"text": "Ada has a cat called Biscuit"})
	// A toy embedding: texts about cats or pets point one way, everything else the other.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var out struct {
			Data []item `json:"data"`
		}
		for i, text := range in.Input {
			v := []float32{0, 1}
			if strings.Contains(strings.ToLower(text), "cat") || strings.Contains(strings.ToLower(text), "pet") {
				v = []float32{1, 0}
			}
			out.Data = append(out.Data, item{i, v})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()
	old := embedEndpoint
	embedEndpoint = server.URL
	t.Cleanup(func() { embedEndpoint = old })
	hits, err := a.searchMemory(context.Background(), "what pet do I have", 2)
	if err != nil || len(hits) == 0 || !strings.Contains(hits[0].Text, "Biscuit") {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
}

func TestPathsOutsideTheWorkspaceNeedApproval(t *testing.T) {
	a := newTestApp(t)
	_ = os.MkdirAll(a.workspace, 0700)
	var read, write *agentTool
	tools := a.agentTools()
	for i := range tools {
		switch tools[i].Name {
		case "read_file":
			read = &tools[i]
		case "write_file":
			write = &tools[i]
		}
	}
	if ask := read.Guard(a, map[string]any{"path": "notes.txt"}); ask != "" {
		t.Errorf("workspace read should not ask: %q", ask)
	}
	outside := filepath.Join(filepath.Dir(a.workspace), "elsewhere.txt")
	if ask := write.Guard(a, map[string]any{"path": outside}); ask == "" {
		t.Error("writing outside the workspace must ask")
	}
	if ask := read.Guard(a, map[string]any{"path": filepath.Join("..", "..", "secret.txt")}); ask == "" {
		t.Error("a path that climbs out of the workspace must ask")
	}
}

func TestWebToolsRefuseThisComputer(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "secret") }))
	defer local.Close()
	_, _, err := fetchPublic(context.Background(), local.URL, 1024)
	if err == nil || !strings.Contains(err.Error(), "local network") {
		t.Errorf("a loopback address must be refused, got %v", err)
	}
	if _, _, err := fetchPublic(context.Background(), "file:///c:/windows/win.ini", 1024); err == nil {
		t.Error("non-web schemes must be refused")
	}
}

func TestHTMLToText(t *testing.T) {
	got := htmlToText(`<html><head><title>x</title><style>p{}</style></head><body><h1>Hi &amp; bye</h1><p>One</p><script>evil()</script><p>Two</p></body></html>`)
	if strings.Contains(got, "evil") || strings.Contains(got, "<") || !strings.Contains(got, "Hi & bye") || !strings.Contains(got, "Two") {
		t.Errorf("got %q", got)
	}
}

// A small model sometimes calls remember to answer a recall question. The answer is already in the
// notes, so nothing may be written; a genuinely new fact still is.
func TestRememberSkipsFactsThatAreAlreadyInTheNotes(t *testing.T) {
	a := newTestApp(t)
	_ = a.writePersona(Profile{UserName: "Ada", AgentName: "Sam", Purpose: "planning"})
	_ = os.MkdirAll(filepath.Join(a.workspace, "memory"), 0700)
	_ = os.WriteFile(filepath.Join(a.workspace, "memory", "2026-10-01.md"), []byte("# Notes\n\n- Ada's neighbour Priya waters the plants while she is away.\n- The Rotterdam trip budget was 1,800 euros.\n"), 0600)
	ctx := context.Background()
	for _, restated := range []string{"Priya waters the plants when I'm traveling.", "Rotterdam trip budget: 1,800 euros."} {
		if out, _ := toolRemember(ctx, a, map[string]any{"text": restated, "file": "user"}); !strings.Contains(out, "Nothing was added") {
			t.Errorf("%q should have been recognised as known, got %q", restated, out)
		}
	}
	user, _ := os.ReadFile(filepath.Join(a.workspace, "USER.md"))
	if strings.Contains(string(user), "Priya") || strings.Contains(string(user), "Rotterdam") {
		t.Errorf("a known fact was written again:\n%s", user)
	}
	if out, _ := toolRemember(ctx, a, map[string]any{"text": "Prefers tea over coffee", "file": "user"}); !strings.Contains(out, "Saved") {
		t.Errorf("a new fact must be saved, got %q", out)
	}
}

// Anything secret-looking from the notes must never leave in a web query or address.
func TestWebToolsRefuseToSendSecretsFromTheNotes(t *testing.T) {
	a := newTestApp(t)
	_ = a.writePersona(Profile{UserName: "Ada", AgentName: "Sam", Purpose: "planning"})
	_ = os.MkdirAll(filepath.Join(a.workspace, "memory"), 0700)
	_ = os.WriteFile(filepath.Join(a.workspace, "memory", "n.md"), []byte("# Notes\n\n- The cottage wifi password is bluewhale42.\n- Ordered 2000 screws.\n"), 0600)
	ctx := context.Background()
	if _, err := toolWebSearch(ctx, a, map[string]any{"query": "Cornwall cottage wifi password bluewhale42"}); err == nil || !strings.Contains(err.Error(), "private information") {
		t.Errorf("a query carrying the password must be refused, got %v", err)
	}
	if _, err := toolWebFetch(ctx, a, map[string]any{"url": "https://example.com/?p=BlueWhale42"}); err == nil || !strings.Contains(err.Error(), "private information") {
		t.Errorf("an address carrying the password must be refused, got %v", err)
	}
	if err := a.refuseToSendSecrets("how many screws are in a box of 2000"); err != nil {
		t.Errorf("ordinary numbers and words must not be blocked: %v", err)
	}
	if err := a.refuseToSendSecrets("what is the capital of Portugal"); err != nil {
		t.Errorf("an ordinary query must not be blocked: %v", err)
	}
}
