package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

type agentWorkStatus struct {
	State       string `json:"state"` // idle | working | awaiting_approval
	Busy        bool   `json:"busy"`
	Detail      string `json:"detail,omitempty"`
	ActiveTurns int    `json:"activeTurns"`
}

type apiStateSnapshot struct {
	Snapshot
	Agent    agentStatus     `json:"agent"`
	Services []serviceStatus `json:"services"`
	Runtime  runtimeStatus   `json:"runtime"`
	Orb      orbSnapshot     `json:"orb"`
	Work     agentWorkStatus `json:"work"`
	Notices  []notice        `json:"notices,omitempty"`
	Revision uint64          `json:"revision,omitempty"`
}

type stateEvent struct {
	Revision uint64           `json:"revision"`
	Snapshot apiStateSnapshot `json:"snapshot"`
}

type stateEventHub struct {
	mu       sync.Mutex
	revision uint64
	key      []byte
	latest   []byte
	subs     map[chan []byte]struct{}
}

func (h *stateEventHub) publish(key []byte, snapshot apiStateSnapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if bytes.Equal(h.key, key) && h.latest != nil {
		return
	}
	if h.subs == nil {
		h.subs = make(map[chan []byte]struct{})
	}
	h.revision++
	snapshot.Revision = h.revision
	encoded, err := json.Marshal(stateEvent{Revision: h.revision, Snapshot: snapshot})
	if err != nil {
		return
	}
	h.key = append(h.key[:0], key...)
	h.latest = encoded
	for subscriber := range h.subs {
		select {
		case subscriber <- encoded:
		default:
			select {
			case <-subscriber:
			default:
			}
			subscriber <- encoded
		}
	}
}

func (h *stateEventHub) subscribe() ([]byte, <-chan []byte, func()) {
	h.mu.Lock()
	if h.subs == nil {
		h.subs = make(map[chan []byte]struct{})
	}
	subscriber := make(chan []byte, 1)
	h.subs[subscriber] = struct{}{}
	initial := append([]byte(nil), h.latest...)
	h.mu.Unlock()
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, subscriber)
			h.mu.Unlock()
		})
	}
	return initial, subscriber, unsubscribe
}

func (a *app) currentWorkStatus() agentWorkStatus {
	a.workMu.Lock()
	defer a.workMu.Unlock()
	return a.work
}

func (a *app) beginAgentWork() bool {
	a.workMu.Lock()
	if a.work.ActiveTurns > 0 {
		a.workMu.Unlock()
		return false
	}
	a.work.ActiveTurns++
	a.work.Busy = true
	a.work.State = "working"
	a.work.Detail = "Thinking"
	a.workMu.Unlock()
	return true
}

func (a *app) updateAgentWork(state, detail string) {
	a.workMu.Lock()
	if a.work.ActiveTurns > 0 {
		if state != "" {
			a.work.State = state
		}
		a.work.Busy = true
		a.work.Detail = detail
	}
	a.workMu.Unlock()
}

func (a *app) endAgentWork() {
	a.workMu.Lock()
	if a.work.ActiveTurns > 0 {
		a.work.ActiveTurns--
	}
	if a.work.ActiveTurns == 0 {
		a.work = agentWorkStatus{State: "idle"}
	} else {
		a.work.Busy = true
	}
	a.workMu.Unlock()
}

func (a *app) buildStateSnapshot() apiStateSnapshot {
	agent := agentStatus{State: "asleep", Label: "Assistant is asleep"}
	var services []serviceStatus
	if a.sup != nil {
		agent = a.sup.Agent()
		services = a.sup.Status()
	}
	work := a.currentWorkStatus()
	inferenceBusy := a.inferenceBusy()
	state := "active"
	if !agent.Wanted {
		state = "idle"
	} else if !agent.Awake || work.Busy || inferenceBusy {
		state = "working"
	}
	waking := agent.Wanted && !agent.Awake
	a.orbShown.set(state)
	a.orbShown.setWaking(waking)
	orb := a.orbShown.snapshot()
	orb.State, orb.Waking = state, waking

	a.mu.Lock()
	current := a.state
	current.RecentLogs = append([]string(nil), a.state.RecentLogs...)
	current.StageLabels = append([]string(nil), a.state.StageLabels...)
	current.GatewayRunning = agent.Wanted
	snapshot := apiStateSnapshot{
		Snapshot: current,
		Agent:    agent,
		Services: services,
		Runtime: runtimeStatus{
			PID:             os.Getpid(),
			Executable:      executablePath(),
			DataDir:         a.dataDir,
			RuntimeDir:      a.runtimeDir(),
			LlamaBackend:    a.llamaBackendName(),
			AudioBackend:    a.audioBackendName(),
			AudioReady:      a.audioReady(),
			InferenceBusy:   inferenceBusy,
			AgentReady:      agent.Awake,
			GatewayWarm:     a.agentWarm.Load(),
			WhistleReady:    a.whistleReady(),
			StateSavedAtUTC: time.Now().UTC().Format(time.RFC3339),
		},
		Orb:     orb,
		Work:    work,
		Notices: a.pendingNotices(),
	}
	a.mu.Unlock()
	return snapshot
}

func (a *app) stateChangeKey(snapshot apiStateSnapshot) []byte {
	snapshot.Revision = 0
	snapshot.Runtime.StateSavedAtUTC = ""
	snapshot.Orb.AgeMs = -1
	snapshot.Orb.ChangedAgeMs = -1
	snapshot.Orb.LevelAgeMs = -1
	snapshot.Orb.StimulusAgeMs = -1
	snapshot.Orb.FlashAgeMs = -1
	snapshot.Orb.Level = 0
	snapshot.Orb.Bass = 0
	snapshot.Orb.Mid = 0
	snapshot.Orb.Treble = 0
	key, _ := json.Marshal(snapshot)
	return key
}

func (a *app) publishStateIfChanged() {
	snapshot := a.buildStateSnapshot()
	a.stateEvents.publish(a.stateChangeKey(snapshot), snapshot)
}

func (a *app) watchStateChanges() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	a.publishStateIfChanged()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.publishStateIfChanged()
		}
	}
}

func (a *app) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is unavailable", http.StatusInternalServerError)
		return
	}
	a.publishStateIfChanged()
	initial, updates, unsubscribe := a.stateEvents.subscribe()
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if len(initial) > 0 {
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", initial); err != nil {
			return
		}
		flusher.Flush()
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-updates:
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (a *app) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snapshot := a.buildStateSnapshot()
	a.stateEvents.mu.Lock()
	snapshot.Revision = a.stateEvents.revision
	a.stateEvents.mu.Unlock()
	writeJSON(w, snapshot)
}
