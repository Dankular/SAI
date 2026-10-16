package main

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
	"time"
)

// The values below were read back from the web orb's own GLSL snoise running on a GPU (headless Edge,
// SwiftShader), at these points. Matching them, and staying inside +-1, is what makes the desktop sphere
// morph by the same amount as the web one.
func TestSimplexMatchesTheWebShader(t *testing.T) {
	for _, c := range []struct{ x, y, z, want float64 }{
		{0.1, 0.2, 0.3, -0.475}, {1.1, 2.2, 0.3, 0.773}, {-3, 0.5, 2, -0.145}, {0.7, -1.3, 2.9, 0.427},
	} {
		if got := orbSimplex(c.x, c.y, c.z); math.Abs(got-c.want) > 0.02 {
			t.Errorf("snoise(%v, %v, %v) = %.3f, the web shader gives %.3f", c.x, c.y, c.z, got, c.want)
		}
	}
}

func TestSimplexStaysWithinOne(t *testing.T) {
	// In double precision the shader's hash goes out of range at exact multiples of 7 and 49, giving
	// values of +-4; the port does those steps in float32 as the GPU does.
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 200000; i++ {
		x, y, z := r.Float64()*10-5, r.Float64()*10-5, r.Float64()*10-5
		if v := orbSimplex(x, y, z); math.Abs(v) > 1.05 {
			t.Fatalf("snoise(%v, %v, %v) = %.3f, outside the range the web shader has", x, y, z, v)
		}
	}
}

func fidelityScene(level float64, noSpiral bool) orbFrameInputs {
	return orbFrameInputs{State: "active", ChangedAgeMs: -1, StimulusAgeMs: -1, FlashAgeMs: -1, PixelRatio: 1,
		Level: level, Bass: level, Mid: level, Treble: level, Style: orbStyle{NoSpiral: noSpiral}}
}

// A loud voice must make the sphere lumpy, not send it out in shards: the silhouette stays inside the
// halo's circle, as it does on the web.
func TestLoudVoiceMorphsTheSphereWithoutExploding(t *testing.T) {
	const w, h = 640, 570
	frame := renderOrbFrameWithInputs(w, h, fidelityScene(0.9, true), 6)
	cx, cy := float64(w-1)/2, float64(h-1)/2
	far := 0.0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if frame[(y*w+x)*4+3] > 40 {
				far = math.Max(far, math.Hypot(float64(x)-cx, float64(y)-cy))
			}
		}
	}
	sphere := float64(h) * orbSphereHeightFraction
	if far > sphere*1.6 {
		t.Errorf("something is drawn %.0f px from the centre, %.1fx the sphere's radius: the sphere has exploded", far, far/sphere)
	}
}

func TestHaloRingIsDrawnAroundTheSphere(t *testing.T) {
	const w, h = 640, 570
	frame := renderOrbFrameWithInputs(w, h, fidelityScene(0.5, true), 6)
	cx, cy := w/2, h/2
	sphere := float64(h) * orbSphereHeightFraction
	// just outside the sphere's own edge, on the horizontal axis, only the halo can light a pixel
	ring := int(sphere * 1.18)
	if a := frame[(cy*w+cx+ring)*4+3]; a < 8 {
		t.Errorf("no halo at %d px from the centre (alpha %d)", ring, a)
	}
	if a := frame[(cy*w+cx+int(sphere*2.4))*4+3]; a != 0 {
		t.Errorf("something is lit far outside the halo (alpha %d)", a)
	}
}

// Drawing in bands on many workers must give exactly the picture one worker gives.
func TestParallelDrawingMatchesSingleThreaded(t *testing.T) {
	scene := fidelityScene(0.6, false)
	scene.Treble = 0.3
	orbWorkerOverride = 1
	single := renderOrbFrameWithInputs(480, 400, scene, 4.2)
	orbWorkerOverride = 6
	many := renderOrbFrameWithInputs(480, 400, scene, 4.2)
	orbWorkerOverride = 0
	if !bytes.Equal(single, many) {
		diff := 0
		for i := range single {
			if single[i] != many[i] {
				diff++
			}
		}
		t.Errorf("%d bytes differ between 1 worker and 6", diff)
	}
}

func TestFullRateOnlyWhenSomethingIsHappening(t *testing.T) {
	calm := orbFrameInputs{State: "active", ChangedAgeMs: 5000, StimulusAgeMs: -1, FlashAgeMs: -1}
	if orbWantsFullRate(calm) {
		t.Error("a calm orb should be drawn slowly")
	}
	for name, busy := range map[string]orbFrameInputs{
		"voice":       {State: "active", Level: 0.2, ChangedAgeMs: 5000, StimulusAgeMs: -1, FlashAgeMs: -1},
		"thinking":    {State: "working", ChangedAgeMs: 5000, StimulusAgeMs: -1, FlashAgeMs: -1},
		"a pulse":     {State: "active", ChangedAgeMs: 5000, StimulusAgeMs: 300, FlashAgeMs: -1},
		"a flash":     {State: "active", ChangedAgeMs: 5000, StimulusAgeMs: -1, FlashAgeMs: 100},
		"a new state": {State: "active", ChangedAgeMs: 200, StimulusAgeMs: -1, FlashAgeMs: -1},
		"waking":      {State: "idle", Waking: true, ChangedAgeMs: 5000, StimulusAgeMs: -1, FlashAgeMs: -1},
	} {
		if !orbWantsFullRate(busy) {
			t.Errorf("%s should be drawn at the full rate", name)
		}
	}
}

// A loose guard against an accidental return to the slow renderer (it was 100-140 ms a frame once, and a
// widget that slow looks like smear instead of shimmer). The bound is generous so slow machines pass.
func TestOrbFrameStaysFast(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	scene := fidelityScene(0.7, false)
	renderOrbFrameWithInputs(500, 400, scene, 1) // build the one-time tables
	start := time.Now()
	const n = 5
	for i := 0; i < n; i++ {
		renderOrbFrameWithInputs(500, 400, scene, float64(i))
	}
	if per := time.Since(start) / n; per > 250*time.Millisecond {
		t.Errorf("a 500x400 frame takes %v; it was about 25 ms when this was written", per)
	}
}

// The whole-pixel blend used to present frames must agree with the plain per-channel blend.
func TestPixelBlendMatchesChannelBlend(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	from, to := make([]byte, 4096), make([]byte, 4096)
	r.Read(from)
	r.Read(to)
	for _, amount := range []float64{0.01, 0.25, 0.5, 0.77, 0.99} {
		got := make([]byte, len(from))
		blendNativeOrbFrames(got, from, to, amount)
		for i := range got {
			want := float64(from[i])*(1-amount) + float64(to[i])*amount
			if math.Abs(float64(got[i])-want) > 1.6 {
				t.Fatalf("amount %.2f byte %d: got %d, want about %.1f", amount, i, got[i], want)
			}
		}
	}
}

// A frame drawn into a recycled, dirty buffer must be identical to one drawn into a fresh buffer.
func TestRecycledBufferGivesTheSameFrame(t *testing.T) {
	scene := fidelityScene(0.4, false)
	fresh := renderOrbFrameWithInputs(320, 260, scene, 2.5)
	dirty := bytes.Repeat([]byte{0xAB}, 320*260*4)
	if got := renderOrbFrameInto(dirty, 320, 260, scene, 2.5); !bytes.Equal(got, fresh) {
		t.Error("a frame drawn into an old buffer differs from a fresh one")
	}
	if renderOrbFrameInto(make([]byte, 10), 320, 260, scene, 2.5) != nil {
		t.Error("a buffer of the wrong size must be refused")
	}
}

func TestWorkersScaleWithNeed(t *testing.T) {
	if orbWorkersFor(false, false) != 1 || orbWorkersFor(true, false) != 2 || orbWorkersFor(false, true) != 2 || orbWorkersFor(true, true) != 4 {
		t.Error("calm plain orb: 1, busy plain or calm spiral: 2, busy spiral: 4")
	}
	// whatever the count, the picture is the same
	scene := fidelityScene(0.5, false)
	var first []byte
	for _, n := range []int{1, 2, 4} {
		scene.Workers = n
		frame := renderOrbFrameWithInputs(400, 330, scene, 3)
		if first == nil {
			first = frame
		} else if !bytes.Equal(first, frame) {
			t.Errorf("%d workers draw a different picture than 1", n)
		}
	}
}

// Thinking fires the neuron the way the web's soma-merge phase does: a gold burn at the core that swells
// and dies away, with a gap, about twice a second.
func TestThinkingBurnsAtTheCoreAndFades(t *testing.T) {
	const w, h = 320, 280
	coreGold := func(elapsed float64) int {
		in := orbFrameInputs{State: "working", ChangedAgeMs: 5000, StimulusAgeMs: -1, FlashAgeMs: -1, PixelRatio: 1, Style: orbStyle{NoSpiral: true}}
		frame := renderOrbFrameWithInputs(w, h, in, elapsed)
		sum := 0
		for y := h/2 - 6; y <= h/2+6; y++ {
			for x := w/2 - 6; x <= w/2+6; x++ {
				i := (y*w + x) * 4
				sum += int(frame[i+2]) // red: gold has plenty, the cyan base has little
			}
		}
		return sum
	}
	lo, hi := 1<<30, 0
	for step := 0; step < 24; step++ {
		v := coreGold(float64(step) * 0.0225)
		lo, hi = min(lo, v), max(hi, v)
	}
	if hi < lo*13/10 {
		t.Errorf("the core does not pulse while thinking: red ranges only %d..%d over one burst", lo, hi)
	}
}

func TestAfterglowFollowsThinkingThenFades(t *testing.T) {
	const w, h = 320, 280
	violet := func(previous, state string, ageMs int64) int {
		in := orbFrameInputs{State: state, PreviousState: previous, ChangedAgeMs: ageMs, StimulusAgeMs: -1, FlashAgeMs: -1, PixelRatio: 1, Style: orbStyle{NoSpiral: true}}
		frame := renderOrbFrameWithInputs(w, h, in, 4)
		sum := 0
		for y := h/2 - 30; y <= h/2+30; y++ {
			for x := w/2 - 30; x <= w/2+30; x++ {
				i := (y*w + x) * 4
				sum += int(frame[i+2]) - int(frame[i+1])/2 // violet is red-heavy, the cyan base is not
			}
		}
		return sum
	}
	fresh, late, never := violet("working", "active", 200), violet("working", "active", 3000), violet("active", "active", 200)
	if fresh <= late || fresh <= never {
		t.Errorf("no violet afterglow just after thinking: fresh %d, after it faded %d, never thought %d", fresh, late, never)
	}
}

// Dropping faint points (level of detail) must not change how bright the spiral is, only how many points make it.
func TestDetailCullingKeepsTheSpiralsLight(t *testing.T) {
	in := orbFrameInputs{State: "active", StimulusAgeMs: -1, FlashAgeMs: -1, PixelRatio: 1, Workers: 1}
	light := func(detail float64) (total float64, frame []byte) {
		saved := orbGalaxyDetail
		orbGalaxyDetail = detail
		defer func() { orbGalaxyDetail = saved }()
		frame = renderOrbFrameWithInputs(400, 340, in, 3)
		for _, v := range frame {
			total += float64(v)
		}
		return
	}
	full, _ := light(1)
	culled, _ := light(0.3)
	if ratio := culled / full; ratio < 0.97 || ratio > 1.03 {
		t.Errorf("culling changed the total light by %.1f%%", (ratio-1)*100)
	}
}
