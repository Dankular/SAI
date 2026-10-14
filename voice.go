package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// gatewayPort is fixed so the dashboard can always reach the Gateway's
// OpenAI-compatible endpoint, which runs a real OpenClaw agent turn: tools,
// skills, memory and sessions all apply, exactly as in OpenClaw's own chat.
const (
	gatewayPort    = 28615
	dashboardAgent = "openclaw/main"
	dashboardKey   = "dashboard"
)

func (a *app) gatewayTokenPath() string { return filepath.Join(a.dataDir, "gateway.token") }

// gatewayToken returns the Gateway shared secret, creating it on first use.
// It lives only on disk and in OpenClaw's config; the browser never sees it.
func (a *app) gatewayToken() (string, error) {
	if data, err := os.ReadFile(a.gatewayTokenPath()); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := os.WriteFile(a.gatewayTokenPath(), []byte(token+"\n"), 0600); err != nil {
		return "", err
	}
	return token, nil
}

// gatewayConfigSteps are the OpenClaw settings that expose the chat endpoint.
func (a *app) gatewayConfigSteps() ([][]string, error) {
	token, err := a.gatewayToken()
	if err != nil {
		return nil, err
	}
	return [][]string{
		{"config", "set", "gateway.mode", "local"},
		{"config", "set", "gateway.auth.mode", "token"},
		{"config", "set", "gateway.auth.token", token},
		{"config", "set", "gateway.bind", "loopback"},
		{"config", "set", "gateway.port", fmt.Sprint(gatewayPort)},
		{"config", "set", "gateway.http.endpoints.chatCompletions.enabled", "true"},
	}, nil
}

// spokenHint is appended to a message when the reply will be spoken aloud.
const spokenHint = "\n\n[This will be spoken aloud. Answer in one to three short, natural sentences, the way you would say it. " +
	"No lists, bullets, numbering, headings, bold or emoji. If there is a lot to say, give the main point and offer to say more.]"

type chatEvent struct {
	Delta string `json:"delta,omitempty"`
	Done  bool   `json:"done,omitempty"`
	Error string `json:"error,omitempty"`
}

// handleVoiceChat sends one message to the OpenClaw agent and streams the reply
// back as newline-delimited JSON ({"delta":"..."} ... {"done":true}).
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
	a.mu.Lock()
	awake := a.state.GatewayRunning
	a.mu.Unlock()
	if !awake {
		http.Error(w, "your assistant is asleep; wake it up first", http.StatusConflict)
		return
	}
	// The Gateway takes about a minute to start answering; wait for it rather than fail.
	for deadline := time.Now().Add(3 * time.Minute); !a.gatewayReady(); {
		if time.Now().After(deadline) || r.Context().Err() != nil {
			http.Error(w, "your assistant is still waking up; please try again in a moment", http.StatusServiceUnavailable)
			return
		}
		time.Sleep(time.Second)
	}
	token, err := a.gatewayToken()
	if err != nil {
		http.Error(w, "could not read the gateway credentials", http.StatusInternalServerError)
		return
	}
	content := text
	if in.Spoken {
		// Small models follow what is nearest the end of the prompt, so the
		// spoken-style instruction rides on each message instead of only
		// living in the persona files.
		content += spokenHint
	}
	payload, _ := json.Marshal(map[string]any{
		"model":    dashboardAgent,
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": content}},
	})
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", gatewayPort), bytes.NewReader(payload))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Authorization", "Bearer "+token)
	upstream.Header.Set("x-openclaw-session-key", dashboardKey)
	response, err := (&http.Client{}).Do(upstream)
	if err != nil {
		http.Error(w, "your assistant is not answering yet; give it a moment and try again", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 600))
		http.Error(w, fmt.Sprintf("the assistant returned %s: %s", response.Status, strings.TrimSpace(string(detail))), http.StatusBadGateway)
		return
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
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error any `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Error != nil {
			emit(chatEvent{Error: gatewayErrorText(chunk.Error)})
			return
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				emit(chatEvent{Delta: choice.Delta.Content})
			}
		}
	}
	if err := scanner.Err(); err != nil && r.Context().Err() == nil {
		emit(chatEvent{Error: "the connection to your assistant was interrupted"})
		return
	}
	emit(chatEvent{Done: true})
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

// warmGateway sends one tiny turn once the Gateway is listening. OpenClaw
// initialises its tools lazily on the first agent run, which can take minutes;
// doing it now means the user's first message is not the one that pays for it.
func (a *app) warmGateway() {
	defer a.gwWarm.Store(true) // even on failure: never leave the dashboard waiting forever
	for deadline := time.Now().Add(6 * time.Minute); !a.gatewayReady(); time.Sleep(2 * time.Second) {
		if a.ctx.Err() != nil || time.Now().After(deadline) {
			return
		}
	}
	token, err := a.gatewayToken()
	if err != nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"model":    dashboardAgent,
		"messages": []map[string]string{{"role": "user", "content": "Reply with the single word: ready"}},
	})
	request, err := http.NewRequestWithContext(a.ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", gatewayPort), bytes.NewReader(payload))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("x-openclaw-session-key", "warmup")
	client := http.Client{Timeout: 4 * time.Minute}
	if response, err := client.Do(request); err == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		a.recordCommandOutput("Your assistant is warmed up and ready")
	}
}

// gatewayReady reports whether OpenClaw says it is ready (its /readyz probe),
// cached briefly. A bare open port is not enough: the Gateway binds its port
// well before it finishes loading plugins and can answer.
func (a *app) gatewayReady() bool {
	return a.gwReady.get(time.Second, func() bool {
		if !a.llamaHealthy(llamaChatPort) {
			return false
		}
		client := http.Client{Timeout: 1500 * time.Millisecond}
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", gatewayPort))
		if err != nil {
			return false
		}
		response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
}

// gatewayErrorText turns an OpenAI-style error object into something a person can read.
func gatewayErrorText(raw any) string {
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
