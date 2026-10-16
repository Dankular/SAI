package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelGatewayHooksCoverRequestStreamAndResponse(t *testing.T) {
	a := newTestApp(t)
	model := useFakeModel(t,
		textChunk("Searching now.")+"\n"+toolChunks("call_1", "extension", `{"action":"discover","query":"calendar"}`),
	)
	var phases []string
	var streamed []modelGatewayStreamEvent
	var afterResult *modelGatewayResult
	var afterErr error
	a.addModelGatewayHook(modelGatewayHook{
		BeforeCall: func(_ context.Context, call *modelGatewayCall) error {
			phases = append(phases, "before")
			call.Messages[0].Content += "\nGateway note."
			return nil
		},
		OnStream: func(_ context.Context, _ *modelGatewayCall, event modelGatewayStreamEvent) error {
			phases = append(phases, event.Kind)
			streamed = append(streamed, event)
			return nil
		},
		AfterCall: func(_ context.Context, _ *modelGatewayCall, result *modelGatewayResult, err error) {
			phases = append(phases, "after")
			afterResult, afterErr = result, err
		},
	})

	var emitted []agentEvent
	reply, calls, err := a.callModel(context.Background(), &modelGatewayCall{
		Session: "gateway-test", Step: -1,
		Messages: []chatMsg{{Role: "system", Content: "System."}, {Role: "user", Content: "Find a calendar tool."}},
		Tools:    []map[string]any{{"type": "function"}},
		emit:     func(event agentEvent) { emitted = append(emitted, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Searching now." || len(calls) != 1 || calls[0].Function.Name != "extension" {
		t.Fatalf("reply=%q calls=%+v", reply, calls)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model got %d requests, want 1", len(model.requests))
	}
	messages := model.requests[0]["messages"].([]any)
	if !strings.Contains(messages[0].(map[string]any)["content"].(string), "Gateway note.") {
		t.Error("pre-call hook changes were not sent to the model")
	}
	if len(streamed) != 2 || streamed[0].Kind != "text" || streamed[1].Kind != "tool_call" || streamed[1].Arguments == "" {
		t.Errorf("stream events = %+v", streamed)
	}
	if len(emitted) != 1 || emitted[0].Delta != "Searching now." {
		t.Errorf("client stream = %+v", emitted)
	}
	if afterErr != nil || afterResult == nil || afterResult.Text != reply || len(afterResult.ToolCalls) != 1 || afterResult.Duration < 0 {
		t.Errorf("post-call result=%+v error=%v", afterResult, afterErr)
	}
	if got := strings.Join(phases, ","); got != "before,text,tool_call,after" {
		t.Errorf("hook order = %q", got)
	}
}

func TestMarketplacePreflightRoutesConfidentRequestsAndSkipsOrdinaryQuestions(t *testing.T) {
	a := newTestApp(t)
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var response struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		for i, input := range request.Input {
			lower := strings.ToLower(input)
			vector := []float32{0, 1}
			if strings.Contains(lower, "find an mcp server") ||
				strings.Contains(lower, "search a marketplace") ||
				strings.Contains(lower, "discover and compare external agent tools") ||
				strings.Contains(lower, "connect the assistant to a new service") ||
				strings.Contains(lower, "look for a tool the agent can install") ||
				strings.Contains(lower, "which external tool should i add") ||
				strings.Contains(lower, "find an mcp tool for calendar") {
				vector = []float32{1, 0}
			}
			response.Data = append(response.Data, struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			}{Index: i, Embedding: vector})
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode embeddings: %v", err)
		}
	}))
	defer embeddings.Close()
	oldEndpoint := embedEndpoint
	embedEndpoint = embeddings.URL
	t.Cleanup(func() { embedEndpoint = oldEndpoint })

	var discoveries []string
	discover := func(_ context.Context, query string) (string, error) {
		discoveries = append(discoveries, query)
		return "Calendar MCP candidate: calendar-bridge", nil
	}

	marketplaceCall := &modelGatewayCall{
		Step:         0,
		UserText:     "Find an MCP tool for calendar.",
		RouteContext: "I need to manage calendar events.",
		Messages:     []chatMsg{{Role: "system", Content: "Base system prompt."}},
	}
	if err := a.marketplacePreflightWith(context.Background(), marketplaceCall, discover); err != nil {
		t.Fatal(err)
	}
	if len(discoveries) != 1 || discoveries[0] != marketplaceCall.UserText {
		t.Fatalf("discoveries = %q, want one search for the current request", discoveries)
	}
	if !strings.Contains(marketplaceCall.Messages[0].Content, "Calendar MCP candidate: calendar-bridge") {
		t.Fatal("marketplace candidate was not added to the model's system context")
	}

	ordinaryCall := &modelGatewayCall{
		Step:     0,
		UserText: "Explain how calendar invitations work.",
		Messages: []chatMsg{{Role: "system", Content: "Base system prompt."}},
	}
	if err := a.marketplacePreflightWith(context.Background(), ordinaryCall, discover); err != nil {
		t.Fatal(err)
	}
	if len(discoveries) != 1 {
		t.Errorf("ordinary question triggered marketplace search; discoveries = %q", discoveries)
	}
	if ordinaryCall.Messages[0].Content != "Base system prompt." {
		t.Errorf("ordinary question changed system context: %q", ordinaryCall.Messages[0].Content)
	}
}
