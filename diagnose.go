package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type diagCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// diagnose reports whether a finished install has everything it should, and whether
// the running parts are healthy. It is read-only and quick (a few local probes).
func (a *app) diagnose() []diagCheck {
	var checks []diagCheck
	add := func(name string, ok bool, detail string) { checks = append(checks, diagCheck{name, ok, detail}) }
	fileCheck := func(name, path string) {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			add(name, false, "missing: "+path)
			return
		}
		add(name, true, formatBytes(info.Size()))
	}

	// ---- installed pieces
	if exe, ok := findFile(a.llamaRoot(), "llama-server.exe"); ok {
		add("llama.cpp runtime", true, filepath.Base(filepath.Dir(exe)))
	} else {
		add("llama.cpp runtime", false, "no backend installed under "+a.llamaRoot())
	}
	fileCheck("Chat model (MiniCPM-V 4.7 1B)", a.chatModel())
	fileCheck("Speculative-decoding draft (DSpark)", a.draftModel())
	fileCheck("Embedding model", a.embedModel())
	add("Workspace POSIX userland", a.workspaceUserlandReady(), a.userlandDir())
	if exe, ok := findFile(a.audioRoot(), "audiocpp_server.exe"); ok {
		add("Speech runtime", true, filepath.Base(filepath.Dir(exe)))
	} else {
		add("Speech runtime", false, "no backend installed under "+a.audioRoot())
	}
	fileCheck("Voice model (OmniVoice)", a.omniModel())
	fileCheck("Turn detection model (Smart Turn)", a.turnModel())
	add("Speech recognition model (Whistle)", a.whistleReady(), a.whistleDir())
	if profile := a.voiceProfile(); profile.RefWav != "" {
		add("Reference voice", true, "cloning from "+filepath.Base(profile.RefWav))
	} else {
		add("Reference voice", true, "none: using the described voice")
	}

	// ---- configuration
	for _, name := range []string{"IDENTITY.md", "SOUL.md"} {
		data, err := os.ReadFile(filepath.Join(a.workspace, name))
		add("Persona file "+name, err == nil && strings.Contains(string(data), "sovereign-ai:managed"), fmt.Sprintf("%d bytes", len(data)))
	}
	rules, _ := os.ReadFile(filepath.Join(a.workspace, "AGENTS.md"))
	add("Global rules in AGENTS.md", strings.Contains(string(rules), rulesBegin), "")
	add("Skills folder", isDir(filepath.Join(a.workspace, "skills")), fmt.Sprintf("%d skills", len(a.listSkills())))

	// ---- running parts
	add("Chat model server", a.llamaHealthy(llamaChatPort), fmt.Sprintf("port %d, backend %s", llamaChatPort, orDash(a.llamaBackendName())))
	add("Embedding server", a.llamaHealthy(llamaEmbedPort), fmt.Sprintf("port %d", llamaEmbedPort))
	voiceUp := a.audioHealthy()
	add("Speech server", voiceUp, fmt.Sprintf("port %d, backend %s", audioPort, orDash(a.audioBackendName())))
	if voiceUp {
		loaded := a.audioModelsLoaded()
		add("Voice and turn models loaded", loaded[omniModelID] && loaded[turnModelID], fmt.Sprintf("omnivoice=%v smart-turn=%v", loaded[omniModelID], loaded[turnModelID]))
	}
	agent := a.sup.Agent()
	add("Agent loop", agent.Awake, "assistant "+agent.State)
	for _, service := range a.sup.Status() {
		if service.Wanted {
			add("Process manager: "+service.Label, service.State == svcRunning, fmt.Sprintf("%s, %d restarts%s", service.State, service.Restarts, orDash2(service.Error)))
		}
	}
	return checks
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// audioModelsLoaded asks the speech server which models it has loaded.
func (a *app) audioModelsLoaded() map[string]bool {
	loaded := map[string]bool{}
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", audioPort))
	if err != nil {
		return loaded
	}
	defer response.Body.Close()
	var list struct {
		Data []struct {
			ID     string `json:"id"`
			Loaded bool   `json:"loaded"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&list) == nil {
		for _, m := range list.Data {
			loaded[m.ID] = m.Loaded
		}
	}
	return loaded
}

func (a *app) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	checks := a.diagnose()
	ok := true
	for _, c := range checks {
		ok = ok && c.OK
	}
	writeJSON(w, map[string]any{"ok": ok, "checks": checks})
}

func orDash2(s string) string {
	if s == "" {
		return ""
	}
	return ", " + s
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
