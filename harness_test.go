package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var harnessNow = time.Date(2026, 10, 16, 12, 55, 0, 0, time.UTC)

func harnessApp(t *testing.T) *app {
	t.Helper()
	a := newTestApp(t)
	if err := a.writePersona(Profile{UserName: "Dan Khosa", AgentName: "Jarvis", Purpose: "General", ReplyStyle: "balanced"}); err != nil {
		t.Fatal(err)
	}
	a.state.Profile = Profile{UserName: "Dan Khosa", AgentName: "Jarvis"}
	return a
}

func readNotes(t *testing.T, a *app, name string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(a.workspace, name))
	return string(data)
}

func TestHarnessAnswersExactQuestionsWithoutTheModel(t *testing.T) {
	a := harnessApp(t)
	cases := map[string]string{
		"what time is it?":            "It's 12:55 on Friday 16 October.",
		"What's the time":             "It's 12:55 on Friday 16 October.",
		"what day is it":              "Today is Friday 16 October 2026.",
		"what's the date today?":      "Today is Friday 16 October 2026.",
		"what is 17 times 23?":        "17 times 23 is 391.",
		"What's 15 percent of 86.40?": "15 percent of 86.40 is 12.96.",
		"calculate (2 + 3) * 4":       "(2 plus 3) times 4 is 20.",
		"what is 10 divided by 4":     "10 divided by 4 is 2.5.",
		"how much is 1,250 x 2":       "1,250 times 2 is 2500.",
		"2 ^ 10":                      "2 to the power of 10 is 1024.",
	}
	for in, want := range cases {
		plan := a.harnessPlan(in, harnessNow)
		if plan.Direct != want {
			t.Errorf("%q -> %q, want %q", in, plan.Direct, want)
		}
	}
	// Things that only look like maths, or ask for more than maths, go to the model.
	for _, in := range []string{"my dentist is on 3 November at 2pm", "who won in 2019?", "write a poem about 7 swans", "meet at 10 - 11 am", "what time does the train to Leeds leave?"} {
		if plan := a.harnessPlan(in, harnessNow); plan.Direct != "" && !strings.HasPrefix(plan.Direct, "Noted") {
			t.Errorf("%q should not be answered directly, got %q", in, plan.Direct)
		}
	}
}

func TestHarnessGivesTheModelAnExactCalculationWhenMathIsPartOfALongerQuestion(t *testing.T) {
	a := harnessApp(t)
	plan := a.harnessPlan("I bought 3 items at 4.50 * 3 each, is that a good price for a toaster?", harnessNow)
	if plan.Direct != "" {
		t.Fatalf("a longer question needs the model, got %q", plan.Direct)
	}
	if len(plan.Notes) == 0 || !strings.Contains(plan.Notes[0], "13.5") {
		t.Errorf("the exact result should be handed over: %v", plan.Notes)
	}
}

func TestHarnessRemembersUpdatesAndForgetsWithoutTheModel(t *testing.T) {
	a := harnessApp(t)
	plan := a.harnessPlan("Please remember that my dentist appointment is on 3 November at 2pm.", harnessNow)
	if !plan.Wrote || !strings.Contains(plan.Direct, "Dentist appointment is on 3 November at 2pm") {
		t.Fatalf("plan = %+v", plan)
	}
	user := readNotes(t, a, "USER.md")
	if !strings.Contains(user, "- Dentist appointment is on 3 November at 2pm (") {
		t.Fatalf("not saved as a note about the user:\n%s", user)
	}
	// the same thing again changes nothing
	if again := a.harnessPlan("remember that my dentist appointment is on 3 November at 2pm", harnessNow); again.Wrote || !strings.Contains(again.Direct, "already") {
		t.Errorf("a repeat should be recognised: %+v", again)
	}
	// a correction replaces the old line rather than adding a second one
	upd := a.harnessPlan("remember that my dentist appointment is on 4 November at 9am", harnessNow)
	if !strings.HasPrefix(upd.Direct, "Updated") {
		t.Errorf("correction plan = %q", upd.Direct)
	}
	user = readNotes(t, a, "USER.md")
	if strings.Contains(user, "3 November") || !strings.Contains(user, "4 November at 9am") || strings.Count(user, "ppointment") != 1 {
		t.Errorf("the old note was not replaced:\n%s", user)
	}
	// facts that are not about the user go to MEMORY.md
	a.harnessPlan("note that the Rotterdam budget was 1,800 euros", harnessNow)
	if !strings.Contains(readNotes(t, a, "MEMORY.md"), "Rotterdam budget was 1,800 euros") {
		t.Errorf("a general note belongs in MEMORY.md:\n%s", readNotes(t, a, "MEMORY.md"))
	}
	// forgetting removes only what matches
	a.harnessPlan("remember that I prefer tea over coffee", harnessNow)
	gone := a.harnessPlan("forget about the dentist appointment", harnessNow)
	if !gone.Wrote || !strings.Contains(gone.Direct, "forgotten") && !strings.Contains(gone.Direct, "Gone") && !strings.Contains(gone.Direct, "Done") {
		t.Errorf("forget plan = %+v", gone)
	}
	user = readNotes(t, a, "USER.md")
	if strings.Contains(user, "ppointment") || !strings.Contains(user, "tea over coffee") {
		t.Errorf("forget removed the wrong thing:\n%s", user)
	}
	if none := a.harnessPlan("forget the volcano", harnessNow); none.Wrote || !strings.Contains(none.Direct, "nothing") && !strings.Contains(none.Direct, "couldn't") {
		t.Errorf("forgetting something unknown should say so: %+v", none)
	}
	if q := a.harnessPlan("do you remember my dentist appointment?", harnessNow); q.Direct != "" || q.Wrote {
		t.Errorf("a question is not an instruction to save: %+v", q)
	}
}

func TestHarnessLearnsFactsTheUserVolunteers(t *testing.T) {
	a := harnessApp(t)
	// a message that is only facts is acknowledged by the harness itself
	plan := a.harnessPlan("By the way I'm allergic to peanuts, and my wife is called Priya.", harnessNow)
	if !plan.Wrote || !strings.HasSuffix(plan.Direct, "You're allergic to peanuts, and your wife is called Priya.") {
		t.Fatalf("plan = %+v", plan)
	}
	user := readNotes(t, a, "USER.md")
	for _, want := range []string{"Allergic to peanuts", "Wife is called Priya"} {
		if !strings.Contains(user, want) {
			t.Errorf("USER.md is missing %q:\n%s", want, user)
		}
	}
	// a fact plus a request: saved by the harness, the model is told, and still does the request
	mixed := a.harnessPlan("I love jazz. Can you suggest a good album to start with?", harnessNow)
	if mixed.Direct != "" || len(mixed.Notes) != 1 || !strings.Contains(mixed.Notes[0], "already saved") || !strings.Contains(mixed.Notes[0], "Do not call any tool") {
		t.Errorf("mixed message plan = %+v", mixed)
	}
	if !strings.Contains(readNotes(t, a, "USER.md"), "Loves jazz") {
		t.Error("the fact in a mixed message was not saved")
	}
	if q := a.harnessPlan("Am I allergic to anything I should tell the waiter?", harnessNow); q.Wrote {
		t.Error("a question must not be saved as a fact")
	}
}

func TestHarnessAnswersQuestionsFromTheNotes(t *testing.T) {
	a := harnessApp(t)
	a.harnessPlan("remember that my car service is on 14 December", harnessNow)
	a.harnessPlan("my dog is called Biscuit and I'm allergic to shellfish", harnessNow)
	a.harnessPlan("note that the Rotterdam budget was 1,800 euros", harnessNow)
	cases := map[string]string{
		"when is my car service?":          "Your car service is on 14 December.",
		"What is my dog called?":           "Your dog is called Biscuit.",
		"what's the Rotterdam budget":      "The Rotterdam budget was 1,800 euros.",
		"what am I allergic to?":           "You're allergic to shellfish.",
		"when is my passport renewal?":     "",
		"what is the capital of Portugal?": "",
	}
	for q, want := range cases {
		plan := a.harnessPlan(q, harnessNow)
		if plan.Direct != want {
			t.Errorf("%q -> %q, want %q", q, plan.Direct, want)
		}
	}
}

func TestMangledWorkspacePathsAreMappedIntoTheWorkspaceButOtherPathsAreNot(t *testing.T) {
	a := harnessApp(t)
	_ = os.MkdirAll(a.workspace, 0700)
	// the model garbles the long path it was given; the file belongs in the workspace
	got, inside := a.resolvePath("C:/usersysadmin/Appdata\\Local\\SovereignAI/todo.txt")
	if !inside || filepath.Base(got) != "todo.txt" || filepath.Dir(got) != a.workspace {
		t.Errorf("mangled path -> %q (inside=%v)", got, inside)
	}
	got, inside = a.resolvePath(filepath.Join(a.workspace, "memory", "n.md"))
	if !inside || got != filepath.Join(a.workspace, "memory", "n.md") {
		t.Errorf("a real workspace path must be unchanged: %q", got)
	}
	// a genuinely different location is still outside, and still needs approval
	if _, inside := a.resolvePath("D:\\Projects\\notes.txt"); inside {
		t.Error("an ordinary outside path must stay outside")
	}
}

func TestHarnessOffersOnlyTheToolsAMessageNeeds(t *testing.T) {
	cases := []struct {
		text string
		has  []string
		not  []string
	}{
		{"read notes.txt and tell me what it says", []string{"read_file", "list_dir"}, []string{"shell", "web_search"}},
		{"search the web for the latest Go release", []string{"web_search", "web_fetch"}, []string{"shell", "read_file"}},
		{"run git status in the shell", []string{"shell"}, []string{"web_search", "write_file"}},
		{"tell me a joke", []string{"memory_search"}, []string{"shell", "write_file", "remember"}},
		{"do I have a dentist appointment coming up?", []string{"memory_search"}, []string{"shell"}},
	}
	for _, c := range cases {
		tools := toolsFor(c.text)
		for _, n := range c.has {
			if !tools[n] {
				t.Errorf("%q should offer %s", c.text, n)
			}
		}
		for _, n := range c.not {
			if tools[n] {
				t.Errorf("%q should not offer %s", c.text, n)
			}
		}
		if tools["remember"] {
			t.Errorf("remember is the harness's job, never the model's")
		}
	}
}

func TestReplyFilterRemovesNarrationRepeatsAndFalseClaims(t *testing.T) {
	run := func(spoken, wrote bool, chunks ...string) string {
		var got strings.Builder
		f := newReplyFilter(spoken, wrote, func(e agentEvent) { got.WriteString(e.Delta) })
		for _, c := range chunks {
			f.Event(agentEvent{Delta: c})
		}
		f.Finish()
		return strings.TrimSpace(got.String())
	}
	// narration before and after the answer, streamed in token-sized pieces
	if got := run(true, false, "I'll comp", "ute that for you now", ".\n\nIt", "’s 391", ". I", "’m looking it up."); got != "It’s 391." {
		t.Errorf("narration: %q", got)
	}
	if got := run(false, false, "Sure, let me check that. ", "Your file has 3 lines. ", "Let me know if you want more."); got != "Your file has 3 lines. Let me know if you want more." {
		t.Errorf("'let me know' is not narration: %q", got)
	}
	// the same confirmation twice
	if got := run(true, true, "I've saved your dentist appointment: 3 November. ", "I've stored your dentist appointment for 3 November."); got != "I've saved your dentist appointment: 3 November." {
		t.Errorf("repeat: %q", got)
	}
	// a claim of saving when nothing was saved
	if got := run(true, false, "Your dentist is on the 3rd. ", "I've saved that to your notes."); got != "Your dentist is on the 3rd." {
		t.Errorf("false claim kept: %q", got)
	}
	// ...but a real save keeps it
	if got := run(true, true, "I've saved that to your notes."); got != "I've saved that to your notes." {
		t.Errorf("true claim dropped: %q", got)
	}
	// markdown in an answer that will be spoken
	if got := run(true, false, "Here are the options:\n", "- **Tea** is calming.\n", "- Coffee is quick.\n"); strings.ContainsAny(got, "*-") {
		t.Errorf("markdown reached a spoken reply: %q", got)
	}
	// text mode keeps its formatting
	if got := run(false, false, "Options:\n", "- **Tea**\n"); !strings.Contains(got, "- **Tea**") {
		t.Errorf("text mode must keep markdown: %q", got)
	}
	// never leave the user with nothing
	if got := run(true, false, "I'll look that up."); got != "I'll look that up." {
		t.Errorf("an all-dropped reply must still show something: %q", got)
	}
	// a claim made after a real write tool ran is kept
	var got strings.Builder
	f := newReplyFilter(true, false, func(e agentEvent) { got.WriteString(e.Delta) })
	f.Event(agentEvent{Tool: "write_file"})
	f.Event(agentEvent{Delta: "I've written the file."})
	f.Finish()
	if !strings.Contains(got.String(), "I've written the file.") {
		t.Errorf("a write that really happened: %q", got.String())
	}
}

func TestHarnessEndToEndSkipsTheModelAndNarrowsItsTools(t *testing.T) {
	a := harnessApp(t)
	model := useFakeModel(t,
		textChunk("Let me check that. ")+"\n"+textChunk("Foxes are canids."), // only the second one (joke/chat) is used
	)
	// 1. an exact question never reaches the model
	var events []agentEvent
	if err := a.runTurn(context.Background(), turnOptions{Session: "h", Text: "what is 17 times 23?", Spoken: true}, collect(&events)); err != nil {
		t.Fatal(err)
	}
	if replyText(events) != "17 times 23 is 391." || len(model.requests) != 0 {
		t.Errorf("reply %q, model calls %d", replyText(events), len(model.requests))
	}
	// 2. remembering never reaches the model, and survives in the saved conversation
	events = nil
	if err := a.runTurn(context.Background(), turnOptions{Session: "h", Text: "remember that I prefer tea over coffee"}, collect(&events)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replyText(events), "Prefers tea over coffee") || len(model.requests) != 0 {
		t.Errorf("reply %q, model calls %d", replyText(events), len(model.requests))
	}
	if !strings.Contains(readNotes(t, a, "USER.md"), "Prefers tea over coffee") {
		t.Error("the preference was not saved")
	}
	// 3. a chat message reaches the model with a trimmed tool list, and its narration is dropped
	events = nil
	if err := a.runTurn(context.Background(), turnOptions{Session: "h", Text: "tell me about foxes"}, collect(&events)); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model calls = %d", len(model.requests))
	}
	if replyText(events) != "Foxes are canids." {
		t.Errorf("reply = %q", replyText(events))
	}
	offered := map[string]bool{}
	for _, tool := range model.requests[0]["tools"].([]any) {
		offered[tool.(map[string]any)["function"].(map[string]any)["name"].(string)] = true
	}
	if offered["shell"] || offered["remember"] || offered["write_file"] || !offered["memory_search"] {
		t.Errorf("tools offered for a chat message: %v", offered)
	}
	// a fact the user volunteers is saved by the harness and the model is told so
	events = nil
	_ = a.runTurn(context.Background(), turnOptions{Session: "h", Text: "I'm allergic to shellfish."}, collect(&events))
	if !strings.Contains(readNotes(t, a, "USER.md"), "Allergic to shellfish") {
		t.Error("the volunteered fact was not saved")
	}
}

func TestHarnessReadsEachFactInASentenceOnItsOwn(t *testing.T) {
	a := harnessApp(t)
	a.harnessPlan("I'm allergic to shellfish and my dog is called Biscuit.", harnessNow)
	user := readNotes(t, a, "USER.md")
	for _, want := range []string{"- Allergic to shellfish (", "- Dog is called Biscuit ("} {
		if !strings.Contains(user, want) {
			t.Errorf("missing %q in:\n%s", want, user)
		}
	}
	if strings.Contains(user, "shellfish and") {
		t.Errorf("two facts were merged:\n%s", user)
	}
	if got := splitClauses("I like tea, and my sister lives in Leeds. Nice day"); len(got) != 3 {
		t.Errorf("clauses = %q", got)
	}
}

func TestHarnessAcceptsRememberAfterAFillerWord(t *testing.T) {
	a := harnessApp(t)
	a.harnessPlan("remember that my car service is on 12 December", harnessNow)
	plan := a.harnessPlan("actually remember that my car service is on 14 December", harnessNow)
	if !strings.HasPrefix(plan.Direct, "Updated") {
		t.Fatalf("plan = %q", plan.Direct)
	}
	user := readNotes(t, a, "USER.md")
	if strings.Contains(user, "12 December") || !strings.Contains(user, "14 December") {
		t.Errorf("the correction did not replace the old date:\n%s", user)
	}
}

func TestForgettingAlsoClearsTheConversation(t *testing.T) {
	a := harnessApp(t)
	model := useFakeModel(t)
	ctx := context.Background()
	var events []agentEvent
	_ = a.runTurn(ctx, turnOptions{Session: "f", Text: "remember that my locker code is 4821"}, collect(&events))
	s := a.session("f")
	if len(s.msgs) == 0 || !strings.Contains(s.msgs[0].Content, "4821") {
		t.Fatalf("setup: %+v", s.msgs)
	}
	events = nil
	_ = a.runTurn(ctx, turnOptions{Session: "f", Text: "forget my locker code"}, collect(&events))
	if !strings.Contains(replyText(events), "forgotten") {
		t.Errorf("reply = %q", replyText(events))
	}
	for _, m := range a.session("f").msgs {
		if strings.Contains(m.Content, "4821") {
			t.Errorf("the code is still in the conversation: %+v", m)
		}
	}
	if strings.Contains(readNotes(t, a, "USER.md"), "4821") || len(model.requests) != 0 {
		t.Error("the note must be gone and the model never involved")
	}
}

func TestCompoundRequestsRunStepByStep(t *testing.T) {
	if got := splitSteps("Create a file called todo.txt with buy milk, then read it back to me."); len(got) != 2 || got[1] != "read it back to me." && got[1] != "read it back to me" {
		t.Errorf("steps = %q", got)
	}
	if got := splitSteps("check the time and then tell me a joke, afterwards say goodnight"); len(got) != 3 {
		t.Errorf("steps = %q", got)
	}
	for _, single := range []string{"what is the weather like?", "if it rains then bring an umbrella", "write a poem about the sea", "then"} {
		if got := splitSteps(single); got != nil {
			t.Errorf("%q should be one task, got %q", single, got)
		}
	}
	a := harnessApp(t)
	model := useFakeModel(t, textChunk("Foxes are canids."), textChunk("Owls hunt at night."))
	var events []agentEvent
	if err := a.runTurn(context.Background(), turnOptions{Session: "c", Text: "tell me about foxes, then tell me about owls"}, collect(&events)); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model calls = %d, want one per step", len(model.requests))
	}
	if got := replyText(events); !strings.Contains(got, "Foxes are canids.") || !strings.Contains(got, "Owls hunt at night.") {
		t.Errorf("reply = %q", got)
	}
	// each step is a separate user message, so the second step sees what the first did
	if msgs := a.session("c").msgs; msgs[0].Content != "tell me about foxes" || msgs[2].Content != "tell me about owls" {
		t.Errorf("history = %+v", msgs)
	}
}

func TestReplyFilterCatchesMoreNarrationAndFalseUpdates(t *testing.T) {
	rules := func() *replyRules { return &replyRules{spoken: true, wrote: func() bool { return false }} }
	for _, s := range []string{"Now I'll look it up.", "First, let me check the file.", "Okay, I'm going to run that for you.", "I've updated my notes: car service is now on 14 December.", "I'm still looking for it."} {
		if text, ok := rules().sentence(s); ok {
			t.Errorf("%q should be dropped, kept as %q", s, text)
		}
	}
	for _, s := range []string{"Let me know if you need anything else.", "I'll be here if you need me.", "Your service is on 14 December.", "I can't see a file called that."} {
		if _, ok := rules().sentence(s); !ok {
			t.Errorf("%q should be kept", s)
		}
	}
}

func TestHarnessSearchesTheWebItselfForQuestionsAboutTheChangingWorld(t *testing.T) {
	a := harnessApp(t)
	var asked []string
	a.harness.search = func(ctx context.Context, q string) (string, error) {
		asked = append(asked, q)
		return "1. Prime Minister - Canada\n   https://example.org\n   Mark Carney is the current Prime Minister of Canada.", nil
	}
	plan := a.harnessPlan("Who is the current prime minister of Canada?", harnessNow)
	if plan.Direct != "" || len(asked) != 1 || asked[0] != "Who is the current prime minister of Canada" {
		t.Fatalf("plan %+v, searches %q", plan, asked)
	}
	if len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "Mark Carney") || !strings.Contains(plan.Notes[0], "already searched") {
		t.Errorf("the results must reach the model: %v", plan.Notes)
	}
	// ordinary chat does not search
	asked = nil
	a.harnessPlan("tell me about foxes", harnessNow)
	a.harnessPlan("what is the capital of Portugal?", harnessNow)
	if len(asked) != 0 {
		t.Errorf("searched for ordinary questions: %q", asked)
	}
	// a secret from the notes is never put in a search
	_ = os.MkdirAll(filepath.Join(a.workspace, "memory"), 0700)
	_ = os.WriteFile(filepath.Join(a.workspace, "memory", "n.md"), []byte("# n\n\n- Safe code is bluewhale42.\n"), 0600)
	asked = nil
	if p := a.harnessPlan("what is the latest news about bluewhale42?", harnessNow); len(asked) != 0 || strings.Contains(strings.Join(p.Notes, " "), "bluewhale42") {
		t.Errorf("a secret was searched for: %q", asked)
	}
}

func TestHarnessAnswersAnyWikipediaFactFromRecentContext(t *testing.T) {
	a := harnessApp(t)
	var requested string
	a.harness.fetch = func(ctx context.Context, address string) (string, error) {
		requested = address
		if !strings.Contains(address, "generator=random") {
			return "", errors.New("unexpected Wikipedia request: " + address)
		}
		return `{"query":{"pages":{"42":{"title":"Honey","extract":"Honey stored in sealed containers can remain edible for thousands of years."}}}}`, nil
	}
	model := useFakeModel(t, textChunk("I'm well."))
	session := a.session("wikipedia-fact")
	session.msgs = []chatMsg{
		{Role: "user", Content: "Let's use Playwright on Wikipedia; find me a fact."},
		{Role: "assistant", Content: "What fact do you want?"},
	}
	var events []agentEvent
	if err := a.runTurn(context.Background(), turnOptions{Session: session.key, Text: "Any fact, let's find one now."}, collect(&events)); err != nil {
		t.Fatal(err)
	}
	got := replyText(events)
	if !strings.Contains(got, "Honey stored in sealed containers can remain edible for thousands of years.") || !strings.Contains(got, "https://en.wikipedia.org/wiki/Honey") {
		t.Fatalf("reply = %q, want a sourced Wikipedia fact", got)
	}
	if requested == "" || len(model.requests) != 0 {
		t.Errorf("Wikipedia request = %q; model calls = %d, want the harness lookup and no model call", requested, len(model.requests))
	}
}

func TestHarnessUsesConfiguredPlaywrightForWikipediaFact(t *testing.T) {
	a := harnessApp(t)
	configureFakeServer(t, a, "playwright")
	a.harness.fetch = func(context.Context, string) (string, error) {
		return "", errors.New("Wikipedia API fallback should not run when Playwright is configured")
	}
	plan := a.harnessPlanWithContext("Any fact, let's find one now.", harnessNow, "Let's use Playwright on Wikipedia; find me a fact.")
	if !strings.Contains(plan.Direct, "Honey stored in sealed containers can remain edible for thousands of years.") || !strings.Contains(plan.Direct, "https://en.wikipedia.org/wiki/Special:Random") {
		t.Fatalf("plan = %+v, want the Playwright page fact with its Wikipedia source", plan)
	}
}

func TestDashboardPlaywrightFactFlowSurvivesSessionReload(t *testing.T) {
	a := harnessApp(t)
	configureFakeServer(t, a, "playwright")
	a.harness.fetch = func(context.Context, string) (string, error) {
		return "", errors.New("Wikipedia API fallback should not run when Playwright is configured")
	}
	model := useFakeModel(t, textChunk("I have a tool to run a browser: Playwright. What are you planning to do with it?"))
	var events []agentEvent
	if err := a.runTurn(context.Background(), turnOptions{Session: dashboardKey, Text: "playwright"}, collect(&events)); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("tool-identification request made %d model calls, want 1", len(model.requests))
	}

	events = nil
	if err := a.runTurn(context.Background(), turnOptions{Session: dashboardKey, Text: "Let's use it on Wikipedia; find me a fact."}, collect(&events)); err != nil {
		t.Fatal(err)
	}
	if got := replyText(events); !strings.Contains(got, "Honey stored in sealed containers can remain edible for thousands of years.") || !strings.Contains(got, "https://en.wikipedia.org/wiki/Special:Random") {
		t.Fatalf("first fact request = %q", got)
	}
	if len(model.requests) != 1 {
		t.Fatalf("fact request reached the model; calls = %d", len(model.requests))
	}

	// The dashboard session is persisted and reloaded between browser visits.
	a.agent.sessions = nil
	events = nil
	if err := a.runTurn(context.Background(), turnOptions{Session: dashboardKey, Text: "Any fact, let's find one now."}, collect(&events)); err != nil {
		t.Fatal(err)
	}
	if got := replyText(events); !strings.Contains(got, "Honey stored in sealed containers can remain edible for thousands of years.") || !strings.Contains(got, "https://en.wikipedia.org/wiki/Special:Random") {
		t.Fatalf("follow-up fact request = %q", got)
	}
	model.assertComplete(t)

	a.mu.Lock()
	logs := strings.Join(a.state.RecentLogs, "\n")
	a.mu.Unlock()
	if !strings.Contains(logs, "route=wikipedia_fact") || !strings.Contains(logs, "prior_wikipedia=true") || !strings.Contains(logs, "direct=true") {
		t.Fatalf("route trace is missing the contextual Wikipedia decision: %s", logs)
	}
	if strings.Contains(logs, "Any fact, let's find one now") {
		t.Fatal("route trace retained raw user text")
	}
}

func TestSearchFallsBackAndNeverPretendsToHaveResults(t *testing.T) {
	a := harnessApp(t)
	botCheck := "<html><body>Unfortunately, bots use DuckDuckGo too. Please complete the challenge.</body></html>"
	// DuckDuckGo blocks us: Wikipedia answers instead
	a.harness.fetch = func(ctx context.Context, address string) (string, error) {
		switch {
		case strings.Contains(address, "duckduckgo.com"):
			return botCheck, nil
		case strings.Contains(address, "w/api.php"):
			return `{"query":{"search":[{"title":"Prime Minister of Canada"}]}}`, nil
		case strings.Contains(address, "rest_v1/page/summary/Prime_Minister_of_Canada"):
			return `{"extract":"The prime minister of Canada is the head of government. The incumbent is Mark Carney."}`, nil
		}
		return "", errors.New("unexpected " + address)
	}
	out, err := a.webSearch(context.Background(), "prime minister of canada")
	if err != nil || !strings.Contains(out, "Mark Carney") || !strings.Contains(out, "Wikipedia") {
		t.Fatalf("fallback result %q, %v", out, err)
	}
	// but not for a question about the changing world: that is a failure, not a stale article
	if _, err := a.webSearch(context.Background(), "who is the current prime minister of canada"); err == nil {
		t.Error("a 'current ...' question must not be answered from an encyclopedia article")
	}
	// nothing answers: that is an error telling the model not to guess, never "no results"
	a.harness.fetch = func(ctx context.Context, address string) (string, error) { return botCheck, nil }
	if _, err := a.webSearch(context.Background(), "anything"); err == nil || !strings.Contains(err.Error(), "Do not guess") {
		t.Errorf("a failed search must say so: %v", err)
	}
	// ... and the harness tells the model when its own lookup failed
	a.harness.search = func(ctx context.Context, q string) (string, error) { return "", errors.New("down") }
	plan := a.harnessPlan("what is the latest Go version?", harnessNow)
	if len(plan.Notes) == 0 || !strings.Contains(plan.Notes[0], "could not") || !strings.Contains(plan.Notes[0], "Do not guess") {
		t.Errorf("notes = %v", plan.Notes)
	}
}

func TestHarnessAnswersTheWeatherExactly(t *testing.T) {
	a := harnessApp(t)
	var asked string
	a.harness.fetch = func(ctx context.Context, address string) (string, error) {
		asked = address
		return "Sunny, +15°C (feels +11°C), wind ↘16km/h, humidity 45%\n", nil
	}
	plan := a.harnessPlan("What's the weather like in London today?", harnessNow)
	if plan.Direct != "In London right now: Sunny, 15°C (feels 11°C), wind 16km/h, humidity 45%." || !strings.Contains(asked, "wttr.in/London") {
		t.Errorf("plan %q, asked %q", plan.Direct, asked)
	}
	if p := a.harnessPlan("what's the temperature in New York right now?", harnessNow); !strings.HasPrefix(p.Direct, "In New York right now") {
		t.Errorf("two-word place: %q", p.Direct)
	}
	// no place, or not about now: the model handles it
	for _, q := range []string{"what's the weather like?", "will it rain tomorrow in Paris?", "weather forecast for the weekend in Leeds"} {
		asked = ""
		if p := a.harnessPlan(q, harnessNow); strings.HasPrefix(p.Direct, "In ") || asked != "" && strings.Contains(asked, "wttr.in") {
			t.Errorf("%q should not be answered by the harness: %q", q, p.Direct)
		}
	}
}
