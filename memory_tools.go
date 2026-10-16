package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// The model-facing memory tools. Notes (USER.md, MEMORY.md) and the event history stay plain files;
// these tools only search, read and correct them. Everything the model must not get wrong (what a
// forget touches, what a pin keeps) is decided here in code.

var memoryKinds = map[string]string{"note": "workspace", "event": "event", "conversation": "conversation"}

// searchMemoryFiltered runs searchMemory and keeps the hits of one kind, newer than a number of days.
func (a *app) searchMemoryFiltered(ctx context.Context, query, kind string, days, k int) ([]memChunk, error) {
	if kind != "" {
		mapped, ok := memoryKinds[strings.ToLower(kind)]
		if !ok {
			return nil, fmt.Errorf("kind must be note, event or conversation")
		}
		kind = mapped
	}
	hits, err := a.searchMemory(ctx, query, k*4)
	if err != nil {
		return nil, err
	}
	cutoff := time.Time{}
	if days > 0 {
		cutoff = time.Now().AddDate(0, 0, -days)
	}
	out := hits[:0:0]
	for _, h := range hits {
		if kind != "" && h.Kind != kind {
			continue
		}
		if !cutoff.IsZero() {
			if t, err := time.Parse(time.RFC3339, h.CreatedAt); err != nil || t.Before(cutoff) {
				continue
			}
		}
		out = append(out, h)
		if len(out) == k {
			break
		}
	}
	return out, nil
}

func toolMemorySearch(ctx context.Context, a *app, args map[string]any) (string, error) {
	hits, err := a.searchMemoryFiltered(ctx, argString(args, "query"), argString(args, "kind"), argInt(args, "days", 0), 5)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "Nothing relevant is in memory.", nil
	}
	var b strings.Builder
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.SourceID)
		pin := ""
		if a.memoryUseOf(h.SourceID).Pinned {
			pin = " | pinned"
		}
		fmt.Fprintf(&b, "[%s | %s | %s%s | %s | rank %.3f] %s\n%s\n\n", h.Kind, h.Scope, h.Status, pin, h.SourceID, h.Score, h.File, clipText(h.Text, 700))
	}
	a.noteMemoryUse(ids...)
	return b.String(), nil
}

// toolMemoryGet returns the whole of one remembered item by the id a search showed.
func toolMemoryGet(ctx context.Context, a *app, args map[string]any) (string, error) {
	id := strings.TrimSpace(argString(args, "source"))
	if id == "" {
		return "", errors.New("give the source id a memory search showed")
	}
	if strings.HasPrefix(id, "event:") {
		events, err := a.loadMemoryEvents()
		if err != nil {
			return "", err
		}
		for _, event := range events {
			if "event:"+event.ID == id || event.ID == id {
				a.noteMemoryUse("event:" + event.ID)
				return eventText(event) + "\nRecorded: " + event.CreatedAt, nil
			}
		}
		return "", errors.New("no such event")
	}
	if err := a.refreshMemory(ctx); err != nil {
		// A failed embedding refresh still leaves the plain-text chunks usable.
		_ = err
	}
	a.agent.mem.mu.Lock()
	var parts []string
	for _, chunk := range a.agent.mem.chunks {
		if chunk.SourceID == id || strings.HasPrefix(chunk.SourceID, id+":part:") {
			parts = append(parts, chunk.Text)
		}
	}
	a.agent.mem.mu.Unlock()
	if len(parts) == 0 {
		return "", errors.New("no such memory; use a source id from memory_search")
	}
	a.noteMemoryUse(id)
	return strings.Join(parts, "\n"), nil
}

func toolMemoryEdit(ctx context.Context, a *app, args map[string]any) (string, error) {
	switch action := argString(args, "action"); action {
	case "forget":
		about := strings.TrimSpace(argString(args, "about"))
		if about == "" {
			return "", errors.New("say what to forget")
		}
		removed := a.forgetNotes(about)
		events := a.forgetEvents(about)
		if len(removed)+events == 0 {
			return "Nothing matched, so nothing was forgotten.", nil
		}
		return fmt.Sprintf("Done. I forgot %d note(s) and %d recorded event(s) about that.", len(removed), events), nil
	case "update":
		old, replacement := strings.TrimSpace(argString(args, "about")), mdLine(noteDateRE.ReplaceAllString(strings.TrimSpace(argString(args, "text")), ""))
		if old == "" || replacement == "" {
			return "", errors.New("give about (the old fact) and text (the new fact)")
		}
		file := "memory"
		if argString(args, "file") == "user" {
			file = "user"
		}
		removed := a.forgetNotes(old)
		if len(removed) == 0 {
			return "", errors.New("no saved note matches the old fact; use remember for a new one")
		}
		if a.saveFact(file, replacement) != "known" {
			a.recordFactChange(removed[0], replacement)
		}
		return "Done. I changed that to: " + strings.TrimRight(replacement, ".") + ".", nil
	case "pin", "unpin":
		id := strings.TrimSpace(argString(args, "source"))
		if id == "" {
			return "", errors.New("give the source id from memory_search")
		}
		a.setMemoryPinned(id, action == "pin")
		if action == "pin" {
			return "Pinned. It will rank higher and is kept when memory is tidied.", nil
		}
		return "Unpinned.", nil
	}
	return "", errors.New("action must be forget, update, pin or unpin")
}

// forgetEvents drops recorded events whose text covers most of what the user asked to forget. The
// event history is otherwise append-only; a deliberate forget is the one thing allowed to rewrite it.
func (a *app) forgetEvents(about string) int {
	want := significantWords(about)
	if len(want) == 0 {
		return 0
	}
	a.agent.extractMu.Lock()
	defer a.agent.extractMu.Unlock()
	events, err := a.loadMemoryEvents()
	if err != nil || len(events) == 0 {
		return 0
	}
	kept := events[:0:0]
	dropped := 0
	for _, event := range events {
		have := significantWords(event.Summary + " " + event.Information + " " + event.Preference + " " + event.Decision)
		hits := 0
		for w := range want {
			if have[w] {
				hits++
			}
		}
		if hits > 0 && hits*10 >= len(want)*6 && dropped < 3 {
			dropped++
			continue
		}
		kept = append(kept, event)
	}
	if dropped == 0 {
		return 0
	}
	a.rewriteMemoryEvents(kept)
	return dropped
}

func (a *app) rewriteMemoryEvents(events []memoryEventRecord) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for _, event := range events {
		_ = encoder.Encode(event)
	}
	path := a.memoryEventsPath()
	a.agent.eventsMu.Lock()
	tmp := path + ".tmp"
	if os.WriteFile(tmp, output.Bytes(), 0600) == nil {
		_ = os.Rename(tmp, path)
	}
	a.agent.eventsMu.Unlock()
	a.memoryDirty()
}

func memoryToolDefs() []agentTool {
	kinds := []string{"note", "event", "conversation"}
	return []agentTool{
		{
			Name:        "memory_search",
			Description: "Look up what you already know: anything the user told you before, their notes, plans, people and past conversations. Use this FIRST for any question about the user's own life or about something said earlier.",
			Parameters: obj([]string{"query"}, map[string]any{
				"query": str("What to look for"),
				"kind":  map[string]any{"type": "string", "enum": kinds, "description": "Only this kind of memory (optional)"},
				"days":  num("Only memories from the last this many days (optional)"),
			}),
			Run: toolMemorySearch,
		},
		{
			Name:        "memory_get",
			Description: "Read the whole of one memory by the source id that memory_search showed.",
			Parameters:  obj([]string{"source"}, map[string]any{"source": str("Source id, for example event:evt_ab12")}),
			Run:         toolMemoryGet,
		},
		{
			Name:        "memory_edit",
			Description: "Correct memory. forget: drop what matches 'about'. update: replace the old fact 'about' with the new fact 'text'. pin or unpin: keep or release a remembered item by 'source'.",
			Parameters: obj([]string{"action"}, map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"forget", "update", "pin", "unpin"}, "description": "What to do"},
				"about":  str("Words describing the old fact"),
				"text":   str("The new fact (update)"),
				"file":   map[string]any{"type": "string", "enum": []string{"user", "memory"}, "description": "Where the new fact belongs (update)"},
				"source": str("Source id from memory_search (pin, unpin)"),
			}),
			Guard: func(a *app, args map[string]any) string {
				switch argString(args, "action") {
				case "forget":
					return "Forget everything about: " + clipText(argString(args, "about"), 200)
				case "update":
					return "Change what I remember about \"" + clipText(argString(args, "about"), 120) + "\" to: " + clipText(argString(args, "text"), 200)
				}
				return ""
			},
			Terminal: true,
			Run:      toolMemoryEdit,
		},
	}
}

// sortedMemoryKinds is used in listings so output stays stable.
func sortedMemoryKinds() []string { return sortedKeys(memoryKinds) }

var _ = sort.Strings
