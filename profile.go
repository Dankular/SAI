package main

import (
	"fmt"
	"sort"
	"strings"
)

// profileBlock is a short, evidence-backed summary of what the assistant has learned about its
// one user, built in Go from the event history so the model never has to rediscover it:
// stated preferences, current decisions, and anything the user pinned. Inferred preferences and
// superseded decisions are left out.
const profileBudget = 900 // characters

func (a *app) profileBlock() string {
	events, err := a.loadMemoryEvents()
	if err != nil || len(events) == 0 {
		return ""
	}
	superseded := map[string]bool{}
	for _, e := range events {
		if e.SupersedesID != "" {
			superseded[e.SupersedesID] = true
		}
	}
	type entry struct {
		line    string
		pinned  bool
		created string
	}
	var entries []entry
	seen := map[string]bool{}
	for _, e := range events {
		if superseded[e.ID] || e.Scope != "user" {
			continue
		}
		pinned := a.memoryUseOf("event:" + e.ID).Pinned
		var line string
		switch {
		case e.Event == "preference_expressed" && e.Status == "explicit" && e.Preference != "":
			line = e.Preference
			if e.PreferenceScope != "" {
				line += " (" + e.PreferenceScope + ")"
			}
		case e.Event == "decision_changed" && e.Decision != "":
			line = "Decided: " + e.Decision
		case pinned:
			line = e.Summary
		}
		key := normalizeEventText(line)
		if line == "" || seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, entry{line: line, pinned: pinned, created: e.CreatedAt})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].pinned != entries[j].pinned {
			return entries[i].pinned
		}
		return entries[i].created > entries[j].created
	})
	var b strings.Builder
	b.WriteString("\n=== What you have learned about the user ===\n")
	head := b.Len()
	for _, e := range entries {
		line := fmt.Sprintf("- %s\n", clipText(e.line, 200))
		if b.Len()-head+len(line) > profileBudget {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
