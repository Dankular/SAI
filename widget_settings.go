package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// What the Settings page can change about the desktop widget: its size, whether the spiral shows and how
// big it is, and a colour for each state. Everything is validated here, saved in the app's state file,
// and read back by the renderer on every frame, so a change shows at once (a new size restarts the
// widget, because its drawing surface is made at one size).

const (
	widgetMinSize   = 240
	widgetMaxSize   = 1000
	spiralSizeMin   = 0.3
	spiralSizeMax   = 1.6
	previewMaxSide  = 640
	previewMinSide  = 120
	previewElapsed  = 6.0
	widgetStateHint = "idle, active, listening, working or speaking"
)

// widgetStates are the states with a colour, in the order the page lists them.
var widgetStates = []string{"idle", "active", "listening", "working", "speaking"}

var hexColorRE = regexp.MustCompile(`^#?([0-9a-fA-F]{6})$`)

func validWidgetState(state string) bool {
	for _, s := range widgetStates {
		if s == state {
			return true
		}
	}
	return false
}

// parseHexColor reads "#rrggbb" (the # is optional).
func parseHexColor(text string) (orbRGB, bool) {
	m := hexColorRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return orbRGB{}, false
	}
	v, _ := strconv.ParseUint(m[1], 16, 32)
	return orbRGB{float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)}, true
}

func hexOf(c orbRGB) string {
	return fmt.Sprintf("#%02x%02x%02x", int(c.r+0.5), int(c.g+0.5), int(c.b+0.5))
}

// stockStateHex is the colour a state has when nothing is chosen: the middle shade of its palette.
func stockStateHex(state string) string {
	return hexOf(orbPaletteHex[state].middle)
}

// widgetLogicalSize is the widget's size in logical pixels: a chosen width or height, else the preset.
func widgetLogicalSize(s Snapshot) (width, height int) {
	dims, ok := widgetSizes[s.WidgetSize]
	if !ok {
		dims = widgetSizes["medium"]
	}
	width, height = dims[0], dims[1]
	if s.WidgetWidth > 0 {
		width = min(max(s.WidgetWidth, widgetMinSize), widgetMaxSize)
	}
	if s.WidgetHeight > 0 {
		height = min(max(s.WidgetHeight, widgetMinSize), widgetMaxSize)
	}
	return
}

// styleFromSnapshot turns the saved settings into what the renderer takes.
func styleFromSnapshot(s Snapshot) orbStyle {
	style := orbStyle{NoSpiral: s.WidgetNoSpiral, SpiralSize: s.WidgetSpiralSize}
	for state, hex := range s.WidgetColors {
		if c, ok := parseHexColor(hex); ok && validWidgetState(state) {
			if style.Colors == nil {
				style.Colors = map[string]orbRGB{}
			}
			style.Colors[state] = c
		}
	}
	return style
}

type widgetAppearance struct {
	Width      *int
	Height     *int
	Spiral     *bool
	SpiralSize *float64
	Colors     map[string]string // a state to "#rrggbb"; an empty value puts the stock colour back
	Reset      *bool
}

// applyWidgetAppearance validates and applies the settings to the state. The caller holds a.mu. It
// returns whether the widget must be rebuilt (its size changed) and what was wrong, if anything.
func applyWidgetAppearance(state *Snapshot, in widgetAppearance) (resize bool, problem string) {
	beforeW, beforeH := widgetLogicalSize(*state)
	if in.Width != nil && (*in.Width != 0 && (*in.Width < widgetMinSize || *in.Width > widgetMaxSize)) {
		return false, fmt.Sprintf("width must be between %d and %d", widgetMinSize, widgetMaxSize)
	}
	if in.Height != nil && (*in.Height != 0 && (*in.Height < widgetMinSize || *in.Height > widgetMaxSize)) {
		return false, fmt.Sprintf("height must be between %d and %d", widgetMinSize, widgetMaxSize)
	}
	if in.SpiralSize != nil && (*in.SpiralSize < spiralSizeMin || *in.SpiralSize > spiralSizeMax) {
		return false, fmt.Sprintf("spiral size must be between %.1f and %.1f", spiralSizeMin, spiralSizeMax)
	}
	for name, value := range in.Colors {
		if !validWidgetState(name) {
			return false, name + " is not a state; use " + widgetStateHint
		}
		if _, ok := parseHexColor(value); !ok && strings.TrimSpace(value) != "" {
			return false, value + " is not a colour; use #rrggbb"
		}
	}
	if in.Reset != nil && *in.Reset {
		state.WidgetWidth, state.WidgetHeight = 0, 0
		state.WidgetNoSpiral, state.WidgetSpiralSize = false, 0
		state.WidgetColors = nil
	}
	if in.Width != nil {
		state.WidgetWidth = *in.Width
	}
	if in.Height != nil {
		state.WidgetHeight = *in.Height
	}
	if in.Spiral != nil {
		state.WidgetNoSpiral = !*in.Spiral
	}
	if in.SpiralSize != nil {
		if *in.SpiralSize == 1 {
			state.WidgetSpiralSize = 0 // stock
		} else {
			state.WidgetSpiralSize = *in.SpiralSize
		}
	}
	if len(in.Colors) > 0 {
		// copy on write: the state is read by other goroutines holding the old map
		colors := make(map[string]string, len(state.WidgetColors)+len(in.Colors))
		for k, v := range state.WidgetColors {
			colors[k] = v
		}
		for name, value := range in.Colors {
			c, ok := parseHexColor(value)
			if !ok || hexOf(c) == stockStateHex(name) {
				delete(colors, name) // the stock colour is stored as "not chosen"
				continue
			}
			colors[name] = hexOf(c)
		}
		if len(colors) == 0 {
			colors = nil
		}
		state.WidgetColors = colors
	}
	afterW, afterH := widgetLogicalSize(*state)
	return afterW != beforeW || afterH != beforeH, ""
}

// widgetSettingsView is what the Settings page shows: the effective values and the stock ones.
type widgetSettingsView struct {
	Width, Height int
	SpiralOn      bool
	SpiralSize    float64
	Colors        map[string]string
	StockColors   map[string]string
}

func widgetViewOf(s Snapshot) widgetSettingsView {
	w, h := widgetLogicalSize(s)
	size := s.WidgetSpiralSize
	if size <= 0 {
		size = 1
	}
	view := widgetSettingsView{Width: w, Height: h, SpiralOn: !s.WidgetNoSpiral, SpiralSize: size,
		Colors: map[string]string{}, StockColors: map[string]string{}}
	for _, state := range widgetStates {
		view.StockColors[state] = stockStateHex(state)
		view.Colors[state] = view.StockColors[state]
		if hex, ok := s.WidgetColors[state]; ok {
			if c, valid := parseHexColor(hex); valid {
				view.Colors[state] = hexOf(c)
			}
		}
	}
	return view
}

var previewMu sync.Mutex // one preview render at a time: each is a full software render

// handleWidgetPreview draws one frame of the desktop orb with the settings in the query, as a PNG, using
// the same renderer the desktop widget uses. Nothing is saved.
//
//	state=active  w=500  h=400  spiral=1  size=1.0  colors=idle:ff8800,active:00ff88
func (a *app) handleWidgetPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "active"
	}
	if !validWidgetState(state) {
		http.Error(w, state+" is not a state; use "+widgetStateHint, http.StatusBadRequest)
		return
	}
	number := func(name string, fallback float64) float64 {
		if v, err := strconv.ParseFloat(q.Get(name), 64); err == nil {
			return v
		}
		return fallback
	}
	logicalW := int(number("w", 500))
	logicalH := int(number("h", 400))
	if logicalW < widgetMinSize || logicalW > widgetMaxSize || logicalH < widgetMinSize || logicalH > widgetMaxSize {
		http.Error(w, fmt.Sprintf("w and h must be between %d and %d", widgetMinSize, widgetMaxSize), http.StatusBadRequest)
		return
	}
	spiralSize := number("size", 1)
	if spiralSize < spiralSizeMin || spiralSize > spiralSizeMax {
		http.Error(w, "size is out of range", http.StatusBadRequest)
		return
	}
	style := orbStyle{NoSpiral: q.Get("spiral") == "0", SpiralSize: spiralSize}
	for _, pair := range strings.Split(q.Get("colors"), ",") {
		name, value, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		c, valid := parseHexColor(value)
		if !valid || !validWidgetState(name) {
			http.Error(w, "colors must look like active:ff8800", http.StatusBadRequest)
			return
		}
		if style.Colors == nil {
			style.Colors = map[string]orbRGB{}
		}
		style.Colors[name] = c
	}
	// The scene scales with the frame's height, so a smaller frame is the same picture: draw it small.
	scale := 1.0
	if longest := max(logicalW, logicalH); longest > previewMaxSide {
		scale = float64(previewMaxSide) / float64(longest)
	}
	pw, ph := max(int(float64(logicalW)*scale), previewMinSide), max(int(float64(logicalH)*scale), previewMinSide)
	inputs := orbFrameInputs{State: state, ChangedAgeMs: -1, StimulusAgeMs: -1, FlashAgeMs: -1, PixelRatio: scale, Style: style}

	previewMu.Lock()
	frame := renderOrbFrameWithInputs(pw, ph, inputs, number("t", previewElapsed))
	previewMu.Unlock()

	// premultiplied BGRA over the dark the dashboard shows it on
	img := image.NewNRGBA(image.Rect(0, 0, pw, ph))
	bg := [3]float64{6, 9, 18}
	for i := 0; i < pw*ph; i++ {
		b, g, rr, al := float64(frame[i*4]), float64(frame[i*4+1]), float64(frame[i*4+2]), float64(frame[i*4+3])
		inv := 1 - al/255
		img.SetNRGBA(i%pw, i/pw, color.NRGBA{uint8(min(255, rr+bg[0]*inv)), uint8(min(255, g+bg[1]*inv)), uint8(min(255, b+bg[2]*inv)), 255})
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		http.Error(w, "could not draw the preview", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out.Bytes())
}
