package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The dashboard's view of memory: everything the assistant remembers about its one user, why it
// believes it, and the controls to correct it. All of it reads and writes the same files the
// agent uses, so what the page shows is exactly what the model sees.

type apiNote struct {
	File string `json:"file"`
	Text string `json:"text"`
	Date string `json:"date,omitempty"`
}

type apiEvent struct {
	ID        string                `json:"id"`
	Event     string                `json:"event"`
	Summary   string                `json:"summary"`
	Status    string                `json:"status"`
	Scope     string                `json:"scope"`
	CreatedAt string                `json:"created_at"`
	Pinned    bool                  `json:"pinned"`
	Uses      int                   `json:"uses"`
	Evidence  []memoryEventEvidence `json:"evidence,omitempty"`
}

type apiMemory struct {
	Notes   []apiNote       `json:"notes"`
	Events  []apiEvent      `json:"events"`
	Profile string          `json:"profile"`
	Jobs    []cronJob       `json:"jobs"`
	Skills  []skillProposal `json:"skills"`
}

func (a *app) memorySnapshot() apiMemory {
	out := apiMemory{Notes: []apiNote{}, Events: []apiEvent{}, Jobs: a.listCronJobs(), Skills: append([]skillProposal{}, a.listSkillProposals()...)}
	for _, name := range []string{"USER.md", "MEMORY.md"} {
		data, err := os.ReadFile(filepath.Join(a.workspace, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "- ") {
				continue
			}
			body := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			date := ""
			if m := noteDateRE.FindStringSubmatch(body); m != nil {
				date = m[1]
				body = strings.TrimSpace(noteDateRE.ReplaceAllString(body, ""))
			}
			out.Notes = append(out.Notes, apiNote{File: name, Text: body, Date: date})
		}
	}
	events, _ := a.loadMemoryEvents()
	superseded := map[string]bool{}
	for _, e := range events {
		if e.SupersedesID != "" {
			superseded[e.SupersedesID] = true
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		status := e.Status
		if superseded[e.ID] {
			status = "superseded"
		} else if e.Event == "task_unfinished" && staleEvent(e, time.Now()) {
			status = "stale"
		}
		use := a.memoryUseOf("event:" + e.ID)
		out.Events = append(out.Events, apiEvent{
			ID: e.ID, Event: e.Event, Summary: e.Summary, Status: status, Scope: e.Scope, CreatedAt: e.CreatedAt,
			Pinned: use.Pinned, Uses: use.Uses, Evidence: e.Evidence,
		})
	}
	out.Profile = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(a.profileBlock()), "=== What you have learned about the user ==="))
	return out
}

// removeNoteExact deletes the one bullet whose text (without its date) equals text.
func (a *app) removeNoteExact(file, text string) bool {
	if file != "USER.md" && file != "MEMORY.md" {
		return false
	}
	path := filepath.Join(a.workspace, file)
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		body := strings.TrimSpace(noteDateRE.ReplaceAllString(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")), ""))
		if body == strings.TrimSpace(text) {
			lines = append(lines[:i:i], lines[i+1:]...)
			if os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600) == nil {
				a.memoryDirty()
				return true
			}
			return false
		}
	}
	return false
}

func (a *app) removeEventByID(id string) bool {
	a.agent.extractMu.Lock()
	defer a.agent.extractMu.Unlock()
	events, err := a.loadMemoryEvents()
	if err != nil {
		return false
	}
	kept := events[:0:0]
	for _, e := range events {
		if e.ID != id {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(events) {
		return false
	}
	a.rewriteMemoryEvents(kept)
	return true
}

func (a *app) handleMemory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, a.memorySnapshot())
	case http.MethodPost:
		var req struct {
			Action string `json:"action"`
			ID     string `json:"id"`
			File   string `json:"file"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ok, message := true, ""
		switch req.Action {
		case "forget_note":
			ok = a.removeNoteExact(req.File, req.Text)
		case "forget_event":
			ok = a.removeEventByID(req.ID)
		case "pin":
			a.setMemoryPinned("event:"+req.ID, true)
		case "unpin":
			a.setMemoryPinned("event:"+req.ID, false)
		case "consolidate":
			message = a.consolidateMemory().String()
		case "dismiss_notice":
			a.dismissNotice(req.ID)
		case "skill_approve":
			if err := a.approveSkillProposal(req.ID); err != nil {
				ok, message = false, err.Error()
			}
		case "skill_reject":
			ok = a.rejectSkillProposal(req.ID) == nil
		case "dream":
			report, err := a.dreamOnce(r.Context())
			message = report.String()
			if err != nil {
				ok, message = false, "I could not reflect just now: "+err.Error()
			}
		case "remove_job":
			_, ok = a.removeCronJob(req.ID)
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": ok, "message": message})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
