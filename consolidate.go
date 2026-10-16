package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Consolidation keeps a long-lived memory small and current without asking the model anything:
// repeated notes merge, a newer note about the same subject replaces an older one, repeated
// events collapse, and use counts that went cold are forgotten. Every file it rewrites is copied
// to brain/backups first. It is safe to run at any time and is idempotent.

const staleTaskAge = 60 * 24 * time.Hour

type consolidationReport struct {
	NotesMerged    int `json:"notes_merged"`
	NotesReplaced  int `json:"notes_replaced"`
	EventsMerged   int `json:"events_merged"`
	NotesArchived  int `json:"notes_archived"`
	UsageForgotten int `json:"usage_forgotten"`
}

func (r consolidationReport) String() string {
	return fmt.Sprintf("merged %d duplicate notes, replaced %d outdated notes, moved %d old notes to the archive, merged %d repeated events, dropped %d cold usage records",
		r.NotesMerged, r.NotesReplaced, r.NotesArchived, r.EventsMerged, r.UsageForgotten)
}

func (r consolidationReport) changed() bool {
	return r.NotesMerged+r.NotesReplaced+r.NotesArchived+r.EventsMerged+r.UsageForgotten > 0
}

var noteDateRE = regexp.MustCompile(`\((\d{4}-\d{2}-\d{2})\)\s*$`)

type noteLine struct {
	index int
	text  string // the bullet text without its date
	date  string
	words map[string]bool
}

func (a *app) consolidateMemory() consolidationReport {
	var report consolidationReport
	for _, name := range []string{"USER.md", "MEMORY.md"} {
		merged, replaced := a.consolidateNoteFile(filepath.Join(a.workspace, name))
		report.NotesMerged += merged
		report.NotesReplaced += replaced
	}
	report.NotesArchived = a.archiveOverflow()
	report.EventsMerged = a.consolidateEvents()
	report.UsageForgotten = a.pruneMemoryUsage(time.Now())
	if report.changed() {
		a.memoryDirty()
	}
	return report
}

// jaccard compares two word sets: 1 means identical, 0 means nothing in common.
func jaccard(x, y map[string]bool) float64 {
	if len(x) == 0 || len(y) == 0 {
		return 0
	}
	shared := 0
	for w := range x {
		if y[w] {
			shared++
		}
	}
	return float64(shared) / float64(len(x)+len(y)-shared)
}

func (a *app) consolidateNoteFile(path string) (merged, replaced int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	original := string(data)
	lines := strings.Split(strings.ReplaceAll(original, "\r\n", "\n"), "\n")
	var notes []noteLine
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
		date := ""
		if m := noteDateRE.FindStringSubmatch(body); m != nil {
			date = m[1]
			body = strings.TrimSpace(noteDateRE.ReplaceAllString(body, ""))
		}
		notes = append(notes, noteLine{index: i, text: body, date: date, words: significantWords(body)})
	}
	drop := map[int]bool{}
	for i := range notes {
		if drop[notes[i].index] {
			continue
		}
		for j := i + 1; j < len(notes); j++ {
			if drop[notes[j].index] {
				continue
			}
			older, newer := &notes[i], &notes[j]
			if older.date > newer.date { // dates are ISO, so text order is time order; ties keep the later line
				older, newer = newer, older
			}
			switch {
			case strings.EqualFold(older.text, newer.text) || (len(older.words) >= 3 && jaccard(older.words, newer.words) >= 0.8):
				drop[older.index] = true
				merged++
			case sameSubject(older.text, newer.text):
				drop[older.index] = true
				replaced++
			default:
				continue
			}
			if older == &notes[i] {
				break // note i is gone; stop comparing it
			}
		}
	}
	if len(drop) == 0 {
		return 0, 0
	}
	if !a.backupMemoryFile(path, data) {
		return 0, 0
	}
	kept := make([]string, 0, len(lines))
	for i, l := range lines {
		if !drop[i] {
			kept = append(kept, l)
		}
	}
	if os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0600) != nil {
		return 0, 0
	}
	return merged, replaced
}

func sameSubject(x, y string) bool {
	mx, my := subjectRE.FindStringSubmatch(x), subjectRE.FindStringSubmatch(y)
	if mx == nil || my == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(mx[1]), strings.TrimSpace(my[1]))
}

func (a *app) backupMemoryFile(path string, data []byte) bool {
	dir := filepath.Join(a.dataDir, "brain", "backups")
	if os.MkdirAll(dir, 0700) != nil {
		return false
	}
	name := fmt.Sprintf("%s.%s", filepath.Base(path), time.Now().UTC().Format("20060102T150405"))
	return os.WriteFile(filepath.Join(dir, name), data, 0600) == nil
}

// consolidateEvents collapses events that say the same thing twice, keeping the newest. An event
// another event points at is never dropped, so the references stay whole.
func (a *app) consolidateEvents() int {
	a.agent.extractMu.Lock()
	defer a.agent.extractMu.Unlock()
	events, err := a.loadMemoryEvents()
	if err != nil || len(events) < 2 {
		return 0
	}
	referenced := map[string]bool{}
	for _, e := range events {
		if e.SupersedesID != "" {
			referenced[e.SupersedesID] = true
		}
		for _, id := range e.RelatedIDs {
			referenced[id] = true
		}
	}
	newest := map[string]int{}
	for i, e := range events {
		key := e.Event + "\x00" + e.Scope + "\x00" + normalizeEventText(e.Summary)
		if prev, ok := newest[key]; !ok || events[prev].CreatedAt <= e.CreatedAt {
			newest[key] = i
		}
	}
	kept := events[:0:0]
	merged := 0
	for i, e := range events {
		key := e.Event + "\x00" + e.Scope + "\x00" + normalizeEventText(e.Summary)
		if newest[key] != i && !referenced[e.ID] && !a.memoryUseOf("event:"+e.ID).Pinned {
			merged++
			continue
		}
		kept = append(kept, e)
	}
	if merged == 0 {
		return 0
	}
	if data, err := os.ReadFile(a.memoryEventsPath()); err == nil {
		a.backupMemoryFile(a.memoryEventsPath(), data)
	}
	a.rewriteMemoryEvents(kept)
	return merged
}

// pruneMemoryUsage forgets use counts that have gone cold for half a year. Pins are kept.
func (a *app) pruneMemoryUsage(now time.Time) int {
	m := &a.agent.meta
	m.mu.Lock()
	defer m.mu.Unlock()
	a.loadMemoryMetaLocked()
	dropped := 0
	for id, use := range m.items {
		if use.Pinned {
			continue
		}
		last, err := time.Parse(time.RFC3339, use.LastUsed)
		if err != nil || now.Sub(last) > 180*24*time.Hour {
			delete(m.items, id)
			dropped++
		}
	}
	if dropped > 0 {
		a.saveMemoryMetaLocked()
	}
	return dropped
}

// startMemoryKeeper tidies memory once a day while the app runs, and once shortly after start.
func (a *app) startMemoryKeeper() {
	go func() {
		timer := time.NewTimer(2 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-timer.C:
				if report := a.consolidateMemory(); report.changed() {
					a.recordCommandOutput("Memory tidied: " + report.String())
				}
				timer.Reset(24 * time.Hour)
			}
		}
	}()
}
