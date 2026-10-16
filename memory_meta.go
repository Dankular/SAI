package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// memoryMeta is what the assistant has learned about its own memory: which items it actually
// used and which the user pinned. It lives beside the vector index (not in the workspace, so it
// is never indexed itself) and is cheap to lose: ranking just falls back to similarity.
type memoryUse struct {
	Uses     int    `json:"uses"`
	LastUsed string `json:"last_used,omitempty"`
	Pinned   bool   `json:"pinned,omitempty"`
}

type memoryMetaStore struct {
	mu     sync.Mutex
	loaded bool
	items  map[string]memoryUse
}

func (a *app) memoryMetaPath() string {
	return filepath.Join(a.dataDir, "brain", "salience.json")
}

func (a *app) loadMemoryMetaLocked() {
	m := &a.agent.meta
	if m.loaded {
		return
	}
	m.loaded = true
	m.items = map[string]memoryUse{}
	if data, err := os.ReadFile(a.memoryMetaPath()); err == nil {
		_ = json.Unmarshal(data, &m.items)
	}
}

func (a *app) saveMemoryMetaLocked() {
	m := &a.agent.meta
	data, err := json.Marshal(m.items)
	if err != nil {
		return
	}
	path := a.memoryMetaPath()
	if os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// noteMemoryUse records that these sources were shown to the model.
func (a *app) noteMemoryUse(sourceIDs ...string) {
	if len(sourceIDs) == 0 {
		return
	}
	m := &a.agent.meta
	m.mu.Lock()
	defer m.mu.Unlock()
	a.loadMemoryMetaLocked()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range sourceIDs {
		if id == "" {
			continue
		}
		use := m.items[id]
		use.Uses++
		use.LastUsed = now
		m.items[id] = use
	}
	a.saveMemoryMetaLocked()
}

func (a *app) setMemoryPinned(sourceID string, pinned bool) {
	m := &a.agent.meta
	m.mu.Lock()
	defer m.mu.Unlock()
	a.loadMemoryMetaLocked()
	use := m.items[sourceID]
	use.Pinned = pinned
	if !pinned && use.Uses == 0 {
		delete(m.items, sourceID)
	} else {
		m.items[sourceID] = use
	}
	a.saveMemoryMetaLocked()
	a.memoryDirty()
}

func (a *app) memoryUseOf(sourceID string) memoryUse {
	m := &a.agent.meta
	m.mu.Lock()
	defer m.mu.Unlock()
	a.loadMemoryMetaLocked()
	return m.items[sourceID]
}

// salienceBoost turns use history into a rank adjustment. Pinned items always float up. Use
// counts help with diminishing returns and fade with a 90 day half-life, so what the user
// stopped caring about slides back to plain similarity.
func salienceBoost(use memoryUse, now time.Time) float64 {
	if use.Pinned {
		return 0.20
	}
	if use.Uses == 0 {
		return 0
	}
	boost := 0.04 * math.Log1p(float64(use.Uses))
	if last, err := time.Parse(time.RFC3339, use.LastUsed); err == nil {
		ageDays := math.Max(0, now.Sub(last).Hours()/24)
		boost *= math.Pow(0.5, ageDays/90)
	}
	return math.Min(boost, 0.12)
}
