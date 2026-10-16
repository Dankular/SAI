package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The notes files are injected into every prompt, so each has a character budget (workspaceFiles).
// When one outgrows it, its oldest notes move to memory/archive.md. Nothing is deleted: the archive
// is still indexed, so memory_search finds archived notes. The prompt only keeps what fits.

const archiveTarget = 0.8 // shrink to this fraction of the budget so it does not re-trigger at once

func (a *app) archiveOverflow() int {
	moved := 0
	for _, f := range workspaceFiles {
		if f.name != "USER.md" && f.name != "MEMORY.md" {
			continue
		}
		moved += a.archiveNotesOver(filepath.Join(a.workspace, f.name), f.max)
	}
	return moved
}

func (a *app) archiveNotesOver(path string, budget int) int {
	data, err := os.ReadFile(path)
	if err != nil || len(data) <= budget {
		return 0
	}
	text := string(data)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	type note struct {
		index int
		date  string
	}
	var notes []note
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		date := ""
		if m := noteDateRE.FindStringSubmatch(trimmed); m != nil {
			date = m[1]
		}
		notes = append(notes, note{index: i, date: date})
	}
	// oldest first; notes with no date are treated as oldest
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].date < notes[j].date })
	size := len(strings.Join(lines, "\n"))
	target := int(float64(budget) * archiveTarget)
	drop := map[int]bool{}
	var archived []string
	for _, n := range notes {
		if size <= target || len(drop) >= len(notes)-3 { // always keep the newest few
			break
		}
		drop[n.index] = true
		archived = append(archived, strings.TrimSpace(lines[n.index]))
		size -= len(lines[n.index]) + 1
	}
	if len(drop) == 0 || !a.backupMemoryFile(path, data) {
		return 0
	}
	archivePath := filepath.Join(a.workspace, "memory", "archive.md")
	if os.MkdirAll(filepath.Dir(archivePath), 0700) != nil {
		return 0
	}
	existing, _ := os.ReadFile(archivePath)
	header := ""
	if len(existing) == 0 {
		header = "# Archived notes\nOlder notes moved out of the always-loaded files. Search finds them.\n\n"
	}
	entry := "## Archived " + time.Now().Format("2006-01-02") + " from " + filepath.Base(path) + "\n" + strings.Join(archived, "\n") + "\n\n"
	if os.WriteFile(archivePath, append(existing, []byte(header+entry)...), 0600) != nil {
		return 0
	}
	kept := make([]string, 0, len(lines))
	for i, l := range lines {
		if !drop[i] {
			kept = append(kept, l)
		}
	}
	if os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0600) != nil {
		return 0
	}
	return len(drop)
}
