package main

import (
	"context"
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
