package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeWorkspaceFile(t *testing.T, a *app, name, text string) {
	t.Helper()
	path := filepath.Join(a.workspace, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeEvents(t *testing.T, a *app, events ...memoryEventRecord) {
	t.Helper()
	var lines []string
	for _, e := range events {
		b, _ := json.Marshal(e)
		lines = append(lines, string(b))
	}
	writeWorkspaceFile(t, a, "memory/events.jsonl", strings.Join(lines, "\n")+"\n")
}

func TestSalienceBoostFadesAndPinsStick(t *testing.T) {
	now := time.Now()
	fresh := memoryUse{Uses: 5, LastUsed: now.Format(time.RFC3339)}
	cold := memoryUse{Uses: 5, LastUsed: now.AddDate(0, 0, -365).Format(time.RFC3339)}
	if salienceBoost(fresh, now) <= salienceBoost(cold, now) {
		t.Errorf("a recently used memory should outrank a cold one")
	}
	if salienceBoost(memoryUse{}, now) != 0 {
		t.Errorf("an unused memory gets no boost")
	}
	if salienceBoost(memoryUse{Pinned: true}, now) < salienceBoost(fresh, now) {
		t.Errorf("a pin should beat plain use")
	}
}

func TestMemoryUseAndPinPersist(t *testing.T) {
	a := newTestApp(t)
	a.noteMemoryUse("event:evt_1", "event:evt_1")
	a.setMemoryPinned("event:evt_2", true)
	again := newTestApp(t)
	again.dataDir = a.dataDir
	if use := again.memoryUseOf("event:evt_1"); use.Uses != 2 {
		t.Errorf("uses = %d, want 2", use.Uses)
	}
	if !again.memoryUseOf("event:evt_2").Pinned {
		t.Errorf("pin was not saved")
	}
	a.setMemoryPinned("event:evt_2", false)
	if a.memoryUseOf("event:evt_2").Pinned {
		t.Errorf("unpin did not clear the pin")
	}
}

func TestConsolidateNotes(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, "MEMORY.md", strings.Join([]string{
		"# Memory",
		"- Dentist appointment is on 4 November (2026-01-01)",
		"- Dentist appointment is on 3 November (2026-02-01)",
		"- Likes strong black coffee in the morning (2026-01-05)",
		"- Likes strong black coffee in the morning (2026-03-05)",
		"- Garage code is a secret (2026-01-09)",
		"",
	}, "\n"))
	report := a.consolidateMemory()
	if report.NotesMerged+report.NotesReplaced != 2 {
		t.Fatalf("report = %+v", report)
	}
	data, _ := os.ReadFile(filepath.Join(a.workspace, "MEMORY.md"))
	text := string(data)
	if strings.Contains(text, "4 November") || strings.Count(text, "coffee") != 1 || !strings.Contains(text, "3 November") || !strings.Contains(text, "Garage code") {
		t.Errorf("unexpected result:\n%s", text)
	}
	backups, _ := filepath.Glob(filepath.Join(a.dataDir, "brain", "backups", "MEMORY.md.*"))
	if len(backups) != 1 {
		t.Errorf("expected one backup, got %d", len(backups))
	}
	if again := a.consolidateMemory(); again.changed() {
		t.Errorf("a second pass should change nothing: %+v", again)
	}
}

func TestConsolidateEventsKeepsReferenced(t *testing.T) {
	a := newTestApp(t)
	old := time.Now().AddDate(0, 0, -10).UTC().Format(time.RFC3339)
	recent := time.Now().UTC().Format(time.RFC3339)
	ev := func(id, created string, related ...string) memoryEventRecord {
		return memoryEventRecord{ID: id, Event: "note_identified", Summary: "The router password lives in the safe", Scope: "user", Status: "explicit", CreatedAt: created, RelatedIDs: related}
	}
	writeEvents(t, a, ev("evt_a", old), ev("evt_b", old), ev("evt_c", recent, "evt_b"))
	if report := a.consolidateMemory(); report.EventsMerged != 1 {
		t.Fatalf("merged = %d, want 1", report.EventsMerged)
	}
	events, _ := a.loadMemoryEvents()
	ids := map[string]bool{}
	for _, e := range events {
		ids[e.ID] = true
	}
	if ids["evt_a"] || !ids["evt_b"] || !ids["evt_c"] {
		t.Errorf("kept = %v", ids)
	}
}

func TestStaleTaskIsDemoted(t *testing.T) {
	old := memoryEventRecord{Event: "task_unfinished", CreatedAt: time.Now().Add(-staleTaskAge - time.Hour).UTC().Format(time.RFC3339)}
	if !staleEvent(old, time.Now()) {
		t.Errorf("an old unfinished task should be stale")
	}
	if staleEvent(memoryEventRecord{CreatedAt: time.Now().UTC().Format(time.RFC3339)}, time.Now()) {
		t.Errorf("a new event is not stale")
	}
}

func TestMemoryEditForgetUpdatePin(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, "USER.md", "- Likes oat milk (2026-01-01)\n- Works at Initech (2026-01-02)\n")
	writeEvents(t, a, memoryEventRecord{ID: "evt_x", Event: "preference_expressed", Summary: "Prefers oat milk in coffee", Preference: "oat milk in coffee", Scope: "user", Status: "explicit", CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	ctx := context.Background()

	out, err := toolMemoryEdit(ctx, a, map[string]any{"action": "forget", "about": "oat milk"})
	if err != nil || !strings.Contains(out, "1 note") || !strings.Contains(out, "1 recorded event") {
		t.Fatalf("forget = %q, %v", out, err)
	}
	if events, _ := a.loadMemoryEvents(); len(events) != 0 {
		t.Errorf("event survived forget")
	}
	if _, err := toolMemoryEdit(ctx, a, map[string]any{"action": "update", "about": "works at Initech", "text": "Works at Globex"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(a.workspace, "MEMORY.md"))
	if !strings.Contains(string(data), "Works at Globex") {
		t.Errorf("update did not save the new fact: %s", data)
	}
	if _, err := toolMemoryEdit(ctx, a, map[string]any{"action": "update", "about": "unknown thing", "text": "x"}); err == nil {
		t.Errorf("update of a missing note should fail")
	}
	if _, err := toolMemoryEdit(ctx, a, map[string]any{"action": "pin", "source": "event:evt_9"}); err != nil || !a.memoryUseOf("event:evt_9").Pinned {
		t.Errorf("pin failed: %v", err)
	}
}

func TestMemoryGetReadsEvent(t *testing.T) {
	a := newTestApp(t)
	writeEvents(t, a, memoryEventRecord{ID: "evt_g", Event: "note_identified", Summary: "Spare key is under the blue pot", Scope: "user", Status: "explicit", CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	out, err := toolMemoryGet(context.Background(), a, map[string]any{"source": "event:evt_g"})
	if err != nil || !strings.Contains(out, "blue pot") {
		t.Fatalf("get = %q, %v", out, err)
	}
	if _, err := toolMemoryGet(context.Background(), a, map[string]any{"source": "event:nope"}); err == nil {
		t.Errorf("a missing event should be an error")
	}
}

func TestProfileBlockUsesExplicitCurrentFacts(t *testing.T) {
	a := newTestApp(t)
	now := time.Now().UTC().Format(time.RFC3339)
	writeEvents(t, a,
		memoryEventRecord{ID: "evt_1", Event: "preference_expressed", Preference: "short answers", Scope: "user", Status: "explicit", CreatedAt: now},
		memoryEventRecord{ID: "evt_2", Event: "preference_expressed", Preference: "long essays", Scope: "user", Status: "inferred", CreatedAt: now},
		memoryEventRecord{ID: "evt_3", Event: "decision_changed", Decision: "use tabs", Scope: "user", Status: "current", CreatedAt: now},
		memoryEventRecord{ID: "evt_4", Event: "decision_changed", Decision: "use spaces", Scope: "user", Status: "current", CreatedAt: now, SupersedesID: "evt_0"},
		memoryEventRecord{ID: "evt_0", Event: "decision_changed", Decision: "use ancient style", Scope: "user", Status: "current", CreatedAt: now},
		memoryEventRecord{ID: "evt_5", Event: "preference_expressed", Preference: "dark theme", Scope: "project:x", Status: "explicit", CreatedAt: now},
	)
	block := a.profileBlock()
	for _, want := range []string{"short answers", "use tabs", "use spaces"} {
		if !strings.Contains(block, want) {
			t.Errorf("profile missing %q:\n%s", want, block)
		}
	}
	for _, bad := range []string{"long essays", "ancient", "dark theme"} {
		if strings.Contains(block, bad) {
			t.Errorf("profile should not contain %q:\n%s", bad, block)
		}
	}
}

func TestToolsForOffersMemoryEdit(t *testing.T) {
	if !toolsFor("please pin that")["memory_edit"] {
		t.Errorf("pin should offer memory_edit")
	}
	if toolsFor("what is the weather")["memory_edit"] {
		t.Errorf("weather should not offer memory_edit")
	}
}

func TestEvidenceQuoteFixesWrongMessageNumber(t *testing.T) {
	a := newTestApp(t)
	observed := []observedMessage{{Index: 0, Role: "user", Content: "I never use miles, always metric please."}, {Index: 1, Role: "assistant", Content: "Understood."}}
	candidate := extractedMemoryEvent{Event: "preference_expressed", Summary: "Wants metric units", Scope: "user"}
	candidate.Evidence = []struct {
		Message int    `json:"message"`
		Quote   string `json:"quote"`
	}{{Message: 1, Quote: "I never use miles"}, {Message: 0, Quote: "a quote that is nowhere"}}
	if err := a.appendExtractedEvents("s", observed, []extractedMemoryEvent{candidate}); err != nil {
		t.Fatal(err)
	}
	events, _ := a.loadMemoryEvents()
	if len(events) != 1 || len(events[0].Evidence) != 1 || events[0].Evidence[0].SourceID != "session:s:message:0" || events[0].Status != "explicit" {
		t.Fatalf("events = %+v", events)
	}
}

func TestCorrectionKeepsTheOldValueAsHistory(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, "MEMORY.md", "- Gym day is Tuesday (2026-01-01)\n")
	a.harnessPlan("Actually my gym day is Thursday now, please correct that", time.Now())
	events, _ := a.loadMemoryEvents()
	if len(events) != 1 || events[0].Event != "fact_changed" || !strings.Contains(events[0].Information, "Tuesday") || !strings.Contains(events[0].Information, "Thursday") {
		t.Fatalf("events = %+v", events)
	}
	// the same via the tool
	if _, err := toolMemoryEdit(context.Background(), a, map[string]any{"action": "update", "about": "gym day", "text": "Gym day is Friday"}); err != nil {
		t.Fatal(err)
	}
	events, _ = a.loadMemoryEvents()
	if len(events) != 2 || !strings.Contains(events[1].Information, "Thursday") {
		t.Fatalf("events after tool update = %+v", events)
	}
	// history is searchable without embeddings
	hits, err := a.searchMemoryFiltered(context.Background(), "gym day Tuesday before", "event", 0, 3)
	if err != nil || len(hits) == 0 || !strings.Contains(hits[0].Text, "Tuesday") {
		t.Errorf("history not found: %v %v", hits, err)
	}
}

func TestPreviousValueQuestion(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, "MEMORY.md", "- Gym day is Tuesday (2026-01-01)\n- Dentist is on 4 May (2026-01-01)\n")
	a.harnessPlan("Actually my gym day is Thursday now", time.Now())
	plan := a.harnessPlan("What was my gym day before?", time.Now())
	if !strings.Contains(plan.Direct, "Tuesday") || !strings.Contains(plan.Direct, "Thursday") {
		t.Errorf("direct = %q", plan.Direct)
	}
	if plan := a.harnessPlan("What was my dentist day before?", time.Now()); strings.Contains(plan.Direct, "Tuesday") {
		t.Errorf("must not answer with an unrelated fact: %q", plan.Direct)
	}
}

func TestOverflowingNotesMoveToSearchableArchive(t *testing.T) {
	a := newTestApp(t)
	var lines []string
	for i := 0; i < 80; i++ {
		lines = append(lines, "- Note number "+strings.Repeat("x", 30)+" item"+string(rune('a'+i%26))+string(rune('a'+i/26))+" (2026-"+twoDigits(1+i/10)+"-"+twoDigits(1+i%28)+")")
	}
	writeWorkspaceFile(t, a, "MEMORY.md", "# Memory\n"+strings.Join(lines, "\n")+"\n")
	before, _ := os.ReadFile(filepath.Join(a.workspace, "MEMORY.md"))
	if len(before) <= 4000 {
		t.Fatalf("fixture too small: %d", len(before))
	}
	report := a.consolidateMemory()
	after, _ := os.ReadFile(filepath.Join(a.workspace, "MEMORY.md"))
	archive, _ := os.ReadFile(filepath.Join(a.workspace, "memory", "archive.md"))
	if report.NotesArchived == 0 || len(after) > 4000 || !strings.Contains(string(after), "# Memory") {
		t.Fatalf("report=%+v size=%d", report, len(after))
	}
	if strings.Count(string(archive), "- Note number") != report.NotesArchived {
		t.Errorf("archive has %d notes, report says %d", strings.Count(string(archive), "- Note number"), report.NotesArchived)
	}
	// the oldest went, the newest stayed
	if strings.Contains(string(after), "(2026-01-01)") || !strings.Contains(string(after), "(2026-08-") {
		t.Errorf("wrong notes archived:\n%s", after)
	}
	if again := a.consolidateMemory(); again.NotesArchived != 0 {
		t.Errorf("second pass archived more: %+v", again)
	}
}

func twoDigits(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func TestSystemPromptIsStableAcrossTurns(t *testing.T) {
	a := newTestApp(t)
	if a.systemPrompt(time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)) != a.systemPrompt(time.Date(2026, 6, 1, 17, 45, 0, 0, time.UTC)) {
		t.Errorf("the system prompt changes with the clock, which defeats the prompt cache")
	}
	v := volatileContext(time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC), "", "\n\nmemories")
	if !strings.Contains(v, "Friday 16 October 2026, 09:30") || !strings.HasSuffix(v, "memories") {
		t.Errorf("volatile = %q", v)
	}
	if strings.Contains(volatileContext(time.Now(), "\n\n[Local time: x]", ""), "Local time") {
		t.Errorf("the clock was added twice")
	}
}

func TestEventLabelsMustBeBackedUp(t *testing.T) {
	a := newTestApp(t)
	obs := []observedMessage{
		{Index: 0, Role: "user", Content: "Also my VPN problem is fixed, it worked after I restarted the router."},
		{Index: 1, Role: "user", Content: "From now on I will use metric units instead of miles."},
		{Index: 2, Role: "user", Content: "I prefer short answers."},
		{Index: 3, Role: "user", Content: "I still need to finish the tax form next week."},
	}
	ev := func(kind, quote string, msg int, fill func(*extractedMemoryEvent)) extractedMemoryEvent {
		c := extractedMemoryEvent{Event: kind, Summary: "summary of " + kind, Scope: "user"}
		c.Evidence = []struct {
			Message int    `json:"message"`
			Quote   string `json:"quote"`
		}{{Message: msg, Quote: quote}}
		if fill != nil {
			fill(&c)
		}
		return c
	}
	candidates := []extractedMemoryEvent{
		ev("decision_changed", "my VPN problem is fixed", 0, nil),                                                   // no decision, user did not decide anything
		ev("task_unfinished", "my VPN problem is fixed", 0, func(c *extractedMemoryEvent) { c.Progress = "fixed" }), // user said it is done
		ev("decision_changed", "I will use metric units instead of miles", 1, func(c *extractedMemoryEvent) { c.Decision = "use metric units" }),
		ev("preference_expressed", "I prefer short answers", 2, nil),
		ev("task_unfinished", "I still need to finish the tax form next week", 3, func(c *extractedMemoryEvent) { c.NextAction = "finish the tax form" }),
	}
	if err := a.appendExtractedEvents("s", obs, candidates); err != nil {
		t.Fatal(err)
	}
	events, _ := a.loadMemoryEvents()
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Event)
	}
	want := "note_identified,note_identified,decision_changed,preference_expressed,task_unfinished"
	if strings.Join(kinds, ",") != want {
		t.Errorf("kinds = %v, want %s", kinds, want)
	}
	if events[3].Preference != "summary of preference_expressed" {
		t.Errorf("a preference with no field should take the summary: %+v", events[3])
	}
}
