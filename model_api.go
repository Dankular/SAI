package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

type chatModelOption struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Quant     string `json:"quantization"`
	Installed bool   `json:"installed"`
}

type chatModelSpec struct {
	ID    string
	Name  string
	Repo  string
	File  string
	Quant string
	Draft *draftSpec // a speculative-decoding partner trained for this model, if one exists
}

// draftSpec is a small DSpark model that proposes tokens the chat model then verifies, so replies come out
// several times faster with identical text. MaxTokens is how many it proposes at a time.
type draftSpec struct {
	Repo, File string
	MaxTokens  int
}

var chatModels = []chatModelSpec{
	{ID: "minicpm5-2b", Name: "MiniCPM5 2B", Repo: "openbmb/MiniCPM5-2B-GGUF", File: "MiniCPM5-2B-Q8_0.gguf", Quant: "Q8_0",
		Draft: &draftSpec{Repo: "openbmb/MiniCPM5-2B-DSpark-GGUF", File: "MiniCPM5-2.6B-DSpark.gguf", MaxTokens: 7}},
	{ID: "minicpm5-1b", Name: "MiniCPM5 1B", Repo: "openbmb/MiniCPM5-1B-GGUF", File: "MiniCPM5-1B-Q8_0.gguf", Quant: "Q8_0"},
	{ID: "nanbeige4.1-3b", Name: "Nanbeige4.1 3B", Repo: "Edge-Quant/Nanbeige4.1-3B-Q4_K_M-GGUF", File: "nanbeige4.1-3b-q4_k_m.gguf", Quant: "Q4_K_M"},
	{ID: "qwen3.5-4b", Name: "Qwen3.5 4B", Repo: "gguf/Qwen3.5-4B-GGUF", File: "qwen3.5-4b-Q4_K_M.gguf", Quant: "Q4_K_M"},
	{ID: "ministral-3-3b", Name: "Ministral 3 3B", Repo: "mistralai/Ministral-3-3B-Instruct-2512-GGUF", File: "Ministral-3-3B-Instruct-2512-Q4_K_M.gguf", Quant: "Q4_K_M"},
	{ID: "phi-4-mini", Name: "Phi-4-mini", Repo: "bartowski/microsoft_Phi-4-mini-instruct-GGUF", File: "microsoft_Phi-4-mini-instruct-Q4_K_M.gguf", Quant: "Q4_K_M"},
	{ID: "lfm2.5-2.6b", Name: "LFM2.5 2.6B", Repo: "LiquidAI/LFM2.5-2.6B-GGUF", File: "LFM2.5-2.6B-Q4_K_M.gguf", Quant: "Q4_K_M"},
}

func chatModelByID(id string) (chatModelSpec, bool) {
	for _, model := range chatModels {
		if model.ID == id {
			return model, true
		}
	}
	return chatModels[0], false
}

func (a *app) selectedChatModel() chatModelSpec {
	a.mu.Lock()
	id := a.state.ChatModel
	if a.state.ModelSwitching && a.state.ModelSwitchApplying {
		id = a.state.ModelSwitchTarget
	}
	a.mu.Unlock()
	model, ok := chatModelByID(id)
	if !ok {
		return chatModels[0]
	}
	return model
}

func (a *app) modelResponse() map[string]any {
	a.mu.Lock()
	state := a.state
	a.mu.Unlock()
	selected, ok := chatModelByID(state.ChatModel)
	if !ok {
		selected = chatModels[0]
	}
	options := make([]chatModelOption, 0, len(chatModels))
	for _, model := range chatModels {
		options = append(options, chatModelOption{
			ID: model.ID, Name: model.Name, Quant: model.Quant,
			Installed: isFile(filepath.Join(a.modelsDir(), model.File)),
		})
	}
	return map[string]any{
		"models": options, "current": selected.ID, "switching": state.ModelSwitching,
		"target": state.ModelSwitchTarget, "status": state.ModelSwitchStatus, "error": state.ModelSwitchError,
	}
}

func (a *app) handleModel(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, a.modelResponse())
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		var request struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid model selection", http.StatusBadRequest)
			return
		}
		model, ok := chatModelByID(strings.TrimSpace(request.ID))
		if !ok {
			http.Error(w, "unknown model", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		if !a.state.SetupComplete || a.state.Updating {
			a.mu.Unlock()
			http.Error(w, "the assistant must finish setup before changing models", http.StatusConflict)
			return
		}
		if a.state.ModelSwitching {
			a.mu.Unlock()
			http.Error(w, "a model change is already in progress", http.StatusConflict)
			return
		}
		current, ok := chatModelByID(a.state.ChatModel)
		if !ok {
			current = chatModels[0]
		}
		if current.ID == model.ID {
			a.mu.Unlock()
			writeJSON(w, a.modelResponse())
			return
		}
		a.state.ModelSwitching = true
		a.state.ModelSwitchApplying = false
		a.state.ModelSwitchTarget = model.ID
		a.state.ModelSwitchStatus = "Preparing " + model.Name + "..."
		a.state.ModelSwitchError = ""
		_ = a.persistLocked()
		a.mu.Unlock()
		go a.switchChatModel(model)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(a.modelResponse())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *app) switchChatModel(model chatModelSpec) {
	a.updateModelSwitch("Downloading "+model.Name+" from Hugging Face...", false)
	modelPath := filepath.Join(a.modelsDir(), model.File)
	if err := a.ensureModel(model.Repo, model.File, modelPath); err != nil {
		a.finishModelSwitch(fmt.Errorf("could not download %s: %w", model.Name, err))
		return
	}

	if a.sup != nil {
		a.updateModelSwitch("Restarting the local model server...", true)
		if err := a.sup.RestartAndWait(svcModels, 7*time.Minute); err != nil {
			a.mu.Lock()
			a.state.ModelSwitchApplying = false
			a.mu.Unlock()
			if a.sup.State(svcModels) == svcFailed {
				a.sup.Want(svcModels, true)
			}
			a.finishModelSwitch(fmt.Errorf("%s was downloaded but did not start: %w", model.Name, err))
			return
		}
	}

	a.mu.Lock()
	a.state.ChatModel = model.ID
	a.state.ModelSwitching = false
	a.state.ModelSwitchApplying = false
	a.state.ModelSwitchTarget = ""
	a.state.ModelSwitchStatus = model.Name + " is ready."
	a.state.ModelSwitchError = ""
	_ = a.persistLocked()
	a.mu.Unlock()
	a.agentWarm.Store(false)
	if err := a.writePersona(a.profile()); err != nil {
		a.recordCommandOutput("Could not refresh the model details in the assistant's identity: " + err.Error())
	}
	a.recordCommandOutput("Active chat model changed to " + model.Name)
}

func (a *app) updateModelSwitch(status string, applying bool) {
	a.mu.Lock()
	a.state.ModelSwitchStatus = status
	a.state.ModelSwitchApplying = applying
	_ = a.persistLocked()
	a.mu.Unlock()
}

func (a *app) finishModelSwitch(err error) {
	a.mu.Lock()
	a.state.ModelSwitching = false
	a.state.ModelSwitchApplying = false
	a.state.ModelSwitchTarget = ""
	a.state.ModelSwitchStatus = "Model change failed."
	a.state.ModelSwitchError = err.Error()
	_ = a.persistLocked()
	a.mu.Unlock()
	a.recordCommandOutput(err.Error())
}
