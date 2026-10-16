package main

import (
	"math"
	"testing"
)

func TestRenderOrbFrameLeavesEmptyPixelsTransparent(t *testing.T) {
	frame := renderOrbFrame(180, 200, "active", 0, 0.5)
	if got, want := len(frame), 180*200*4; got != want {
		t.Fatalf("frame length = %d, want %d", got, want)
	}
	center := (100*180 + 90) * 4
	if got := frame[center+3]; got == 0 {
		t.Fatal("orb center is transparent")
	}
	// Like the web orb, the arms sweep across the whole frame, so the corner may be lit; but the
	// gaps between them stay transparent, which is what lets clicks and the wallpaper through.
	clear := 0
	for i := 3; i < len(frame); i += 4 {
		if frame[i] == 0 {
			clear++
		}
	}
	if clear < len(frame)/4*15/100 {
		t.Fatalf("only %d of %d pixels are transparent", clear, len(frame)/4)
	}
	// With the spiral off, only the sphere and its glow remain, so the corners are empty.
	bare := renderOrbFrameWithInputs(180, 200, orbFrameInputs{State: "active", StimulusAgeMs: -1, FlashAgeMs: -1, Style: orbStyle{NoSpiral: true}}, 0.5)
	if bare[3] != 0 || bare[len(bare)-1] != 0 {
		t.Fatalf("corner alpha = %d / %d with no spiral, want transparent", bare[3], bare[len(bare)-1])
	}
}

func TestRenderOrbFrameReflectsAssistantState(t *testing.T) {
	idle := renderOrbFrame(180, 200, "idle", 0, 0.5)
	working := renderOrbFrame(180, 200, "working", 0, 0.5)
	if equalBytes(idle, working) {
		t.Fatal("idle and working frames are identical")
	}
}

func TestRenderOrbFrameRespondsToVoiceLevel(t *testing.T) {
	quiet := renderOrbFrame(180, 200, "speaking", 0, 0.5)
	loud := renderOrbFrame(180, 200, "speaking", 1, 0.5)
	if equalBytes(quiet, loud) {
		t.Fatal("quiet and loud frames are identical")
	}
}

func TestOrbNeuronNetworkDrawsVisibleFibersAndNodes(t *testing.T) {
	const width, height = 128, 128
	frame := opaqueBlackOrbFrame(width, height)
	renderOrbNeuronNetwork(frame, width, height, 44, "active", 0, 1.25)

	litPixels := 0
	for i := 0; i < len(frame); i += 4 {
		if frame[i] > 0 || frame[i+1] > 0 || frame[i+2] > 0 {
			litPixels++
		}
	}
	if litPixels < 120 {
		t.Fatalf("neuron lit %d pixels, want visible branching fibers and nodes", litPixels)
	}
}

func TestOrbNeuronNetworkRespondsToVoiceLevel(t *testing.T) {
	const width, height = 128, 128
	quiet := opaqueBlackOrbFrame(width, height)
	loud := opaqueBlackOrbFrame(width, height)
	renderOrbNeuronNetwork(quiet, width, height, 44, "speaking", 0, 2.0)
	renderOrbNeuronNetwork(loud, width, height, 44, "speaking", 1, 2.0)
	if equalBytes(quiet, loud) {
		t.Fatal("speaking neuron did not change with voice level")
	}
}

func TestOrbGalaxyKeepsTheWebOrbSpiralTurns(t *testing.T) {
	visibleTurns := orbGalaxyTurnsBetween(1.055, 1.76)
	if visibleTurns < 0.35 || visibleTurns > 0.5 {
		t.Fatalf("visible spiral winds = %.2f, want the web orb's logarithmic pitch in the desktop bounds", visibleTurns)
	}
	fullGalaxyRadius := math.Exp(orbGalaxyPitch * 2 * math.Pi * orbGalaxySourceTurns)
	fullTurns := orbGalaxyTurnsBetween(1, fullGalaxyRadius)
	if math.Abs(fullTurns-orbGalaxySourceTurns) > 0.01 {
		t.Fatalf("full galaxy winds = %.2f, want the web orb's %.2f turns", fullTurns, orbGalaxySourceTurns)
	}
}

func TestWrapOrbAngleNormalizesNegativeGalaxyAngles(t *testing.T) {
	for _, angle := range []float64{-10, -6.5, -3.5, 3.5, 10} {
		wrapped := wrapOrbAngle(angle)
		if wrapped < -math.Pi || wrapped > math.Pi {
			t.Errorf("wrapped angle %.2f = %.2f, want a value in [-pi, pi]", angle, wrapped)
		}
		if math.Abs(math.Sin(wrapped)-math.Sin(angle)) > 1e-9 || math.Abs(math.Cos(wrapped)-math.Cos(angle)) > 1e-9 {
			t.Errorf("wrapped angle %.2f changed its direction to %.2f", angle, wrapped)
		}
	}
}

func TestOrbGalaxyUsesTheWebOrbsTiltedPlane(t *testing.T) {
	_, projectedVertical := orbGalaxyCoordinates(0, 10)
	projectedVertical = math.Abs(projectedVertical)
	if projectedVertical <= 10 {
		t.Fatalf("tilted galaxy vertical coordinate = %.2f, want the web renderer's foreshortened plane", projectedVertical)
	}
	want := 10 * math.Cos(-orbGalaxyRotation) / math.Sin(orbGalaxyTilt)
	if math.Abs(projectedVertical-want) > 0.01 {
		t.Fatalf("tilted galaxy vertical coordinate = %.2f, want %.2f", projectedVertical, want)
	}
}

func TestOrbGalaxyParticleCloudMatchesTheWebDistribution(t *testing.T) {
	particles := buildOrbGalaxyParticles()
	if len(particles) != orbGalaxyParticleCount {
		t.Fatalf("native galaxy particles = %d, want %d", len(particles), orbGalaxyParticleCount)
	}
	var arms [orbGalaxyArmCount]int
	dust := 0
	maxRadius := 0.0
	for _, particle := range particles {
		if particle.isDust {
			dust++
			continue
		}
		arms[particle.arm]++
		maxRadius = math.Max(maxRadius, particle.radius)
	}
	for arm, count := range arms {
		if count < orbGalaxyParticleCount/8 {
			t.Errorf("spiral arm %d has only %d particles", arm, count)
		}
	}
	if dust < orbGalaxyParticleCount/25 {
		t.Errorf("galaxy hugging dust ring has %d particles, want %d+", dust, orbGalaxyParticleCount/25)
	}
	if maxRadius < 3.5*orbGalaxyCoreRadius {
		t.Errorf("outer spiral radius = %.2f, want the web orb's 1.15-turn reach", maxRadius)
	}
}

func TestRenderOrbFrameShowsGalaxyArmsOutsideSphere(t *testing.T) {
	const width, height = 420, 490
	frame := renderOrbFrame(width, height, "active", 0.2, 0.5)
	strongGalaxyPixels := 0
	centerX, centerY := float64(width-1)/2, float64(height-1)/2
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			distance := math.Hypot(float64(x)-centerX, float64(y)-centerY)
			if distance > 130 && frame[(y*width+x)*4+3] > 40 {
				strongGalaxyPixels++
			}
		}
	}
	if strongGalaxyPixels < 500 {
		t.Fatalf("visible galaxy pixels = %d, want 500+ outside the sphere", strongGalaxyPixels)
	}
}

func TestRenderOrbFrameRespondsToVoiceBands(t *testing.T) {
	quiet := renderOrbFrameWithInputs(180, 200, orbFrameInputs{State: "speaking", Level: 0.25, StimulusAgeMs: -1, FlashAgeMs: -1}, 1.0)
	voiced := renderOrbFrameWithInputs(180, 200, orbFrameInputs{
		State: "speaking", Level: 0.25, Bass: 0.9, Mid: 0.7, Treble: 0.8, StimulusAgeMs: -1, FlashAgeMs: -1,
	}, 1.0)
	if equalBytes(quiet, voiced) {
		t.Fatal("orb frame did not respond to bass, mid, and treble levels")
	}
}

func TestOrbAudioEnvelopeUsesFastAttackAndSlowRelease(t *testing.T) {
	envelope := orbAudioEnvelope{}
	inputs := orbFrameInputs{Level: 1, Bass: 1, Mid: 1, Treble: 1}
	envelope.update(&inputs, 1.0/30)
	if envelope.Level <= 0 || envelope.Treble <= envelope.Mid || envelope.Mid <= envelope.Bass {
		t.Fatalf("audio attack envelope = level %.2f, bass %.2f, mid %.2f, treble %.2f; want fast treble and slower bass", envelope.Level, envelope.Bass, envelope.Mid, envelope.Treble)
	}
	attack := envelope
	inputs = orbFrameInputs{}
	envelope.update(&inputs, 1.0/30)
	if envelope.Level >= attack.Level || envelope.Bass >= attack.Bass || envelope.Mid >= attack.Mid || envelope.Treble >= attack.Treble {
		t.Fatal("audio envelope did not release smoothly when playback level ended")
	}
}

func TestRenderOrbFrameShowsIncomingPulseAndReplyFlash(t *testing.T) {
	base := orbFrameInputs{State: "working", StimulusAgeMs: -1, FlashAgeMs: -1}
	stimulus := base
	stimulus.State = "listening"
	stimulus.StimulusAgeMs = 120
	flash := base
	flash.FlashAgeMs = 120
	plainFrame := renderOrbFrameWithInputs(180, 200, base, 1.0)
	stimulusFrame := renderOrbFrameWithInputs(180, 200, stimulus, 1.0)
	flashFrame := renderOrbFrameWithInputs(180, 200, flash, 1.0)
	if equalBytes(plainFrame, stimulusFrame) {
		t.Fatal("incoming stimulus did not produce a neuron signal")
	}
	if equalBytes(plainFrame, flashFrame) {
		t.Fatal("reply flash did not alter the orb")
	}
}

func BenchmarkRenderOrbFrame(b *testing.B) {
	inputs := orbFrameInputs{State: "speaking", Level: 0.6, Bass: 0.5, Mid: 0.4, Treble: 0.7, StimulusAgeMs: -1, FlashAgeMs: -1}
	renderOrbFrameWithInputs(340, 400, inputs, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		renderOrbFrameWithInputs(340, 400, inputs, float64(i)/30)
	}
}

func BenchmarkRenderOrbGalaxyParticles(b *testing.B) {
	const width, height = 340, 400
	frame := make([]byte, width*height*4)
	inputs := orbFrameInputs{State: "speaking", Level: 0.6, Bass: 0.5}
	palette := orbPalettes["speaking"]
	renderOrbGalaxyParticles(frame, width, height, float64(min(width, height)), palette, inputs, 0.6, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		renderOrbGalaxyParticles(frame, width, height, float64(min(width, height)), palette, inputs, 0.6, float64(i)/30)
	}
}

func opaqueBlackOrbFrame(width, height int) []byte {
	frame := make([]byte, width*height*4)
	for i := 3; i < len(frame); i += 4 {
		frame[i] = 255
	}
	return frame
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
