package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dreamEvent(id, preference string) memoryEventRecord {
	return memoryEventRecord{ID: id, Event: "preference_expressed", Summary: "Prefers " + preference, Preference: preference, Scope: "user", Status: "explicit",
		CreatedAt: time.Now().UTC().Format(time.RFC3339), Evidence: []memoryEventEvidence{{SourceID: "session:s:message:0", Role: "user", Quote: "I prefer " + preference}}}
}

func TestDreamAppliesOnlyGroundedProposals(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, "USER.md", "- Likes strong black coffee (2026-01-01)\n- Enjoys very strong black coffee (2026-01-02)\n- Works at Initech (2026-01-03)\n")
	writeEvents(t, a, dreamEvent("evt_metric", "metric units"))
	reply := `{"add":[` +
		`{"file":"user","text":"Prefers metric units","event_id":"evt_metric"},` +
		`{"file":"user","text":"Owns a yacht in Monaco","event_id":"evt_metric"},` +
		`{"file":"user","text":"Prefers metric units everywhere","event_id":"evt_nope"}],` +
		`"merge":[` +
		`{"remove":["Likes strong black coffee","Enjoys very strong black coffee"],"into":"Likes very strong black coffee"},` +
		`{"remove":["Works at Initech","Likes strong black coffee"],"into":"Is the CEO of Globex and owns a boat"},` +
		`{"remove":["Does not exist","Works at Initech"],"into":"Works at Initech"}]}`
	useFakeModel(t, textChunk(reply))
	report, err := a.dreamOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(a.workspace, "USER.md"))
	text := string(data)
	if len(report.Added) != 1 || !strings.Contains(text, "Prefers metric units") {
		t.Errorf("added = %v\n%s", report.Added, text)
	}
	for _, bad := range []string{"yacht", "everywhere", "Globex", "boat"} {
		if strings.Contains(text, bad) {
			t.Errorf("an ungrounded note got in (%s):\n%s", bad, text)
		}
	}
	if len(report.Merged) != 1 || !strings.Contains(text, "Likes very strong black coffee") || strings.Contains(text, "Enjoys very strong") {
		t.Errorf("merged = %v\n%s", report.Merged, text)
	}
	if !strings.Contains(text, "Works at Initech") {
		t.Errorf("a note was lost:\n%s", text)
	}
	if log, _ := os.ReadFile(filepath.Join(a.workspace, "memory", "dreams.md")); !strings.Contains(string(log), "Added: Prefers metric units") {
		t.Errorf("dream log = %q", log)
	}
	backups, _ := filepath.Glob(filepath.Join(a.dataDir, "brain", "backups", "USER.md.*"))
	if len(backups) == 0 {
		t.Errorf("no backup was taken before the notes changed")
	}
}

func TestDreamRunsOncePerDayAndSkipsWhenNothingNew(t *testing.T) {
	a := newTestApp(t)
	model := useFakeModel(t, textChunk(`{"add":[],"merge":[]}`))
	if report, err := a.dreamOnce(context.Background()); err != nil || report.Asked || len(model.requests) != 0 {
		t.Errorf("with nothing to reflect on the model must not be called: %+v %v", report, err)
	}
	writeEvents(t, a, dreamEvent("evt_1", "tea"))
	a.maybeDream(context.Background())
	if len(model.requests) != 1 {
		t.Fatalf("the first dream should run: %d calls", len(model.requests))
	}
	a.maybeDream(context.Background())
	if len(model.requests) != 1 {
		t.Errorf("a second dream within a day must not run: %d calls", len(model.requests))
	}
	if time.Since(a.lastDream()) > time.Minute {
		t.Errorf("last dream time was not recorded")
	}
}

func TestDreamSurvivesGarbage(t *testing.T) {
	a := newTestApp(t)
	writeEvents(t, a, dreamEvent("evt_1", "tea"))
	writeWorkspaceFile(t, a, "USER.md", "- Likes tea (2026-01-01)\n")
	useFakeModel(t, textChunk("sorry, I cannot do that"))
	if report, err := a.dreamOnce(context.Background()); err != nil || report.changed() {
		t.Errorf("garbage must change nothing: %+v %v", report, err)
	}
}

func TestDuplicateHintsReachTheModel(t *testing.T) {
	pairs := likelyDuplicates([]string{"Likes strong black coffee", "Enjoys very strong black coffee", "Works at Initech"})
	if len(pairs) != 1 || !strings.Contains(pairs[0], "Enjoys very strong black coffee") {
		t.Fatalf("pairs = %v", pairs)
	}
	a := newTestApp(t)
	writeWorkspaceFile(t, a, "USER.md", "- Likes strong black coffee (2026-01-01)\n- Enjoys very strong black coffee (2026-01-02)\n- Works at Initech (2026-01-03)\n- Lives in Leeds (2026-01-04)\n- Has a dog called Biscuit (2026-01-05)\n- Prefers tea in the evening (2026-01-06)\n")
	model := useFakeModel(t, textChunk(`{"add":[],"merge":[{"remove":["Likes strong black coffee","Enjoys very strong black coffee"],"into":"Likes very strong black coffee"}]}`))
	report, err := a.dreamOnce(context.Background())
	if err != nil || len(report.Merged) != 1 {
		t.Fatalf("report = %+v, %v", report, err)
	}
	sent, _ := model.requests[0]["messages"].([]any)
	user := sent[len(sent)-1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "may say the same thing") {
		t.Errorf("the hint was not sent: %s", user)
	}
}
