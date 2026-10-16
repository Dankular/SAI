package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScheduleFromSentenceVariants(t *testing.T) {
	cases := []struct{ say, kind, text, daily string }{
		{"Check the weather in London every morning at 7", "task", "Check the weather in London", "07:00"},
		{"can you tell me the news daily at 6pm", "task", "tell me the news", "18:00"},
		{"Every evening, summarise my notes", "task", "summarise my notes", "18:00"},
		{"please remind me to take vitamins every day at 8am", "reminder", "take vitamins", "08:00"},
		{"Schedule a backup of my notes every friday at 5pm", "task", "backup of my notes", "17:00"},
		{"each morning check my reminders", "task", "check my reminders", "09:00"},
	}
	for _, c := range cases {
		a := newTestApp(t)
		plan := a.harnessPlan(c.say, time.Now())
		jobs := a.listCronJobs()
		if len(jobs) != 1 {
			t.Errorf("%q: direct=%q jobs=%v", c.say, plan.Direct, jobs)
			continue
		}
		if jobs[0].Kind != c.kind || jobs[0].DailyAt != c.daily || !strings.EqualFold(jobs[0].Text, c.text) {
			t.Errorf("%q: got %+v, want kind=%s text=%q daily=%s", c.say, jobs[0], c.kind, c.text, c.daily)
		}
	}
	every := newTestApp(t)
	every.harnessPlan("run a disk check every 3 hours", time.Now())
	if jobs := every.listCronJobs(); len(jobs) != 1 || jobs[0].EverySec != 3*3600 {
		t.Errorf("every 3 hours: %+v", jobs)
	}
}

func TestScheduleFromSentenceIgnoresFactsAndQuestions(t *testing.T) {
	for _, say := range []string{
		"I go to the gym every day",
		"What is the weather like every morning in Spain?",
		"My dentist calls every week",
		"Tell me a story",
	} {
		a := newTestApp(t)
		a.harnessPlan(say, time.Now())
		if jobs := a.listCronJobs(); len(jobs) != 0 {
			t.Errorf("%q should not schedule anything: %+v", say, jobs)
		}
	}
}

func TestCorrectionsReplaceTheOldFact(t *testing.T) {
	for _, say := range []string{
		"Actually my gym day is Thursday now, please correct that",
		"No, my gym day is Thursday",
		"change my gym day to Thursday",
	} {
		a := newTestApp(t)
		writeWorkspaceFile(t, a, "USER.md", "- Gym day is Tuesday (2026-01-01)\n- Likes tea (2026-01-02)\n")
		plan := a.harnessPlan(say, time.Now())
		data, _ := os.ReadFile(filepath.Join(a.workspace, "USER.md"))
		text := string(data)
		if !strings.Contains(text, "Thursday") || strings.Contains(text, "Tuesday") || !strings.Contains(text, "Likes tea") {
			t.Errorf("%q: direct=%q file=%q", say, plan.Direct, text)
		}
	}
	a := newTestApp(t)
	if plan := a.harnessPlan("No, I don't think so", time.Now()); strings.HasPrefix(plan.Direct, "Corrected") || strings.HasPrefix(plan.Direct, "Noted") {
		t.Errorf("a plain disagreement is not a fact: %q", plan.Direct)
	}
}

func TestTerminalToolEndsTheTurn(t *testing.T) {
	a := newTestApp(t)
	model := useFakeModel(t,
		toolChunks("c1", "schedule", `{"action":"list"}`),
		textChunk("SHOULD NOT BE ASKED"),
	)
	var events []agentEvent
	if err := a.runTurn(t.Context(), turnOptions{Session: "term", Text: "tell me about my alarms"}, collect(&events)); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Errorf("model called %d times, want 1", len(model.requests))
	}
	if got := replyText(events); !strings.Contains(got, "Nothing is scheduled") || strings.Contains(got, "SHOULD NOT") {
		t.Errorf("reply = %q", got)
	}
}

func TestCorrectionFindsTheFileAndStripsFiller(t *testing.T) {
	a := newTestApp(t)
	writeWorkspaceFile(t, a, "MEMORY.md", "- Gym day is Tuesday (2026-01-01)\n")
	plan := a.harnessPlan("Update my gym day to Monday, thanks", time.Now())
	memory, _ := os.ReadFile(filepath.Join(a.workspace, "MEMORY.md"))
	if _, err := os.Stat(filepath.Join(a.workspace, "USER.md")); err == nil {
		t.Errorf("a duplicate went into USER.md")
	}
	if !strings.Contains(string(memory), "Gym day is Monday (") || strings.Contains(string(memory), "thanks") || strings.Contains(string(memory), "Tuesday") {
		t.Errorf("direct=%q memory=%q", plan.Direct, memory)
	}
	b := newTestApp(t)
	writeWorkspaceFile(t, b, "MEMORY.md", "- Gym day is Tuesday (2026-01-01)\n")
	b.harnessPlan("Actually my gym day is Thursday now, please correct that", time.Now())
	memory, _ = os.ReadFile(filepath.Join(b.workspace, "MEMORY.md"))
	if strings.Contains(string(memory), "now") || !strings.Contains(string(memory), "Thursday") {
		t.Errorf("memory = %q", memory)
	}
}
