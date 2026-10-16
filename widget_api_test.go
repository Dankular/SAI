package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleOrbPublishesStateAndMicLevel(t *testing.T) {
	a := newTestApp(t)
	post := httptest.NewRequest(http.MethodPost, "/api/orb", strings.NewReader(`{"state":"listening","level":0.42}`))
	posted := httptest.NewRecorder()
	a.handleOrb(posted, post)
	if posted.Code != http.StatusNoContent {
		t.Fatalf("POST /api/orb status = %d, want %d: %s", posted.Code, http.StatusNoContent, posted.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/api/orb", nil)
	got := httptest.NewRecorder()
	a.handleOrb(got, get)
	if got.Code != http.StatusOK {
		t.Fatalf("GET /api/orb status = %d, want %d: %s", got.Code, http.StatusOK, got.Body.String())
	}
	var snapshot struct {
		State      string  `json:"state"`
		Level      float64 `json:"level"`
		AgeMs      int64   `json:"ageMs"`
		LevelAgeMs int64   `json:"levelAgeMs"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode GET /api/orb: %v", err)
	}
	if snapshot.State != "" {
		t.Errorf("client changed Go-owned orb state to %q", snapshot.State)
	}
	if snapshot.Level != 0.42 {
		t.Errorf("orb mic level = %v, want 0.42", snapshot.Level)
	}
	if snapshot.LevelAgeMs < 0 {
		t.Errorf("fresh orb audio update has negative age: %d", snapshot.LevelAgeMs)
	}
}

func TestHandleOrbIgnoresClientStateAndMirrorsAudioEvents(t *testing.T) {
	a := newTestApp(t)
	request := httptest.NewRequest(http.MethodPost, "/api/orb", strings.NewReader(`{"state":"speaking","level":0.72,"bass":0.61,"mid":0.43,"treble":0.86,"waking":true,"stimulus":true,"flash":true}`))
	response := httptest.NewRecorder()
	a.handleOrb(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("POST /api/orb status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body.String())
	}

	snapshot := a.orbShown.snapshot()
	if snapshot.State != "" || snapshot.Level != 0.72 {
		t.Errorf("client state/audio = (%q, %v), want empty Go-owned state and level 0.72", snapshot.State, snapshot.Level)
	}
	if snapshot.Bass != 0.61 || snapshot.Mid != 0.43 || snapshot.Treble != 0.86 {
		t.Errorf("mirrored voice bands = (%v, %v, %v), want (0.61, 0.43, 0.86)", snapshot.Bass, snapshot.Mid, snapshot.Treble)
	}
	if snapshot.Waking || snapshot.StimulusID == 0 || snapshot.FlashID == 0 {
		t.Errorf("mirrored neuron events = waking:%v stimulus:%d flash:%d, want only transient events active", snapshot.Waking, snapshot.StimulusID, snapshot.FlashID)
	}
	if snapshot.StimulusAgeMs < 0 || snapshot.FlashAgeMs < 0 {
		t.Errorf("fresh neuron events have negative ages: stimulus=%d flash=%d", snapshot.StimulusAgeMs, snapshot.FlashAgeMs)
	}
}

func TestHandleOrbRejectsMicLevelOutsideNormalizedRange(t *testing.T) {
	a := newTestApp(t)
	request := httptest.NewRequest(http.MethodPost, "/api/orb", strings.NewReader(`{"level":1.01}`))
	response := httptest.NewRecorder()
	a.handleOrb(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/orb with level 1.01 status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestHandleOrbStateHeartbeatKeepsMicLevel(t *testing.T) {
	a := newTestApp(t)
	for _, body := range []string{`{"level":0.63}`, `{"state":"listening"}`} {
		request := httptest.NewRequest(http.MethodPost, "/api/orb", strings.NewReader(body))
		response := httptest.NewRecorder()
		a.handleOrb(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("POST /api/orb status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body.String())
		}
	}
	if got := a.orbShown.snapshot().Level; got != 0.63 {
		t.Errorf("state heartbeat changed mic level to %v, want 0.63", got)
	}
}

func TestHandleWidgetPersistsDraggedPosition(t *testing.T) {
	a := newTestApp(t)
	a.state.SetupComplete = true
	a.sup = newSupervisor(nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/widget", strings.NewReader(`{"x":0.25,"y":0.75}`))
	response := httptest.NewRecorder()
	a.handleWidget(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("POST /api/widget status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !a.state.WidgetPositionSet || a.state.WidgetPositionX != 0.25 || a.state.WidgetPositionY != 0.75 {
		t.Fatalf("saved widget position = (%v, %v, set=%v), want (0.25, 0.75, set=true)", a.state.WidgetPositionX, a.state.WidgetPositionY, a.state.WidgetPositionSet)
	}

	reloaded := &app{statePath: a.statePath}
	if err := reloaded.load(); err != nil {
		t.Fatalf("reload persisted app state: %v", err)
	}
	if !reloaded.state.WidgetPositionSet || reloaded.state.WidgetPositionX != 0.25 || reloaded.state.WidgetPositionY != 0.75 {
		t.Errorf("reloaded widget position = (%v, %v, set=%v), want (0.25, 0.75, set=true)", reloaded.state.WidgetPositionX, reloaded.state.WidgetPositionY, reloaded.state.WidgetPositionSet)
	}
}

func TestUpdateWidgetPositionPersistsNativeDragRelease(t *testing.T) {
	a := newTestApp(t)
	a.state.SetupComplete = true
	a.sup = newSupervisor(nil, nil)
	a.updateWidgetPosition(0.35, 0.65, false)

	if !a.state.WidgetPositionSet || a.state.WidgetPositionX != 0.35 || a.state.WidgetPositionY != 0.65 {
		t.Fatalf("native drag position = (%v, %v, set=%v), want (0.35, 0.65, set=true)", a.state.WidgetPositionX, a.state.WidgetPositionY, a.state.WidgetPositionSet)
	}
	reloaded := &app{statePath: a.statePath}
	if err := reloaded.load(); err != nil {
		t.Fatalf("reload persisted native drag position: %v", err)
	}
	if !reloaded.state.WidgetPositionSet || reloaded.state.WidgetPositionX != 0.35 || reloaded.state.WidgetPositionY != 0.65 {
		t.Errorf("reloaded native drag position = (%v, %v, set=%v), want (0.35, 0.65, set=true)", reloaded.state.WidgetPositionX, reloaded.state.WidgetPositionY, reloaded.state.WidgetPositionSet)
	}
}

func TestHandleWidgetClampsDraggedPositionToWorkArea(t *testing.T) {
	a := newTestApp(t)
	a.state.SetupComplete = true
	a.sup = newSupervisor(nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/widget", strings.NewReader(`{"x":-0.2,"y":1.3}`))
	response := httptest.NewRecorder()
	a.handleWidget(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("POST /api/widget status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if a.state.WidgetPositionX != 0 || a.state.WidgetPositionY != 1 {
		t.Errorf("clamped widget position = (%v, %v), want (0, 1)", a.state.WidgetPositionX, a.state.WidgetPositionY)
	}
}

func TestHandleWidgetCornerPresetClearsDraggedPosition(t *testing.T) {
	a := newTestApp(t)
	a.state.SetupComplete = true
	a.state.WidgetPositionSet = true
	a.state.WidgetPositionX, a.state.WidgetPositionY = 0.2, 0.8
	a.sup = newSupervisor(nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/widget", strings.NewReader(`{"corner":"bottom-left"}`))
	response := httptest.NewRecorder()
	a.handleWidget(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("POST /api/widget status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if a.state.WidgetPositionSet || a.state.WidgetCorner != "bottom-left" {
		t.Errorf("corner preset left dragged position active: set=%v corner=%q", a.state.WidgetPositionSet, a.state.WidgetCorner)
	}
}
