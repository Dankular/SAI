package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var cronNow = time.Date(2026, 10, 16, 14, 0, 0, 0, time.Local) // a Friday

func TestParseWhen(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"in 10 minutes", cronNow.Add(10 * time.Minute)},
		{"in an hour", cronNow.Add(time.Hour)},
		{"in half an hour", cronNow.Add(30 * time.Minute)},
		{"in 2 days", cronNow.Add(48 * time.Hour)},
		{"tomorrow at 9am", time.Date(2026, 10, 17, 9, 0, 0, 0, time.Local)},
		{"tomorrow", time.Date(2026, 10, 17, 9, 0, 0, 0, time.Local)},
		{"tonight", time.Date(2026, 10, 16, 20, 0, 0, 0, time.Local)},
		{"today at 5pm", time.Date(2026, 10, 16, 17, 0, 0, 0, time.Local)},
		{"at 9", time.Date(2026, 10, 17, 9, 0, 0, 0, time.Local)}, // 9 has passed today
		{"at 15:30", time.Date(2026, 10, 16, 15, 30, 0, 0, time.Local)},
		{"monday at 8", time.Date(2026, 10, 19, 8, 0, 0, 0, time.Local)},
		{"friday at 3pm", time.Date(2026, 10, 16, 15, 0, 0, 0, time.Local)},
		{"friday at 9am", time.Date(2026, 10, 23, 9, 0, 0, 0, time.Local)},
		{"2026-11-03 14:30", time.Date(2026, 11, 3, 14, 30, 0, 0, time.Local)},
	}
	for _, c := range cases {
		got, err := parseWhen(c.in, cronNow)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("parseWhen(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "someday", "yesterday at 5", "2020-01-01 10:00", "in 0 minutes"} {
		if got, err := parseWhen(bad, cronNow); err == nil {
			t.Errorf("parseWhen(%q) = %v, want an error", bad, got)
		}
	}
}

func TestParseEvery(t *testing.T) {
	every, daily, weekday, first, err := parseEvery("every 2 hours", cronNow)
	if err != nil || every != 2*time.Hour || daily != "" || !first.Equal(cronNow.Add(2*time.Hour)) {
		t.Errorf("every 2 hours = %v %q %q %v %v", every, daily, weekday, first, err)
	}
	every, _, _, _, err = parseEvery("hourly", cronNow)
	if err != nil || every != time.Hour {
		t.Errorf("hourly = %v %v", every, err)
	}
	_, daily, _, first, err = parseEvery("daily at 8am", cronNow)
	if err != nil || daily != "08:00" || !first.Equal(time.Date(2026, 10, 17, 8, 0, 0, 0, time.Local)) {
		t.Errorf("daily at 8am = %q %v %v", daily, first, err)
	}
	_, daily, weekday, first, err = parseEvery("every monday at 9", cronNow)
	if err != nil || daily != "09:00" || weekday != "monday" || !first.Equal(time.Date(2026, 10, 19, 9, 0, 0, 0, time.Local)) {
		t.Errorf("every monday at 9 = %q %q %v %v", daily, weekday, first, err)
	}
	for _, bad := range []string{"every 10 seconds", "sometimes", "every"} {
		if _, _, _, _, err := parseEvery(bad, cronNow); err == nil {
			t.Errorf("parseEvery(%q) should fail", bad)
		}
	}
}

func TestReminderFiresOnceAndPersists(t *testing.T) {
	a := newTestApp(t)
	job, err := a.addCronJob("reminder", "call the dentist", "in 1 minute", "")
	if err != nil {
		t.Fatal(err)
	}
	if due := a.dueJobs(time.Now()); len(due) != 0 {
		t.Fatalf("not due yet, got %v", due)
	}
	later := time.Now().Add(2 * time.Minute)
	due := a.dueJobs(later)
	if len(due) != 1 || due[0].ID != job.ID {
		t.Fatalf("due = %v", due)
	}
	a.fireJob(context.Background(), due[0])
	if jobs := a.listCronJobs(); len(jobs) != 0 {
		t.Errorf("a one-off reminder should be gone, have %v", jobs)
	}
	// a fresh process reads the notice back from disk
	again := newTestApp(t)
	again.dataDir = a.dataDir
	notices := again.pendingNotices()
	if len(notices) != 1 || !strings.Contains(notices[0].Text, "call the dentist") {
		t.Fatalf("notices = %v", notices)
	}
	again.dismissNotice(notices[0].ID)
	if len(again.pendingNotices()) != 0 {
		t.Errorf("notice was not dismissed")
	}
}

func TestRepeatingJobAdvancesWithoutCatchUp(t *testing.T) {
	a := newTestApp(t)
	if _, err := a.addCronJob("reminder", "stretch", "", "every 30 minutes"); err != nil {
		t.Fatal(err)
	}
	// the app was off for five hours: it fires once, not ten times
	after := time.Now().Add(5 * time.Hour)
	if due := a.dueJobs(after); len(due) != 1 {
		t.Fatalf("due = %d, want 1", len(due))
	}
	jobs := a.listCronJobs()
	next, _ := time.Parse(time.RFC3339, jobs[0].Next)
	if len(jobs) != 1 || !next.After(after) || next.Sub(after) > 30*time.Minute {
		t.Errorf("next run %v is not just after %v", next, after)
	}
}

func TestScheduleToolAndRemove(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	out, err := toolSchedule(ctx, a, map[string]any{"action": "add", "text": "water the plants", "every": "daily at 8am"})
	if err != nil || !strings.Contains(out, "water the plants") {
		t.Fatalf("add = %q, %v", out, err)
	}
	if _, err := toolSchedule(ctx, a, map[string]any{"action": "add", "text": "x", "when": "whenever"}); err == nil {
		t.Errorf("an unreadable time should be an error")
	}
	if list, _ := toolSchedule(ctx, a, map[string]any{"action": "list"}); !strings.Contains(list, "every day at 08:00") {
		t.Errorf("list = %q", list)
	}
	if _, err := toolSchedule(ctx, a, map[string]any{"action": "remove", "id": "plants"}); err != nil {
		t.Errorf("remove by words: %v", err)
	}
	if list, _ := toolSchedule(ctx, a, map[string]any{"action": "list"}); !strings.Contains(list, "Nothing") {
		t.Errorf("list after remove = %q", list)
	}
}

func TestHarnessSchedulesReminderWithoutTheModel(t *testing.T) {
	a := newTestApp(t)
	for _, say := range []string{"Remind me to call mum tomorrow at 9am", "remind me in 20 minutes to check the oven", "Remind me to take a break every 2 hours"} {
		plan := a.harnessPlan(say, time.Now())
		if !strings.HasPrefix(plan.Direct, "Okay, I'll remind you") {
			t.Errorf("%q: direct = %q", say, plan.Direct)
		}
	}
	if jobs := a.listCronJobs(); len(jobs) != 3 {
		t.Errorf("jobs = %v", jobs)
	}
	if plan := a.harnessPlan("remind me to do something at some point", time.Now()); strings.HasPrefix(plan.Direct, "Okay") {
		t.Errorf("no time was given, nothing should be scheduled: %q", plan.Direct)
	}
}

func TestUnattendedRunRefusesGuardedTools(t *testing.T) {
	a := newTestApp(t)
	tools := []agentTool{{
		Name:  "danger",
		Guard: func(a *app, args map[string]any) string { return "do a thing" },
		Run: func(ctx context.Context, a *app, args map[string]any) (string, error) {
			t.Errorf("a guarded tool ran with nobody to approve it")
			return "", nil
		},
	}}
	ctx := context.WithValue(context.Background(), unattendedKey{}, true)
	out := a.execTool(ctx, tools, toolCall{Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "danger", Arguments: "{}"}}, func(agentEvent) {})
	if !strings.Contains(out, "no one is here") {
		t.Errorf("out = %q", out)
	}
}

func TestMemoryAPI(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, "USER.md", "- Likes tea (2026-02-02)\n")
	writeEvents(t, a, memoryEventRecord{ID: "evt_p", Event: "preference_expressed", Summary: "Prefers tea", Preference: "tea", Scope: "user", Status: "explicit", CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.handleMemory(rec, httptest.NewRequest(http.MethodPost, "/api/memory", strings.NewReader(body)))
		return rec
	}
	snap := a.memorySnapshot()
	if len(snap.Notes) != 1 || snap.Notes[0].Text != "Likes tea" || snap.Notes[0].Date != "2026-02-02" || len(snap.Events) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	post(`{"action":"pin","id":"evt_p"}`)
	if !a.memorySnapshot().Events[0].Pinned {
		t.Errorf("pin not shown")
	}
	if rec := post(`{"action":"forget_note","file":"USER.md","text":"Likes tea"}`); !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("forget_note: %s", rec.Body.String())
	}
	if rec := post(`{"action":"forget_note","file":"../secret.txt","text":"x"}`); !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Errorf("a path outside the notes must be refused: %s", rec.Body.String())
	}
	post(`{"action":"forget_event","id":"evt_p"}`)
	if snap := a.memorySnapshot(); len(snap.Notes) != 0 || len(snap.Events) != 0 {
		t.Errorf("after forgetting: %+v", snap)
	}
	if rec := post(`{"action":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown action code = %d", rec.Code)
	}
}

func TestAgentCallsScheduleAndMemoryTools(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, "MEMORY.md", "- Gym day is Tuesday (2026-01-01)\n")
	model := useFakeModel(t,
		toolChunks("c1", "schedule", `{"action":"add","text":"stretch","every":"every 2 hours"}`),
		toolChunks("c2", "memory_edit", `{"action":"update","about":"gym day","text":"Gym day is Thursday"}`),
		textChunk("Updated."),
	)
	var events []agentEvent
	if err := a.runTurn(context.Background(), turnOptions{Session: "s", Text: "set up a schedule for stretching"}, collect(&events)); err != nil {
		t.Fatal(err)
	}
	if jobs := a.listCronJobs(); len(jobs) != 1 || jobs[0].EverySec != 7200 {
		t.Fatalf("jobs = %+v", jobs)
	}
	// memory_edit update asks for approval; refuse nothing, approve it.
	done := make(chan error, 1)
	go func() {
		done <- a.runTurn(context.Background(), turnOptions{Session: "s", Text: "please correct my gym day"}, func(e agentEvent) {
			if e.Approval != nil {
				a.resolveApproval(e.Approval.ID, true)
			}
		})
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(a.workspace, "MEMORY.md"))
	if !strings.Contains(string(data), "Thursday") || strings.Contains(string(data), "Tuesday") {
		t.Errorf("MEMORY.md = %q", data)
	}
	first := model.requests[0]["tools"].([]any)
	var offered []string
	for _, tool := range first {
		offered = append(offered, tool.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	if !strings.Contains(strings.Join(offered, ","), "schedule") {
		t.Errorf("schedule was not offered: %v", offered)
	}
}

func TestHarnessSchedulesRecurringTaskAndListsIt(t *testing.T) {
	a := newTestApp(t)
	plan := a.harnessPlan("Set up a daily 8am task to check the weather in London", time.Now())
	if !strings.HasPrefix(plan.Direct, "Done.") {
		t.Fatalf("direct = %q", plan.Direct)
	}
	jobs := a.listCronJobs()
	if len(jobs) != 1 || jobs[0].Kind != "task" || jobs[0].DailyAt != "08:00" || jobs[0].Text != "check the weather in London" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if plan := a.harnessPlan("What reminders do I have scheduled?", time.Now()); !strings.Contains(plan.Direct, "check the weather") {
		t.Errorf("list = %q", plan.Direct)
	}
}

func TestScheduledTaskGetsOnlyAllowedTools(t *testing.T) {
	a := newTestApp(t)
	model := useFakeModel(t, textChunk("Done."))
	job, err := a.addCronJob("task", "write a poem", "in 1 minute", "")
	if err != nil {
		t.Fatal(err)
	}
	a.fireJob(context.Background(), job)
	var offered []string
	for _, tool := range model.requests[0]["tools"].([]any) {
		offered = append(offered, tool.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	joined := strings.Join(offered, ",")
	for _, banned := range []string{"shell", "write_file", "extension", "schedule", "memory_edit", "apply_patch", "process"} {
		if strings.Contains(joined, banned) {
			t.Errorf("a scheduled task was offered %s: %s", banned, joined)
		}
	}
	allowed := allowSet(unattendedTools)
	for _, name := range offered {
		if !allowed[name] {
			t.Errorf("%s is not an unattended tool", name)
		}
	}
}

func TestTaskToolListValidatedAndStepsCapped(t *testing.T) {
	a := newTestApp(t)
	if _, err := toolSchedule(context.Background(), a, map[string]any{"action": "add", "kind": "task", "text": "x", "every": "hourly", "tools": "shell, bogus"}); err == nil {
		t.Errorf("an unknown tool should be refused")
	}
	if _, err := toolSchedule(context.Background(), a, map[string]any{"action": "add", "kind": "task", "text": "x", "every": "hourly", "tools": "schedule"}); err == nil {
		t.Errorf("a task must not be able to schedule more tasks")
	}
	if _, err := toolSchedule(context.Background(), a, map[string]any{"action": "add", "kind": "task", "text": "check news", "every": "hourly", "tools": "web_search, calculator", "max_steps": float64(3)}); err != nil {
		t.Fatal(err)
	}
	job := a.listCronJobs()[0]
	if len(job.Tools) != 2 || jobSteps(job) != 3 || !jobTools(job)["calculator"] || jobTools(job)["shell"] {
		t.Errorf("job = %+v", job)
	}
	if jobSteps(cronJob{}) != defaultUnattendedSteps || jobSteps(cronJob{MaxSteps: 999}) != maxAgentSteps {
		t.Errorf("step defaults/caps are wrong")
	}
	// a looping model is stopped at the cap
	model := useFakeModel(t, toolChunks("a", "calculator", `{"expression":"1+1"}`), toolChunks("b", "calculator", `{"expression":"1+1"}`), toolChunks("c", "calculator", `{"expression":"1+1"}`), textChunk("never"))
	a.fireJob(context.Background(), cronJob{ID: "j9", Kind: "task", Text: "loop", MaxSteps: 2})
	if len(model.requests) != 2 {
		t.Errorf("model called %d times, want the cap of 2", len(model.requests))
	}
}
