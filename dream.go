package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Dreaming is the model-assisted half of consolidation, run at most once a day while the assistant is
// awake and idle. The model reads the notes and the new events and proposes tidy-ups; Go applies only
// what it can check: a new note must be backed by a recorded event, and a merged note may only use words
// from the notes it replaces. Whatever changes is logged in memory/dreams.md.

type dreamProposal struct {
	Add []struct {
		File    string `json:"file"`
		Text    string `json:"text"`
		EventID string `json:"event_id"`
	} `json:"add"`
	Merge []struct {
		Remove []string `json:"remove"`
		Into   string   `json:"into"`
	} `json:"merge"`
}

type dreamReport struct {
	Added  []string
	Merged []string
	Asked  bool
}

func (r dreamReport) changed() bool { return len(r.Added)+len(r.Merged) > 0 }

func (r dreamReport) String() string {
	if !r.Asked {
		return "nothing new to reflect on"
	}
	if !r.changed() {
		return "reflected, no changes needed"
	}
	return fmt.Sprintf("added %d notes and merged %d groups of notes", len(r.Added), len(r.Merged))
}

func dreamSchema() map[string]any {
	return jsonObject([]string{"add", "merge"}, map[string]any{
		"add": jsonArray(jsonObject([]string{"file", "text", "event_id"}, map[string]any{
			"file": map[string]any{"type": "string", "enum": []string{"user", "memory"}}, "text": jsonStr(200), "event_id": jsonStr(60),
		}), 0, 5),
		"merge": jsonArray(jsonObject([]string{"remove", "into"}, map[string]any{
			"remove": jsonArray(jsonStr(240), 2, 4), "into": jsonStr(240),
		}), 0, 3),
	})
}

func (a *app) dreamStatePath() string { return filepath.Join(a.dataDir, "brain", "dream.json") }

func (a *app) lastDream() time.Time {
	var state struct {
		Last string `json:"last"`
	}
	if data, err := os.ReadFile(a.dreamStatePath()); err == nil && json.Unmarshal(data, &state) == nil {
		if t, err := time.Parse(time.RFC3339, state.Last); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (a *app) setLastDream(t time.Time) {
	data, _ := json.Marshal(map[string]string{"last": t.UTC().Format(time.RFC3339)})
	if os.MkdirAll(filepath.Dir(a.dreamStatePath()), 0700) == nil {
		_ = os.WriteFile(a.dreamStatePath(), data, 0600)
	}
}

const dreamPrompt = `You tidy a personal assistant's notes. Reply with JSON {"add":[...],"merge":[...]}.
add: a short third-person note ("Prefers metric units") for something worth keeping from a new event. Give the event's id. Only use facts stated in that event. Skip anything already in the notes.
merge: when two or more notes say the same thing in different words, list their exact texts in remove and one note in into that uses only their words.
Prefer empty lists to guesses.`

func (a *app) dreamOnce(ctx context.Context) (dreamReport, error) {
	var report dreamReport
	since := a.lastDream()
	events, _ := a.loadMemoryEvents()
	superseded := map[string]bool{}
	for _, e := range events {
		if e.SupersedesID != "" {
			superseded[e.SupersedesID] = true
		}
	}
	recent := map[string]memoryEventRecord{}
	var eventLines []string
	for i := len(events) - 1; i >= 0 && len(recent) < 12; i-- {
		e := events[i]
		created, _ := time.Parse(time.RFC3339, e.CreatedAt)
		if !created.After(since) || superseded[e.ID] || e.Scope != "user" || (e.Status != "explicit" && e.Status != "verified" && e.Status != "current") || e.Event == "fact_changed" {
			continue
		}
		recent[e.ID] = e
		eventLines = append(eventLines, fmt.Sprintf("%s: %s", e.ID, clipText(eventText(e), 300)))
	}
	userNotes, memoryNotes := noteBodies(filepath.Join(a.workspace, "USER.md")), noteBodies(filepath.Join(a.workspace, "MEMORY.md"))
	if len(recent) == 0 && len(userNotes)+len(memoryNotes) < 6 {
		return report, nil // nothing new and too little to tidy
	}
	report.Asked = true
	input := "Notes about the user (file user):\n- " + strings.Join(clipList(userNotes, 40), "\n- ") +
		"\n\nOther notes (file memory):\n- " + strings.Join(clipList(memoryNotes, 40), "\n- ") +
		"\n\nNew events:\n" + strings.Join(eventLines, "\n")
	if pairs := likelyDuplicates(append(append([]string{}, userNotes...), memoryNotes...)); len(pairs) > 0 {
		input += "\n\nNotes that may say the same thing (merge them if they do):\n" + strings.Join(pairs, "\n")
	}
	modelCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	reply, _, err := a.callModel(modelCtx, &modelGatewayCall{
		Session: "memory-dream", Step: -1, UserText: input, JSONSchema: dreamSchema(),
		Messages: []chatMsg{{Role: "system", Content: dreamPrompt}, {Role: "user", Content: input}}, emit: func(agentEvent) {},
	})
	if err != nil {
		return report, err
	}
	a.setLastDream(time.Now())
	var proposal dreamProposal
	if json.Unmarshal([]byte(extractJSON(reply)), &proposal) != nil {
		return report, nil
	}
	a.applyDream(proposal, recent, &report)
	if report.changed() {
		a.logDream(report)
	}
	return report, nil
}

func clipList(items []string, max int) []string {
	if len(items) > max {
		return items[len(items)-max:]
	}
	return items
}

// wordsCovered is the share of text's significant words that appear in source.
func wordsCovered(text string, source map[string]bool) float64 {
	words := significantWords(text)
	if len(words) == 0 {
		return 0
	}
	hit := 0
	for w := range words {
		if source[w] {
			hit++
		}
	}
	return float64(hit) / float64(len(words))
}

func (a *app) applyDream(p dreamProposal, recent map[string]memoryEventRecord, report *dreamReport) {
	backedUp := false
	backup := func() {
		if backedUp {
			return
		}
		backedUp = true
		for _, name := range []string{"USER.md", "MEMORY.md"} {
			if data, err := os.ReadFile(filepath.Join(a.workspace, name)); err == nil {
				a.backupMemoryFile(filepath.Join(a.workspace, name), data)
			}
		}
	}
	for _, m := range p.Merge {
		if len(report.Merged) >= 3 || len(m.Remove) < 2 {
			continue
		}
		union := map[string]bool{}
		for _, text := range m.Remove {
			for w := range significantWords(text) {
				union[w] = true
			}
		}
		into := strings.TrimSpace(mdLine(m.Into))
		if len(into) < 5 || wordsCovered(into, union) < 0.8 {
			continue
		}
		file, ok := a.allNotesExist(m.Remove)
		if !ok {
			continue
		}
		backup()
		for _, text := range m.Remove {
			a.removeNoteExact(noteFileName(file), strings.TrimSpace(text))
		}
		a.saveFact(file, into)
		report.Merged = append(report.Merged, strings.Join(m.Remove, " + ")+" -> "+into)
	}
	for _, add := range p.Add {
		if len(report.Added) >= 5 {
			break
		}
		event, ok := recent[strings.TrimSpace(add.EventID)]
		text := strings.TrimSpace(mdLine(add.Text))
		if !ok || len(text) < 5 || (add.File != "user" && add.File != "memory") {
			continue
		}
		if wordsCovered(text, significantWords(eventText(event))) < 0.6 || a.alreadyKnown(text) {
			continue
		}
		backup()
		if a.saveFact(add.File, text) != "known" {
			report.Added = append(report.Added, text)
		}
	}
}

func noteFileName(file string) string {
	if file == "user" {
		return "USER.md"
	}
	return "MEMORY.md"
}

// allNotesExist checks that every text is an existing note in one file, and says which file.
func (a *app) allNotesExist(texts []string) (string, bool) {
	for _, file := range []string{"user", "memory"} {
		have := map[string]bool{}
		for _, body := range noteBodies(noteFilePath(a, file)) {
			have[strings.ToLower(body)] = true
		}
		all := true
		for _, text := range texts {
			if !have[strings.ToLower(strings.TrimSpace(text))] {
				all = false
				break
			}
		}
		if all {
			return file, true
		}
	}
	return "", false
}

func (a *app) logDream(report dreamReport) {
	path := filepath.Join(a.workspace, "memory", "dreams.md")
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return
	}
	existing, _ := os.ReadFile(path)
	var b strings.Builder
	if len(existing) == 0 {
		b.WriteString("# Dreams\nWhat I changed in my notes while you were away.\n\n")
	}
	b.WriteString("## " + time.Now().Format("2006-01-02 15:04") + "\n")
	for _, s := range report.Added {
		b.WriteString("- Added: " + s + "\n")
	}
	for _, s := range report.Merged {
		b.WriteString("- Merged: " + s + "\n")
	}
	b.WriteString("\n")
	_ = os.WriteFile(path, append(existing, []byte(b.String())...), 0600)
	a.memoryDirty()
}

// startDreamer checks hourly and dreams once a day when the assistant is awake and idle.
func (a *app) startDreamer() {
	go func() {
		timer := time.NewTimer(10 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-timer.C:
				a.maybeDream(a.ctx)
				timer.Reset(time.Hour)
			}
		}
	}()
}

func (a *app) maybeDream(ctx context.Context) {
	if time.Since(a.lastDream()) < 24*time.Hour {
		return
	}
	if a.sup != nil && !a.sup.Agent().Awake {
		return
	}
	if a.currentWorkStatus().Busy || a.inferenceBusy() {
		return
	}
	if report, err := a.dreamOnce(ctx); err == nil && report.changed() {
		a.recordCommandOutput("Memory reflection: " + report.String())
	}
}

// likelyDuplicates finds pairs of notes that share enough words to be worth asking about. Plain word
// overlap is too weak to merge on its own (consolidation only merges at 80%), so these go to the model
// as hints, and Go still checks whatever it proposes.
func likelyDuplicates(notes []string) []string {
	var pairs []string
	for i := range notes {
		for j := i + 1; j < len(notes) && len(pairs) < 6; j++ {
			if jaccard(significantWords(notes[i]), significantWords(notes[j])) >= 0.4 {
				pairs = append(pairs, `- "`+notes[i]+`" / "`+notes[j]+`"`)
			}
		}
	}
	return pairs
}
