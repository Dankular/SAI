package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func widgetApp(t *testing.T) *app {
	t.Helper()
	a := newTestApp(t)
	a.state.SetupComplete = true
	return a
}

func postWidget(t *testing.T, a *app, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleWidget(rec, httptest.NewRequest(http.MethodPost, "/api/widget", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestWidgetSizeIsCustomOrPreset(t *testing.T) {
	s := Snapshot{WidgetSize: "large"}
	if w, h := widgetLogicalSize(s); w != 610 || h != 490 {
		t.Errorf("preset = %dx%d", w, h)
	}
	s.WidgetWidth = 800
	if w, h := widgetLogicalSize(s); w != 800 || h != 490 {
		t.Errorf("a chosen width keeps the preset height: %dx%d", w, h)
	}
	s.WidgetHeight = 99999
	if _, h := widgetLogicalSize(s); h != widgetMaxSize {
		t.Errorf("a saved size is clamped, got %d", h)
	}
	if w, h := widgetLogicalSize(Snapshot{}); w != 500 || h != 400 {
		t.Errorf("no setting at all is the medium preset, got %dx%d", w, h)
	}
}

func TestSettingsRoundTripAndPersist(t *testing.T) {
	a := widgetApp(t)
	rec, out := postWidget(t, a, `{"width":700,"height":560,"spiral":false,"spiralSize":0.8,"colors":{"active":"#ff8800","working":"22cc88"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	view := out["appearance"].(map[string]any)
	if view["Width"].(float64) != 700 || view["Height"].(float64) != 560 || view["SpiralOn"] != false || view["SpiralSize"].(float64) != 0.8 {
		t.Errorf("appearance = %v", view)
	}
	colors := view["Colors"].(map[string]any)
	if colors["active"] != "#ff8800" || colors["working"] != "#22cc88" || colors["idle"] != stockStateHex("idle") {
		t.Errorf("colors = %v", colors)
	}
	// survives a restart: it is in the state file
	data, err := os.ReadFile(a.statePath)
	if err != nil || !strings.Contains(string(data), `"widgetWidth": 700`) || !strings.Contains(string(data), "#ff8800") {
		t.Errorf("state file = %q (%v)", data, err)
	}
	style := styleFromSnapshot(a.state)
	if !style.NoSpiral || style.SpiralSize != 0.8 || style.Colors["active"] != (orbRGB{255, 136, 0}) {
		t.Errorf("style = %+v", style)
	}
}

func TestSettingsRejectBadValuesAndChangeNothing(t *testing.T) {
	a := widgetApp(t)
	for _, body := range []string{
		`{"width":50}`, `{"height":5000}`, `{"spiralSize":9}`, `{"spiralSize":0}`,
		`{"colors":{"active":"orange"}}`, `{"colors":{"sleepy":"#112233"}}`, `{"colors":{"active":"#12345"}}`,
	} {
		rec, _ := postWidget(t, a, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, rec.Code)
		}
	}
	if a.state.WidgetWidth != 0 || a.state.WidgetHeight != 0 || a.state.WidgetSpiralSize != 0 || a.state.WidgetColors != nil {
		t.Errorf("a refused request changed the settings: %+v", a.state)
	}
	// one bad field must not let the good ones through
	rec, _ := postWidget(t, a, `{"width":600,"colors":{"active":"nope"}}`)
	if rec.Code != http.StatusBadRequest || a.state.WidgetWidth != 0 {
		t.Errorf("a request with one bad field must change nothing: %d, width %d", rec.Code, a.state.WidgetWidth)
	}
}

func TestStockColourIsStoredAsNotChosen(t *testing.T) {
	a := widgetApp(t)
	postWidget(t, a, `{"colors":{"active":"#ff8800"}}`)
	if a.state.WidgetColors["active"] != "#ff8800" {
		t.Fatalf("colors = %v", a.state.WidgetColors)
	}
	postWidget(t, a, `{"colors":{"active":"`+stockStateHex("active")+`"}}`)
	if a.state.WidgetColors != nil {
		t.Errorf("picking the stock colour again should clear the choice, got %v", a.state.WidgetColors)
	}
	postWidget(t, a, `{"colors":{"idle":"#010203","speaking":"#040506"}}`)
	postWidget(t, a, `{"colors":{"idle":""}}`)
	if _, has := a.state.WidgetColors["idle"]; has || a.state.WidgetColors["speaking"] != "#040506" {
		t.Errorf("an empty value resets just that state: %v", a.state.WidgetColors)
	}
}

func TestResizeIsReportedOnlyWhenTheSizeChanges(t *testing.T) {
	s := Snapshot{WidgetSize: "medium"}
	one, two := 520, 520
	if resize, _ := applyWidgetAppearance(&s, widgetAppearance{Width: &one}); !resize {
		t.Error("a new width is a resize")
	}
	if resize, _ := applyWidgetAppearance(&s, widgetAppearance{Width: &two}); resize {
		t.Error("the same width again is not")
	}
	off := false
	if resize, _ := applyWidgetAppearance(&s, widgetAppearance{Spiral: &off}); resize {
		t.Error("hiding the spiral needs no new widget")
	}
	reset := true
	if resize, _ := applyWidgetAppearance(&s, widgetAppearance{Reset: &reset}); !resize || s.WidgetWidth != 0 || s.WidgetNoSpiral {
		t.Errorf("reset should put the size, spiral and colours back: resize=%v %+v", resize, s)
	}
}

func TestPreviewDrawsWithTheChosenSettings(t *testing.T) {
	a := widgetApp(t)
	get := func(query string) (*httptest.ResponseRecorder, []byte) {
		rec := httptest.NewRecorder()
		a.handleWidgetPreview(rec, httptest.NewRequest(http.MethodGet, "/api/widget/preview?"+query, nil))
		return rec, rec.Body.Bytes()
	}
	rec, stock := get("state=active&w=400&h=320")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("status %d type %q: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	img, err := png.Decode(bytes.NewReader(stock))
	if err != nil || img.Bounds().Dx() != 400 || img.Bounds().Dy() != 320 {
		t.Fatalf("preview is not a 400x320 png: %v", err)
	}
	_, noSpiral := get("state=active&w=400&h=320&spiral=0")
	_, orange := get("state=active&w=400&h=320&colors=active:ff8800")
	_, bigSpiral := get("state=active&w=400&h=320&size=1.5")
	for name, other := range map[string][]byte{"spiral off": noSpiral, "orange": orange, "bigger spiral": bigSpiral} {
		if bytes.Equal(stock, other) {
			t.Errorf("%s looks identical to the stock preview", name)
		}
	}
	// an oversized request is drawn small, but keeps its shape
	_, wide := get("w=1000&h=500")
	wi, _ := png.Decode(bytes.NewReader(wide))
	if wi.Bounds().Dx() != previewMaxSide || wi.Bounds().Dy() != previewMaxSide/2 {
		t.Errorf("a 1000x500 preview should come back %dx%d, got %v", previewMaxSide, previewMaxSide/2, wi.Bounds())
	}
	for _, bad := range []string{"state=sleepy", "w=10&h=10", "size=9", "colors=active:zzz", "colors=active"} {
		if rec, _ := get(bad); rec.Code != http.StatusBadRequest && bad != "colors=active" {
			t.Errorf("%s: status %d, want 400", bad, rec.Code)
		}
	}
}

func TestSettingsViewListsEveryStateWithItsStockColour(t *testing.T) {
	view := widgetViewOf(Snapshot{WidgetColors: map[string]string{"speaking": "#abcdef", "junk": "#123456"}})
	if len(view.Colors) != len(widgetStates) || len(view.StockColors) != len(widgetStates) {
		t.Fatalf("view = %+v", view)
	}
	if view.Colors["speaking"] != "#abcdef" || view.Colors["idle"] != view.StockColors["idle"] || !view.SpiralOn || view.SpiralSize != 1 {
		t.Errorf("view = %+v", view)
	}
	for _, state := range widgetStates {
		if _, ok := parseHexColor(view.StockColors[state]); !ok {
			t.Errorf("stock colour of %s is not a colour: %q", state, view.StockColors[state])
		}
	}
}

// Every script and stylesheet the dashboard page asks for must be one the server will hand out. A file
// that is embedded but not on the allowlist is a silent 404, and its feature just never starts.
func TestEveryAssetTheDashboardNamesIsServed(t *testing.T) {
	page, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?:src|href)="(/[^"#?]+\.(?:js|css))"`).FindAllStringSubmatch(string(page), -1)
	seen := map[string]bool{}
	for _, ref := range refs {
		seen[ref[1]] = true
	}
	for _, want := range []string{"/app.js", "/memory.js", "/dashboard.css"} {
		if !seen[want] {
			t.Fatalf("index.html no longer loads %s (found %v)", want, seen)
		}
	}
	for _, ref := range refs {
		if !publicPath(ref[1]) {
			t.Errorf("index.html loads %s but the server will not serve it", ref[1])
		}
		if _, err := os.Stat("web" + ref[1]); err != nil {
			t.Errorf("index.html loads %s but web%s does not exist", ref[1], ref[1])
		}
	}
	// and the modules those scripts import, which have their own allowlist entries
	for _, file := range []string{"web/app.js", "web/memory.js", "web/widget.js"} {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, m := range regexp.MustCompile(`from "(/[^"]+\.js)"`).FindAllStringSubmatch(string(data), -1) {
			if !publicPath(m[1]) {
				t.Errorf("%s imports %s which the server will not serve", file, m[1])
			}
		}
	}
}

// The Memory drawer reads .length on every list, so the API must send [] and never null.
func TestMemoryAPISendsEmptyListsNotNull(t *testing.T) {
	a := newTestApp(t)
	rec := httptest.NewRecorder()
	a.handleMemory(rec, httptest.NewRequest(http.MethodGet, "/api/memory", nil))
	body := rec.Body.String()
	for _, key := range []string{"notes", "events", "jobs", "skills"} {
		if !strings.Contains(body, `"`+key+`":[]`) {
			t.Errorf("%s should be an empty list in %s", key, body)
		}
	}
}
