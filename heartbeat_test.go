package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeartbeatSeedIsInertAndTasksAreRead(t *testing.T) {
	a := newTestApp(t)
	a.ensureHeartbeatFile()
	if tasks := a.heartbeatTasks(); len(tasks) != 0 {
		t.Fatalf("the seed file must not enable the heartbeat: %v", tasks)
	}
	model := useFakeModel(t, textChunk(heartbeatOK))
	if a.runHeartbeatOnce(context.Background()) || len(model.requests) != 0 {
		t.Errorf("an empty checklist must not call the model")
	}
	writeWorkspaceFile(t, a, heartbeatFile, heartbeatSeed+"- Check whether the router is mentioned in my notes\n- Tell me if it will rain\n")
	if tasks := a.heartbeatTasks(); len(tasks) != 2 {
		t.Errorf("tasks = %v", tasks)
	}
}

func TestHeartbeatIsSilentWhenOKAndNoticesWhenNot(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, heartbeatFile, "- Check the dentist date\n")
	model := useFakeModel(t, textChunk(heartbeatOK), textChunk("Your dentist appointment is tomorrow at 9."), textChunk(`{"needs_attention":true,"message":"Your dentist appointment is tomorrow at 9."}`))
	if !a.runHeartbeatOnce(context.Background()) {
		t.Fatal("heartbeat did not run")
	}
	if n := a.pendingNotices(); len(n) != 0 {
		t.Errorf("HEARTBEAT_OK must stay silent: %v", n)
	}
	if !a.runHeartbeatOnce(context.Background()) {
		t.Fatal("second heartbeat did not run")
	}
	notices := a.pendingNotices()
	if len(notices) != 1 || !strings.Contains(notices[0].Text, "dentist") {
		t.Errorf("notices = %v", notices)
	}
	// no tools beyond the lookup set, and no conversation left behind
	for _, tool := range model.requests[0]["tools"].([]any) {
		name := tool.(map[string]any)["function"].(map[string]any)["name"].(string)
		if !allowSet(unattendedTools)[name] {
			t.Errorf("heartbeat was offered %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(a.sessionsDir(), "heartbeat.json")); err == nil {
		t.Errorf("the heartbeat session was kept")
	}
}

func TestHeartbeatSkipsWhileBusy(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, heartbeatFile, "- Check something\n")
	model := useFakeModel(t, textChunk(heartbeatOK))
	if !a.beginAgentWork() {
		t.Skip("could not mark the agent busy")
	}
	defer a.endAgentWork()
	if a.runHeartbeatOnce(context.Background()) || len(model.requests) != 0 {
		t.Errorf("the heartbeat must not run while the user is being served")
	}
}

func TestHeartbeatAsksForAVerdictInsteadOfGuessing(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, heartbeatFile, "- Check tomorrow\n")
	model := useFakeModel(t,
		// 1: rambles, contradicts itself, verdict says it matters
		textChunk("Your notes show a dentist appointment tomorrow at 9am. That is the only item. "+heartbeatOK),
		textChunk(`{"needs_attention":true,"message":"You have a dentist appointment tomorrow at 9am. Bring your card."}`),
		// 2: says nothing is due; verdict agrees
		textChunk("Nothing is due tomorrow according to your notes."),
		textChunk(`{"needs_attention":false,"message":""}`),
		// 3: a failed check never reaches the verdict call
		textChunk("I went around in circles on that without getting an answer."),
		// 4: verdict claims attention with no message: ignored
		textChunk("Something odd happened."),
		textChunk(`{"needs_attention":true,"message":""}`),
	)
	for i := 0; i < 4; i++ {
		a.runHeartbeatOnce(context.Background())
	}
	notices := a.pendingNotices()
	if len(notices) != 1 || notices[0].Text != "Heartbeat: You have a dentist appointment tomorrow at 9am. Bring your card." {
		t.Fatalf("notices = %v", notices)
	}
	if len(model.requests) != 7 {
		t.Errorf("model calls = %d, want 7 (a failed check skips the verdict)", len(model.requests))
	}
	if model.requests[1]["response_format"] == nil || model.requests[1]["tools"] != nil {
		t.Errorf("the verdict call must be schema-constrained and tool-free: %v", model.requests[1])
	}
}

func TestHeartbeatIntervalFromFile(t *testing.T) {
	a := newTestApp(t)
	for text, want := range map[string]time.Duration{
		"every: 15 minutes\n- x\n": 15 * time.Minute,
		"Every: 2 hours\n- x\n":    2 * time.Hour,
		"every: 1 minute\n- x\n":   heartbeatEvery, // too often
		"every: 99 hours\n- x\n":   heartbeatEvery, // too rarely
		"- no setting here\n":      heartbeatEvery,
	} {
		writeWorkspaceFile(t, a, heartbeatFile, text)
		if got := a.heartbeatInterval(); got != want {
			t.Errorf("%q: interval = %v, want %v", text, got, want)
		}
	}
	writeWorkspaceFile(t, a, heartbeatFile, "every: 15 minutes\n")
	if tasks := a.heartbeatTasks(); len(tasks) != 0 {
		t.Errorf("the setting line must not count as a task: %v", tasks)
	}
}
