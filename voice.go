package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dashboardKey names the dashboard's conversation. It is kept between visits.
const dashboardKey = "dashboard"

// spokenHint is appended to a message when the reply will be spoken aloud.
// timeHint tells the model the local time so greetings can fit the moment.
func timeHint(now time.Time) string {
	part := "night"
	switch h := now.Hour(); {
	case h >= 5 && h < 12:
		part = "morning"
	case h >= 12 && h < 17:
		part = "afternoon"
	case h >= 17 && h < 22:
		part = "evening"
	}
	return "\n\n[Local time: " + now.Format("Monday 2 January, 15:04") + " (" + part + ").]"
}

const spokenHint = "\n\n[This will be spoken aloud. Answer in one to three short, natural sentences, the way you would say it. " +
	"No lists, bullets, numbering, headings, bold or emoji. If there is a lot to say, give the main point and offer to say more.]"

type chatEvent struct {
	Delta    string       `json:"delta,omitempty"`
	Tool     string       `json:"tool,omitempty"`     // the assistant is using a tool
	Detail   string       `json:"detail,omitempty"`   // ... on this
	Approval *approvalAsk `json:"approval,omitempty"` // waiting for the user to allow or refuse something
	Done     bool         `json:"done,omitempty"`
	Error    string       `json:"error,omitempty"`
}

// handleVoiceChat runs one agent turn and streams it back as newline-delimited JSON
// ({"delta":"..."} ... {"done":true}). Tool use and approval requests arrive as events too.
func (a *app) handleVoiceChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	var in struct {
		Text   string `json:"text"`
		Spoken bool   `json:"spoken"` // the reply will be read aloud
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "invalid message", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		http.Error(w, "say something first", http.StatusBadRequest)
		return
	}
	if !a.sup.Agent().Wanted {
		http.Error(w, "your assistant is asleep; wake it up first", http.StatusConflict)
		return
	}
	if !a.beginAgentWork() {
		http.Error(w, "your assistant is already handling a message", http.StatusConflict)
		return
	}
	defer a.endAgentWork()
	// The model server takes a moment to load after waking; wait for it rather than fail.
	for deadline := time.Now().Add(3 * time.Minute); !a.llamaHealthy(llamaChatPort); {
		if time.Now().After(deadline) || r.Context().Err() != nil {
			http.Error(w, "your assistant is still waking up; please try again in a moment", http.StatusServiceUnavailable)
			return
		}
		time.Sleep(time.Second)
	}
	hint := timeHint(time.Now())
	if in.Spoken {
		// Small models follow what is nearest the end of the prompt, so the spoken-style
		// instruction rides on each message instead of only living in the persona files.
		hint += spokenHint
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	emit := func(event chatEvent) {
		_ = json.NewEncoder(w).Encode(event)
		if flusher != nil {
			flusher.Flush()
		}
	}
	err := a.runTurnObserved(r.Context(), turnOptions{Session: dashboardKey, Text: text, Hint: hint, Spoken: in.Spoken}, func(e agentEvent) {
		if e.Approval != nil {
			a.updateAgentWork("awaiting_approval", "Waiting for approval")
		} else if e.Tool != "" {
			a.updateAgentWork("working", "Using "+e.Tool)
		}
		emit(chatEvent{Delta: e.Delta, Tool: e.Tool, Detail: e.Detail, Approval: e.Approval})
	}, func(session string, messages []chatMsg, turnStart int, turnErr error) {
		go a.observeConversationTurn(a.ctx, session, messages, turnStart, turnErr)
	})
	a.agentWarm.Store(true)
	switch {
	case err == nil:
		emit(chatEvent{Done: true})
	case errors.Is(err, context.Canceled) || r.Context().Err() != nil:
		// the page went away; nothing to report
	default:
		emit(chatEvent{Error: err.Error()})
	}
}

// handleAgentReset forgets the dashboard conversation (POST /api/agent/reset).
func (a *app) handleAgentReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.resetSession(dashboardKey)
	writeJSON(w, map[string]any{"ok": true})
}

// inferenceBusy reports whether the chat model is generating right now, from
// llama-server's own slot state. It drives the sphere's "working" state.
func (a *app) inferenceBusy() bool {
	return a.inference.get(600*time.Millisecond, func() bool {
		client := http.Client{Timeout: 400 * time.Millisecond}
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/slots", llamaChatPort))
		if err != nil {
			return false
		}
		defer response.Body.Close()
		var slots []struct {
			Processing bool `json:"is_processing"`
		}
		if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&slots) != nil {
			return false
		}
		for _, slot := range slots {
			if slot.Processing {
				return true
			}
		}
		return false
	})
}

// startAgent is the agent service's start. The loop itself needs no process; this makes sure its
// folders exist, refreshes the persona files, and sends one short turn so the model server has
// processed the instructions before the user's first message.
func (a *app) startAgent() error {
	for _, dir := range []string{a.workspace, filepath.Join(a.workspace, "memory"), filepath.Join(a.workspace, "skills")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	if p := a.profile(); p.UserName != "" {
		if err := a.writePersona(p); err != nil {
			return err
		}
	}
	if err := a.waitLlama(2 * time.Minute); err != nil {
		return err
	}
	a.warmAgent()
	return nil
}

func (a *app) waitLlama(limit time.Duration) error {
	for deadline := time.Now().Add(limit); !a.llamaHealthy(llamaChatPort); time.Sleep(500 * time.Millisecond) {
		if a.ctx.Err() != nil {
			return a.ctx.Err()
		}
		if time.Now().After(deadline) {
			return errors.New("the language model did not become ready")
		}
	}
	return nil
}

// warmAgent runs a throwaway one-word turn so llama-server has the instructions in its cache.
func (a *app) warmAgent() {
	defer a.agentWarm.Store(true) // even on failure: never leave the dashboard waiting forever
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()
	messages := []chatMsg{{Role: "system", Content: a.systemPrompt(time.Now())}, {Role: "user", Content: "Reply with the single word: ready"}}
	if _, _, err := a.callModel(ctx, &modelGatewayCall{
		Session: "warmup", Step: -1, Messages: messages, Spoken: true, emit: func(agentEvent) {},
	}); err == nil {
		a.recordCommandOutput("Your assistant is warmed up and ready")
	}
}

// modelErrorText turns an OpenAI-style error object into something a person can read.
func modelErrorText(raw any) string {
	if object, ok := raw.(map[string]any); ok {
		if message, ok := object["message"].(string); ok && message != "" {
			if strings.Contains(strings.ToLower(message), "context") {
				return "That conversation has grown too long for me. Try again, and I'll start fresh."
			}
			return "Something went wrong on my side (" + message + "). Please try again."
		}
	}
	return "Something went wrong on my side. Please try again."
}
