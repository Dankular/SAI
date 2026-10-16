package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// widgetSizes are the widget's logical sizes in pixels (width, height).
var widgetSizes = map[string][2]int{"small": {420, 330}, "medium": {500, 400}, "large": {610, 490}}

func clampUnit(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// orbMirror remembers the state the dashboard's sphere is showing (including "speaking", which only
// the dashboard knows because it plays the audio) so the desktop widget can show the same thing.
type orbMirror struct {
	mu             sync.Mutex
	state          string
	previousState  string
	stateAt        time.Time
	stateChangedAt time.Time
	waking         bool
	level          float64
	bass           float64
	mid            float64
	treble         float64
	levelAt        time.Time
	stimulusAt     time.Time
	stimulusID     uint64
	flashAt        time.Time
	flashID        uint64
}

type orbSnapshot struct {
	State         string  `json:"state"`
	PreviousState string  `json:"previousState"`
	AgeMs         int64   `json:"ageMs"`
	ChangedAgeMs  int64   `json:"changedAgeMs"`
	Waking        bool    `json:"waking"`
	Level         float64 `json:"level"`
	Bass          float64 `json:"bass"`
	Mid           float64 `json:"mid"`
	Treble        float64 `json:"treble"`
	LevelAgeMs    int64   `json:"levelAgeMs"`
	StimulusID    uint64  `json:"stimulusId"`
	StimulusAgeMs int64   `json:"stimulusAgeMs"`
	FlashID       uint64  `json:"flashId"`
	FlashAgeMs    int64   `json:"flashAgeMs"`
}

func (o *orbMirror) set(state string) {
	o.mu.Lock()
	now := time.Now()
	if state != o.state {
		o.previousState = o.state
		o.stateChangedAt = now
		o.stateAt = now
	}
	o.state = state
	o.mu.Unlock()
}

func (o *orbMirror) setWaking(waking bool) {
	o.mu.Lock()
	o.waking = waking
	o.mu.Unlock()
}

func (o *orbMirror) setLevelParts(level, bass, mid, treble *float64) {
	o.mu.Lock()
	if level != nil {
		o.level = *level
	}
	if bass != nil {
		o.bass = *bass
	}
	if mid != nil {
		o.mid = *mid
	}
	if treble != nil {
		o.treble = *treble
	}
	o.levelAt = time.Now()
	o.mu.Unlock()
}

func (o *orbMirror) stimulus() {
	o.mu.Lock()
	o.stimulusAt = time.Now()
	o.stimulusID++
	o.mu.Unlock()
}

func (o *orbMirror) flash() {
	o.mu.Lock()
	o.flashAt = time.Now()
	o.flashID++
	o.mu.Unlock()
}

func (o *orbMirror) snapshot() orbSnapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	snapshot := orbSnapshot{AgeMs: -1, ChangedAgeMs: -1, LevelAgeMs: -1, StimulusAgeMs: -1, FlashAgeMs: -1}
	snapshot.Waking = o.waking
	snapshot.StimulusID, snapshot.FlashID = o.stimulusID, o.flashID
	if o.levelAt.IsZero() {
		snapshot.Level = 0
	} else {
		snapshot.Level, snapshot.Bass, snapshot.Mid, snapshot.Treble = o.level, o.bass, o.mid, o.treble
		snapshot.LevelAgeMs = time.Since(o.levelAt).Milliseconds()
	}
	if !o.stimulusAt.IsZero() {
		snapshot.StimulusAgeMs = time.Since(o.stimulusAt).Milliseconds()
	}
	if !o.flashAt.IsZero() {
		snapshot.FlashAgeMs = time.Since(o.flashAt).Milliseconds()
	}
	if o.state == "" {
		return snapshot
	}
	snapshot.State, snapshot.PreviousState = o.state, o.previousState
	snapshot.AgeMs = time.Since(o.stateAt).Milliseconds()
	if !o.stateChangedAt.IsZero() {
		snapshot.ChangedAgeMs = time.Since(o.stateChangedAt).Milliseconds()
	}
	return snapshot
}

// handleOrb returns the Go-owned assistant state and accepts transient audio and animation signals.
func (a *app) handleOrb(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, a.orbShown.snapshot())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256)
	var in struct {
		Level    *float64 `json:"level"`
		Bass     *float64 `json:"bass"`
		Mid      *float64 `json:"mid"`
		Treble   *float64 `json:"treble"`
		Stimulus *bool    `json:"stimulus"`
		Flash    *bool    `json:"flash"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		http.Error(w, "invalid orb update", http.StatusBadRequest)
		return
	}
	for _, field := range []struct {
		name  string
		value *float64
	}{{"level", in.Level}, {"bass", in.Bass}, {"mid", in.Mid}, {"treble", in.Treble}} {
		if field.value != nil && (*field.value < 0 || *field.value > 1) {
			http.Error(w, "orb "+field.name+" must be between 0 and 1", http.StatusBadRequest)
			return
		}
	}
	if in.Level != nil || in.Bass != nil || in.Mid != nil || in.Treble != nil {
		a.orbShown.setLevelParts(in.Level, in.Bass, in.Mid, in.Treble)
	}
	if in.Stimulus != nil && *in.Stimulus {
		a.orbShown.stimulus()
	}
	if in.Flash != nil && *in.Flash {
		a.orbShown.flash()
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleWidget accepts settings or a normalized desktop position. During a drag the client marks
// intermediate coordinates as transient; the final update persists them once the pointer is released.
func (a *app) handleWidget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var in struct {
		Enabled  *bool    `json:"enabled"`
		Corner   *string  `json:"corner"`
		Size     *string  `json:"size"`
		X        *float64 `json:"x"`
		Y        *float64 `json:"y"`
		Dragging *bool    `json:"dragging"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		http.Error(w, "invalid widget settings", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	if !a.state.SetupComplete {
		a.mu.Unlock()
		http.Error(w, "finish setup first", http.StatusConflict)
		return
	}
	if (in.X == nil) != (in.Y == nil) {
		a.mu.Unlock()
		http.Error(w, "widget position needs both x and y", http.StatusBadRequest)
		return
	}
	relayout := false
	if in.X != nil && in.Y != nil {
		a.state.WidgetPositionSet = true
		a.state.WidgetPositionX = clampUnit(*in.X)
		a.state.WidgetPositionY = clampUnit(*in.Y)
		relayout = true
	}
	if in.Corner != nil {
		switch *in.Corner {
		case "top-left", "top-right", "bottom-left", "bottom-right":
			a.state.WidgetCorner, relayout = *in.Corner, true
			a.state.WidgetPositionSet = false
			a.state.WidgetPositionX, a.state.WidgetPositionY = 0, 0
		}
	}
	if in.Size != nil {
		if _, ok := widgetSizes[*in.Size]; ok {
			a.state.WidgetSize, relayout = *in.Size, true
		}
	}
	if in.Enabled != nil {
		a.state.WidgetEnabled = *in.Enabled
	}
	enabled := a.state.WidgetEnabled
	if in.Dragging == nil || !*in.Dragging {
		_ = a.persistLocked()
	}
	a.mu.Unlock()
	if in.Enabled != nil {
		a.sup.Want(svcWidget, enabled)
	}
	if relayout && enabled {
		a.applyWidgetLayout()
	}
	writeJSON(w, map[string]any{"ok": true, "enabled": enabled})
}

// updateWidgetPosition records a native drag in the same normalized position format used by the
// dashboard API. Intermediate moves remain transient; the release position is persisted once.
func (a *app) updateWidgetPosition(x, y float64, dragging bool) {
	a.mu.Lock()
	if !a.state.SetupComplete {
		a.mu.Unlock()
		return
	}
	a.state.WidgetPositionSet = true
	a.state.WidgetPositionX = clampUnit(x)
	a.state.WidgetPositionY = clampUnit(y)
	if !dragging {
		_ = a.persistLocked()
	}
	a.mu.Unlock()
}
