package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// When a note is corrected, the notes file holds only the new value. The old one is kept as a
// "fact_changed" event so "what was it before?" still has an answer. These events are written by
// Go from the user's own correction, so their evidence is the note itself rather than a chat quote.

// recordFactChange appends a fact_changed event for a replaced note. previous and current are the
// note texts without their date stamps.
func (a *app) recordFactChange(previous, current string) {
	previous = strings.TrimSpace(noteDateRE.ReplaceAllString(strings.TrimSpace(previous), ""))
	current = strings.TrimSpace(noteDateRE.ReplaceAllString(strings.TrimSpace(current), ""))
	if previous == "" || current == "" || strings.EqualFold(previous, current) {
		return
	}
	now := time.Now()
	record := memoryEventRecord{
		Event:       "fact_changed",
		Summary:     fmt.Sprintf("%s (was: %s)", strings.TrimRight(current, "."), strings.TrimRight(previous, ".")),
		Information: fmt.Sprintf("Before %s: %s. Since then: %s.", now.Format("2 January 2006"), strings.TrimRight(previous, "."), strings.TrimRight(current, ".")),
		Scope:       "user", Status: "explicit", CreatedAt: now.UTC().Format(time.RFC3339),
		Evidence: []memoryEventEvidence{{SourceID: "harness:correction", Role: "user", Quote: clipText(current, 240)}},
	}
	record.ID = "evt_" + hashText("fact_changed\x00" + previous + "\x00" + current + "\x00" + now.Format(time.RFC3339Nano))[:24]
	path := a.memoryEventsPath()
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return
	}
	a.agent.eventsMu.Lock()
	defer a.agent.eventsMu.Unlock()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	if json.NewEncoder(file).Encode(record) == nil {
		a.memoryDirty()
	}
}

// noteBodies lists the text of every bullet in a notes file, without dates.
func noteBodies(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") {
			out = append(out, strings.TrimSpace(noteDateRE.ReplaceAllString(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")), "")))
		}
	}
	return out
}
