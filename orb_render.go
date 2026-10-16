package main

import (
	"math"
	"sync"
)

type orbRGB struct{ r, g, b float64 }

type orbPalette struct {
	rim, middle, core orbRGB
	glow              float64
	spin, calm, speed float64
	pulse, galaxy     float64
}

var orbPalettes = map[string]orbPalette{
	"idle":      {orbRGB{0x93, 0xa6, 0xd0}, orbRGB{0x5f, 0x73, 0x9f}, orbRGB{0x2f, 0x3d, 0x63}, 0.62, 0.02, 0.0, 0.5, 0.0, 0.12},
	"active":    {orbRGB{0x52, 0xe8, 0xd4}, orbRGB{0x38, 0xb6, 0xe8}, orbRGB{0x25, 0x63, 0xeb}, 0.90, 0.30, 0.4, 1.0, 0.0, 1.0},
	"listening": {orbRGB{0x8d, 0xff, 0xb8}, orbRGB{0x3f, 0xd6, 0xa0}, orbRGB{0x0e, 0x9f, 0x8e}, 1.00, 0.20, 0.8, 0.8, 0.035, 0.7},
	"working":   {orbRGB{0xff, 0x8b, 0xd8}, orbRGB{0xb0, 0x5c, 0xf0}, orbRGB{0x5b, 0x3d, 0xf5}, 1.05, 0.42, 1.0, 1.6, 0.0, 1.5},
	"speaking":  {orbRGB{0xca, 0xf8, 0xff}, orbRGB{0x86, 0xc6, 0xff}, orbRGB{0x3f, 0x7b, 0xff}, 1.25, 0.60, 0.2, 1.9, 0.0, 2.1},
}

const orbGalaxyArmCount = 4
const orbGalaxyParticleCount = 160_000
const orbGalaxyPitch = 0.19
const orbGalaxySourceTurns = 1.15
const orbGalaxyCoreRadius = 5.4
const orbGalaxyScale = 0.52
const orbGalaxyTilt = 1.0
const orbGalaxyRotation = -0.22
const orbGalaxyRotationBins = 2048
const orbGalaxySineBins = 4096
const orbGalaxyMaxRotationRate = 4.5

type orbFrameInputs struct {
	State, PreviousState      string
	ChangedAgeMs              int64
	Level, Bass, Mid, Treble  float64
	Waking                    bool
	StimulusAgeMs, FlashAgeMs int64
}

type orbGalaxyParticle struct {
	radius, angle, lift, star, outward, colorMix, dust float64
	baseSin, baseCos                                   float64
	rotationRate                                       float64
	rotationBin, swellBin                              uint16
	arm                                                uint8
	isDust                                             bool
}

type orbRotationPair struct{ sin, cos float64 }

var (
	orbGalaxyOnce      sync.Once
	orbGalaxyParticles []orbGalaxyParticle
	orbGalaxySineTable [orbGalaxySineBins]float64
)

// renderOrbFrame returns a top-down, premultiplied BGRA frame for UpdateLayeredWindow.
// Pixels outside the sphere and spiral arms keep alpha zero, so wallpaper stays visible
// and empty areas continue to pass mouse clicks through.
func renderOrbFrame(width, height int, state string, level, elapsed float64) []byte {
	return renderOrbFrameWithInputs(width, height, orbFrameInputs{State: state, Level: level, StimulusAgeMs: -1, FlashAgeMs: -1}, elapsed)
}

func renderOrbFrameWithInputs(width, height int, inputs orbFrameInputs, elapsed float64) []byte {
	if width <= 0 || height <= 0 {
		return nil
	}
	palette := orbPaletteFor(inputs, elapsed)
	inputs.Level = clampOrb(inputs.Level)
	inputs.Bass = clampOrb(inputs.Bass)
	inputs.Mid = clampOrb(inputs.Mid)
	inputs.Treble = clampOrb(inputs.Treble)
	frame := make([]byte, width*height*4)
	minSide := float64(min(width, height))
	cx, cy := float64(width-1)/2, float64(height-1)/2
	breath := 0.03 + 0.02*math.Sin(elapsed*1.3)
	displayLevel := math.Min(1, inputs.Level+breath*(1-math.Min(inputs.Level*4, 1)))
	groupPulse := 1 + displayLevel*0.07 + palette.pulse*math.Sin(elapsed*2.2)
	sphereR := minSide * (0.275*groupPulse + inputs.Bass*0.006)
	outerR := minSide * 0.485
	maxGalaxyScreenR := outerR / math.Sin(orbGalaxyTilt)
	glow := palette.glow * (0.88 + displayLevel*0.7)
	flash := orbFlashAmount(inputs.FlashAgeMs)
	sphereAngle := elapsed * palette.spin
	cosSphere, sinSphere := math.Cos(sphereAngle), math.Sin(sphereAngle)

	// Build the same four-arm, 160k-particle galaxy as the web renderer, then
	// paint the shell over it so the arms remain visible through its transparency.
	renderOrbGalaxyParticles(frame, width, height, minSide, palette, inputs, displayLevel, elapsed)

	for y := 0; y < height; y++ {
		dy := float64(y) - cy
		for x := 0; x < width; x++ {
			dx := float64(x) - cx
			r := math.Hypot(dx, dy)
			if r > maxGalaxyScreenR {
				continue
			}
			index := (y*width + x) * 4
			if r > sphereR*1.1 {
				drawOrbHaloPixel(frame, index, r, sphereR, displayLevel, inputs, palette, flash)
				continue
			}
			rotatedDX := dx*cosSphere + dy*sinSphere
			rotatedDY := -dx*sinSphere + dy*cosSphere
			baseNX, baseNY := rotatedDX/sphereR, rotatedDY/sphereR
			baseZ := math.Sqrt(math.Max(0, 1-baseNX*baseNX-baseNY*baseNY))
			low, middle, high := orbSurfaceNoise(baseNX, baseNY, baseZ, elapsed*0.35*palette.speed)
			displacement := low*(0.05+palette.calm*0.035+inputs.Bass*0.66) + middle*(0.018+inputs.Mid*0.36) + high*(inputs.Treble*0.17) + displayLevel*0.07
			surfaceR := sphereR * clampFloat(1+displacement*0.35, 0.9, 1.1)
			if r <= surfaceR {
				nx, ny := rotatedDX/surfaceR, rotatedDY/surfaceR
				z := math.Sqrt(math.Max(0, 1-nx*nx-ny*ny))
				_, _, surfaceHigh := orbSurfaceNoise(nx, ny, z, elapsed*0.35*palette.speed)
				warp := low*0.5 + middle*0.3 + displacement*1.4
				h := ny*0.9 + warp*1.1 + displayLevel*0.3
				gradient := smoothOrb((h + 1) * 0.5)
				base := mixOrbColor(palette.core, palette.middle, smoothOrb(gradient/0.55))
				base = mixOrbColor(base, palette.rim, smoothOrb((gradient-0.45)/0.55))

				// Three voice bands deform and shade the shell like the WebGL shader:
				// bass makes broad lobes, mids warp the gradient, and treble adds fine ripples.
				facing := z
				fresnel := math.Pow(1-facing, 2.2)
				light := 0.55 + 0.45*facing
				heart := math.Pow(facing, 2.2)
				base = scaleOrbColor(base, light*(0.85+displayLevel*0.7))
				base = mixOrbColor(base, mixOrbColor(palette.middle, palette.rim, 0.5), heart*0.16)
				ribbon := 0.5 + 0.5*math.Sin(nx*8+z*7-elapsed*0.55)*math.Sin(ny*7-z*5+elapsed*0.35)
				ribbon += surfaceHigh * inputs.Treble * 0.35
				base = mixOrbColor(base, palette.rim, (ribbon-0.5)*(0.20+inputs.Mid*0.22))
				mesh := orbWire(nx, ny)
				base = mixOrbColor(base, palette.rim, math.Min(0.8, fresnel*0.45+mesh*0.5+math.Abs(surfaceHigh)*inputs.Treble*0.1))
				base = mixOrbColor(base, orbRGB{255, 46, 185}, flash*0.72)
				alpha := math.Min(0.98, (0.34+fresnel*0.35+heart*0.22+displayLevel*0.2)*glow)
				blendOrbPixel(frame, index, base, alpha)
				continue
			}

			drawOrbHaloPixel(frame, index, r, sphereR, displayLevel, inputs, palette, flash)
		}
	}

	renderOrbNeuronNetworkWithInputs(frame, width, height, sphereR, inputs, elapsed)
	return frame
}

func drawOrbHaloPixel(frame []byte, index int, radius, sphereR, level float64, inputs orbFrameInputs, palette orbPalette, flash float64) {
	haloDistance := math.Max(0, radius-sphereR)
	halo := math.Exp(-math.Pow(haloDistance/(sphereR*(0.17+level*0.06)), 2))
	haloAlpha := halo * (0.055 + level*0.12 + inputs.Bass*0.045 + flash*0.16) * palette.glow
	if haloAlpha <= 0.004 {
		return
	}
	haloColor := mixOrbColor(palette.rim, orbRGB{255, 46, 185}, flash)
	blendOrbPixel(frame, index, haloColor, math.Min(0.38, haloAlpha))
}

// orbGalaxyCoordinates inverses the web renderer's tilted and rotated galaxy
// plane projection. It is also used to verify the native plane orientation.
func orbGalaxyCoordinates(x, y float64) (float64, float64) {
	rotation := -orbGalaxyRotation
	cosRotation, sinRotation := math.Cos(rotation), math.Sin(rotation)
	localX := cosRotation*x - sinRotation*y
	localY := -(sinRotation*x + cosRotation*y) / math.Sin(orbGalaxyTilt)
	return localX, localY
}

func buildOrbGalaxyParticles() []orbGalaxyParticle {
	random := orbNeuronRandom{state: 0x4d595df4d0f33173}
	particles := make([]orbGalaxyParticle, orbGalaxyParticleCount)
	for i := range particles {
		r1, r2, r3 := random.float64(), random.float64(), random.float64()
		star := math.Pow(fractOrb(r1*13.7+r3*5.3), 5)
		particle := orbGalaxyParticle{star: star, colorMix: fractOrb(r3 * 7.1)}
		if r1 < 0.05 {
			particle.isDust = true
			particle.radius = orbGalaxyCoreRadius * (0.95 + 0.5*math.Pow(r3, 1.5))
			particle.angle = r2 * 2 * math.Pi
			particle.lift = (fractOrb(r2*53.1) - 0.5) * 0.55 * orbGalaxyCoreRadius * 0.1
			particle.rotationRate = 0.8
			particle.colorMix = 0
			prepareOrbGalaxyParticle(&particle)
			particles[i] = particle
			continue
		}
		particle.arm = uint8(int(fractOrb(r1*37)*orbGalaxyArmCount) % orbGalaxyArmCount)
		theta := r2 * 2 * math.Pi * orbGalaxySourceTurns
		particle.radius = orbGalaxyCoreRadius * math.Exp(orbGalaxyPitch*theta)
		spread := (r3 - 0.5) * 2
		spread = math.Copysign(math.Pow(math.Abs(spread), 1.6), spread)
		particle.radius += spread * particle.radius * 0.09
		particle.angle = theta + float64(particle.arm)*2*math.Pi/float64(orbGalaxyArmCount)
		particle.lift = (fractOrb(r2*91.7+r3*3.1) - 0.5) * 2 * math.Exp(-(particle.radius-orbGalaxyCoreRadius)*0.05) * 0.9
		particle.rotationRate = 9 / math.Sqrt(particle.radius)
		particle.outward = smoothOrb((particle.radius - orbGalaxyCoreRadius) / (orbGalaxyCoreRadius * 3))
		particle.dust = math.Pow(math.Abs(spread), 1.3)
		prepareOrbGalaxyParticle(&particle)
		particles[i] = particle
	}
	return particles
}

func prepareOrbGalaxyParticle(particle *orbGalaxyParticle) {
	particle.baseSin, particle.baseCos = math.Sincos(particle.angle)
	rotationBin := math.Round(particle.rotationRate / orbGalaxyMaxRotationRate * (orbGalaxyRotationBins - 1))
	particle.rotationBin = uint16(clampFloat(rotationBin, 0, orbGalaxyRotationBins-1))
	swellPhase := fractOrb(particle.radius * 0.55 / (2 * math.Pi))
	particle.swellBin = uint16(int(swellPhase * orbGalaxySineBins))
}

func renderOrbGalaxyParticles(frame []byte, width, height int, minSide float64, palette orbPalette, inputs orbFrameInputs, displayLevel, elapsed float64) {
	orbGalaxyOnce.Do(func() {
		orbGalaxyParticles = buildOrbGalaxyParticles()
		for i := range orbGalaxySineTable {
			orbGalaxySineTable[i] = math.Sin(float64(i) * 2 * math.Pi / orbGalaxySineBins)
		}
	})
	centerX, centerY := float64(width-1)/2, float64(height-1)/2
	cosTilt, sinTilt := math.Cos(orbGalaxyTilt), math.Sin(orbGalaxyTilt)
	cosRotation, sinRotation := math.Cos(orbGalaxyRotation), math.Sin(orbGalaxyRotation)
	focalLength := float64(height) / (2 * math.Tan(46*math.Pi/360))
	galaxyTime := elapsed * 0.12 * palette.galaxy
	groupDrift := elapsed * 0.012 * (0.4 + palette.galaxy)
	pixelRatio := minSide / 340
	var rotations [orbGalaxyRotationBins]orbRotationPair
	for i := range rotations {
		rate := orbGalaxyMaxRotationRate * float64(i) / (orbGalaxyRotationBins - 1)
		rotations[i].sin, rotations[i].cos = math.Sincos(galaxyTime*rate + groupDrift)
	}
	swellOffset := int(math.Mod(elapsed*3/(2*math.Pi)*orbGalaxySineBins, orbGalaxySineBins))

	for _, particle := range orbGalaxyParticles {
		rotation := rotations[particle.rotationBin]
		sx := particle.baseCos*rotation.cos + particle.baseSin*rotation.sin
		sz := particle.baseSin*rotation.cos - particle.baseCos*rotation.sin
		swellIndex := (int(particle.swellBin) - swellOffset) & (orbGalaxySineBins - 1)
		swell := 1 + inputs.Bass*0.05*orbGalaxySineTable[swellIndex]
		radius := particle.radius * swell
		x := radius * sx
		z := radius * sz
		y := particle.lift * swell

		// Rotate the source galaxy plane, then project it with the web orb camera.
		rotatedY := y*cosTilt - z*sinTilt
		rotatedZ := y*sinTilt + z*cosTilt
		screenX := cosRotation*x - sinRotation*rotatedY
		screenY := sinRotation*x + cosRotation*rotatedY
		viewDepth := 15 - rotatedZ*orbGalaxyScale
		if viewDepth <= 1 {
			continue
		}
		projection := focalLength / viewDepth * orbGalaxyScale
		pixelX := centerX + screenX*projection
		pixelY := centerY - screenY*projection
		if math.Hypot(pixelX-centerX, pixelY-centerY) > minSide*0.485/math.Sin(orbGalaxyTilt) {
			continue
		}
		if pixelX < -3 || pixelX > float64(width)+3 || pixelY < -3 || pixelY > float64(height)+3 {
			continue
		}

		outer := mixOrbColor(palette.middle, palette.core, particle.colorMix)
		color := mixOrbColor(palette.rim, outer, smoothOrb(particle.outward/0.75))
		boost := 1.15 + (0.7-1.15)*particle.outward
		if particle.isDust {
			color = palette.rim
			boost = 0.55
		} else {
			color = mixOrbColor(color, orbRGB{5, 0, 31}, particle.dust*0.55)
		}
		brightness := (0.7 + particle.star*1.6) * (0.45 + palette.glow*0.7 + displayLevel*0.4) * boost
		color = scaleOrbColor(color, brightness)
		color.r, color.g, color.b = math.Min(230, color.r), math.Min(230, color.g), math.Min(230, color.b)
		diameter := pixelRatio * (0.8 + 2.2*particle.star + 0.4*displayLevel) * (22 / viewDepth)
		alpha := 0.32 + 0.6*particle.star
		drawOrbGalaxyParticle(frame, width, height, pixelX, pixelY, diameter, color, alpha)
	}
}

func drawOrbGalaxyParticle(frame []byte, width, height int, x, y, diameter float64, color orbRGB, alpha float64) {
	coverage := math.Min(1, diameter*diameter*0.62)
	blendOrbSubpixel(frame, width, height, x, y, color, alpha*coverage)
	if diameter > 2.2 {
		halo := alpha * math.Min(0.14, (diameter-2.2)*0.035)
		blendOrbSubpixel(frame, width, height, x-1, y, color, halo)
		blendOrbSubpixel(frame, width, height, x+1, y, color, halo)
		blendOrbSubpixel(frame, width, height, x, y-1, color, halo)
		blendOrbSubpixel(frame, width, height, x, y+1, color, halo)
	}
}

// blendOrbSubpixel distributes a moving particle across adjacent pixels according to its fractional
// position, preventing it from blinking as its center crosses an integer pixel boundary.
func blendOrbSubpixel(frame []byte, width, height int, x, y float64, color orbRGB, alpha float64) {
	px, py := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := x-float64(px), y-float64(py)
	blend := func(x, y int, weight float64) {
		if x < 0 || x >= width || y < 0 || y >= height || weight <= 0.002 {
			return
		}
		blendOrbPixel(frame, (y*width+x)*4, color, alpha*weight)
	}
	blend(px, py, (1-fx)*(1-fy))
	blend(px+1, py, fx*(1-fy))
	blend(px, py+1, (1-fx)*fy)
	blend(px+1, py+1, fx*fy)
}

// orbAudioEnvelope uses the same attack and release rates as the web renderer,
// keeping the native surface responsive without flickering between audio ticks.
type orbAudioEnvelope struct {
	Level, Bass, Mid, Treble float64
}

func (envelope *orbAudioEnvelope) update(inputs *orbFrameInputs, deltaSeconds float64) {
	deltaSeconds = clampFloat(deltaSeconds, 0, 0.1)
	envelope.Level = followOrbInput(envelope.Level, inputs.Level, deltaSeconds, 14, 4)
	envelope.Bass = followOrbInput(envelope.Bass, inputs.Bass, deltaSeconds, 10, 3.2)
	envelope.Mid = followOrbInput(envelope.Mid, inputs.Mid, deltaSeconds, 13, 4.5)
	envelope.Treble = followOrbInput(envelope.Treble, inputs.Treble, deltaSeconds, 18, 7)
	inputs.Level, inputs.Bass, inputs.Mid, inputs.Treble = envelope.Level, envelope.Bass, envelope.Mid, envelope.Treble
}

func followOrbInput(current, target, deltaSeconds, attack, release float64) float64 {
	rate := release
	if target > current {
		rate = attack
	}
	return current + (target-current)*math.Min(1, rate*deltaSeconds)
}

func orbPaletteFor(inputs orbFrameInputs, elapsed float64) orbPalette {
	current, ok := orbPalettes[inputs.State]
	if !ok {
		current = orbPalettes["idle"]
	}
	previous, ok := orbPalettes[inputs.PreviousState]
	if !ok || inputs.ChangedAgeMs < 0 || inputs.ChangedAgeMs >= 700 {
		return current
	}
	amount := smoothOrb(float64(inputs.ChangedAgeMs) / 700)
	return orbPalette{
		rim: mixOrbColor(previous.rim, current.rim, amount), middle: mixOrbColor(previous.middle, current.middle, amount),
		core: mixOrbColor(previous.core, current.core, amount), glow: lerpOrb(previous.glow, current.glow, amount),
		spin: lerpOrb(previous.spin, current.spin, amount), calm: lerpOrb(previous.calm, current.calm, amount),
		speed: lerpOrb(previous.speed, current.speed, amount), pulse: lerpOrb(previous.pulse, current.pulse, amount),
		galaxy: lerpOrb(previous.galaxy, current.galaxy, amount),
	}
}

func orbGalaxyTheta(radiusRatio float64) float64 {
	if radiusRatio <= 1 {
		return 0
	}
	return math.Log(radiusRatio) / orbGalaxyPitch
}

func orbGalaxyTurnsBetween(innerRatio, outerRatio float64) float64 {
	if innerRatio <= 0 || outerRatio <= innerRatio {
		return 0
	}
	return (orbGalaxyTheta(outerRatio) - orbGalaxyTheta(innerRatio)) / (2 * math.Pi)
}

func orbFlashAmount(ageMs int64) float64 {
	if ageMs < 0 {
		return 0
	}
	return 1 - smoothOrb(float64(ageMs)/870)
}

func orbSurfaceNoise(x, y, z, time float64) (low, middle, high float64) {
	low = math.Sin(x*1.1+z*0.7+time) * math.Cos(y*0.9-z*0.6+time*0.7)
	middle = math.Sin(x*2.7+y*1.3-time*1.4) * math.Cos(y*2.1-z*1.8+time*0.8)
	high = math.Sin(x*6.4-y*3.1+time*2.2) * math.Cos(y*6.4+z*4.5-time*1.7)
	return
}

func clampOrb(value float64) float64 { return clampFloat(value, 0, 1) }

func clampFloat(value, low, high float64) float64 {
	return math.Max(low, math.Min(high, value))
}

func lerpOrb(a, b, amount float64) float64 {
	return a + (b-a)*amount
}

func orbPixelNoise(x, y int) float64 {
	n := uint32(x)*0x8da6b343 ^ uint32(y)*0xd8163841
	n ^= n >> 13
	n *= 0x85ebca6b
	n ^= n >> 16
	return float64(n&0x00ffffff) / 0x00ffffff
}

// orbWire draws a very light spherical mesh that follows the shell's curvature.
func orbWire(x, y float64) float64 {
	z := math.Sqrt(math.Max(0, 1-x*x-y*y))
	latitude := math.Asin(math.Max(-1, math.Min(1, y)))
	longitude := math.Atan2(x, z)
	lat := 1 - smoothOrb((math.Abs(math.Sin(latitude*8))-0.018)*170)
	lon := 1 - smoothOrb((math.Abs(math.Sin(longitude*6))-0.018)*170)
	return math.Max(lat, lon) * 0.18
}

type orbVector3 struct{ x, y, z float64 }

type orbNeuronPath struct {
	points []orbVector3
	axon   bool
}

type orbNeuronNode struct {
	point  orbVector3
	axon   bool
	branch bool
}

type orbNeuronGeometry struct {
	paths []orbNeuronPath
	nodes []orbNeuronNode
}

var (
	orbNeuronOnce         sync.Once
	orbNeuronGeometryData orbNeuronGeometry
)

// renderOrbNeuronNetwork paints the same dendrite, soma and axon structure as the web orb.
// Listening sends a gold pulse toward the soma, working activity gathers at the soma, and
// speaking sends a level-driven orange pulse outward along the axons.
func renderOrbNeuronNetwork(frame []byte, width, height int, sphereR float64, state string, level, elapsed float64) {
	inputs := orbFrameInputs{State: state, Level: level, StimulusAgeMs: -1, FlashAgeMs: -1}
	renderOrbNeuronNetworkWithInputs(frame, width, height, sphereR, inputs, elapsed)
}

func renderOrbNeuronNetworkWithInputs(frame []byte, width, height int, sphereR float64, inputs orbFrameInputs, elapsed float64) {
	if width <= 0 || height <= 0 || sphereR <= 0 || len(frame) < width*height*4 {
		return
	}
	orbNeuronOnce.Do(func() { orbNeuronGeometryData = buildOrbNeuronGeometry() })
	state, level := inputs.State, clampOrb(inputs.Level)
	cx, cy := float64(width-1)/2, float64(height-1)/2
	visibility := 0.35
	switch state {
	case "active":
		visibility = 0.72
	case "listening":
		visibility = 0.88
	case "working", "speaking":
		visibility = 1
	}
	rotY := elapsed * 0.105
	rotX := math.Sin(elapsed*0.17) * 0.14
	cosY, sinY := math.Cos(rotY), math.Sin(rotY)
	cosX, sinX := math.Cos(rotX), math.Sin(rotX)
	listenWave := 0.90 - fractOrb(elapsed*0.8*1.5)*0.82
	workingPulse := 0.35 + 0.65*math.Sin(math.Pi*fractOrb(elapsed*1.6*1.3))
	speakingWave := 0.06 + fractOrb(elapsed*0.95*2.4)*0.82
	voiceGlow := 0.32 + level*0.68
	flash := orbFlashAmount(inputs.FlashAgeMs)
	lineGlow := math.Max(1.0, sphereR*0.014)
	lineCore := math.Max(0.8, sphereR*0.008)

	project := func(point orbVector3) (x, y, depth float64) {
		rotatedX := point.x*cosY + point.z*sinY
		rotatedZ := -point.x*sinY + point.z*cosY
		rotatedY := point.y*cosX - rotatedZ*sinX
		depth = point.y*sinX + rotatedZ*cosX
		return cx + rotatedX*sphereR, cy + rotatedY*sphereR, depth
	}

	for _, path := range orbNeuronGeometryData.paths {
		baseColor := orbRGB{12, 206, 244}
		if path.axon {
			baseColor = orbRGB{36, 173, 255}
		}
		for i := 0; i+1 < len(path.points); i++ {
			a, b := path.points[i], path.points[i+1]
			x1, y1, d1 := project(a)
			x2, y2, d2 := project(b)
			depth := 0.2 + 0.8*smoothOrb((d1+d2+2)*0.25)
			radius := (a.magnitude() + b.magnitude()) * 0.5
			pulse, trail := 0.0, 0.0
			signalColor := orbRGB{255, 197, 83}
			if state == "listening" && !path.axon {
				pulse = math.Exp(-math.Pow((radius-listenWave)/0.055, 2)) * (0.4 + level*0.6)
				trail = math.Max(0, radius-listenWave) * 0.5 * (0.4 + level*0.6)
			}
			if inputs.StimulusAgeMs >= 0 && inputs.StimulusAgeMs < 850 && !path.axon {
				progress := float64(inputs.StimulusAgeMs) / 850
				wave := 0.90 - smoothOrb(progress)*0.82
				pulse = math.Max(pulse, math.Exp(-math.Pow((radius-wave)/0.06, 2)))
			}
			if state == "working" {
				pulse = math.Max(pulse, math.Exp(-radius*3.4)*workingPulse)
			}
			if (state == "speaking" || level > 0.28) && path.axon {
				pulse = math.Max(pulse, math.Exp(-math.Pow((radius-speakingWave)/0.05, 2))*voiceGlow)
				trail = math.Max(trail, math.Max(0, speakingWave-radius)*0.50*voiceGlow)
				signalColor = orbRGB{255, 122, 38}
			} else if state == "speaking" && !path.axon {
				pulse = math.Exp(-radius*4.2) * level * 0.42
			}
			if inputs.Waking {
				signalColor = orbRGB{122, 92, 255}
				pulse = math.Max(pulse, 0.36+0.28*math.Sin(elapsed*1.7+radius*8))
			}
			mix := math.Min(0.94, pulse*0.92+trail*0.38)
			color := mixOrbColor(baseColor, signalColor, mix)
			color = mixOrbColor(color, orbRGB{255, 47, 185}, flash*0.9)
			alpha := visibility * depth * (0.34 + pulse*0.52 + trail*0.34 + flash*0.3)
			if state == "working" {
				alpha = visibility * depth * (0.35 + pulse*0.46 + flash*0.3)
			}
			drawOrbNeuronSegment(frame, width, height, cx, cy, sphereR, x1, y1, x2, y2, color, alpha, lineGlow, lineCore)
		}
	}

	// Synaptic tips and branch points keep the fibers legible as a node network.
	for _, node := range orbNeuronGeometryData.nodes {
		x, y, depth := project(node.point)
		depthFactor := 0.2 + 0.8*smoothOrb((depth+1)*0.5)
		radial := node.point.magnitude()
		pulse := 0.0
		color := orbRGB{69, 225, 255}
		if state == "listening" && !node.axon {
			pulse = math.Exp(-math.Pow((radial-listenWave)/0.065, 2)) * (0.4 + level*0.6)
		} else if state == "working" {
			pulse = math.Exp(-radial*3.2) * workingPulse
		} else if (state == "speaking" || level > 0.28) && node.axon {
			pulse = math.Exp(-math.Pow((radial-speakingWave)/0.06, 2)) * voiceGlow
			color = orbRGB{255, 145, 54}
		}
		if inputs.StimulusAgeMs >= 0 && inputs.StimulusAgeMs < 850 && !node.axon {
			progress := float64(inputs.StimulusAgeMs) / 850
			wave := 0.90 - smoothOrb(progress)*0.82
			pulse = math.Max(pulse, math.Exp(-math.Pow((radial-wave)/0.065, 2)))
		}
		if inputs.Waking {
			color = orbRGB{122, 92, 255}
			pulse = math.Max(pulse, 0.38+0.25*math.Sin(elapsed*1.7+radial*8))
		}
		if node.branch {
			color = mixOrbColor(color, orbRGB{28, 183, 244}, 0.34)
		}
		color = mixOrbColor(color, orbRGB{255, 213, 126}, math.Min(0.9, pulse))
		color = mixOrbColor(color, orbRGB{255, 47, 185}, flash*0.9)
		size := 1.15
		if node.branch {
			size = 1.45
		}
		alpha := visibility * depthFactor * (0.52 + pulse*0.56 + flash*0.3)
		if state == "speaking" && node.axon {
			alpha *= 0.8 + level*0.45
		}
		drawOrbNeuronNode(frame, width, height, cx, cy, sphereR, x, y, color, alpha, size)
	}

	// Soma: a compact cell body with a cyan membrane and a state-driven inner glow.
	somaPulse := 0.0
	somaColor := orbRGB{29, 229, 255}
	if state == "listening" {
		somaPulse = 0.55 + 0.45*(0.5+0.5*math.Sin(elapsed*4))
		somaColor = mixOrbColor(somaColor, orbRGB{255, 193, 74}, somaPulse*0.8)
	} else if state == "working" {
		somaPulse = workingPulse
		somaColor = mixOrbColor(somaColor, orbRGB{255, 191, 72}, 0.84)
	} else if state == "speaking" {
		somaPulse = 0.2 + level*0.7
		somaColor = mixOrbColor(somaColor, orbRGB{255, 147, 66}, somaPulse*0.72)
	}
	if inputs.Waking {
		somaColor = orbRGB{122, 92, 255}
		somaPulse = 0.55 + 0.3*math.Sin(elapsed*1.7)
	}
	somaColor = mixOrbColor(somaColor, orbRGB{255, 47, 185}, flash)
	somaX, somaY, _ := project(orbVector3{})
	drawOrbNeuronNode(frame, width, height, cx, cy, sphereR, somaX, somaY, somaColor, 0.58+visibility*0.3+somaPulse*0.15+flash*0.2, math.Max(3.2, sphereR*0.055))
}

func buildOrbNeuronGeometry() orbNeuronGeometry {
	random := orbNeuronRandom{state: 0x9e3779b97f4a7c15}
	geometry := orbNeuronGeometry{}
	var branch func(start, direction orbVector3, length float64, level, maxLevel int, axon bool)
	branch = func(start, direction orbVector3, length float64, level, maxLevel int, axon bool) {
		segments := 6
		points := make([]orbVector3, 0, segments+1)
		points = append(points, start)
		at, heading := start, direction.unit()
		for i := 0; i < segments; i++ {
			jitter := random.unit().scale(0.16)
			if !axon {
				jitter = jitter.scale(1.65)
			}
			heading = heading.add(jitter).unit()
			at = at.add(heading.scale(length / float64(segments)))
			points = append(points, at)
		}
		geometry.paths = append(geometry.paths, orbNeuronPath{points: points, axon: axon})
		if level >= maxLevel {
			geometry.nodes = append(geometry.nodes, orbNeuronNode{point: at, axon: axon})
			return
		}
		children := 2
		if !axon && random.float64() > 0.36 {
			children = 3
		} else if axon && random.float64() > 0.68 {
			children = 1
		}
		geometry.nodes = append(geometry.nodes, orbNeuronNode{point: at, axon: axon, branch: true})
		for i := 0; i < children; i++ {
			spread := 0.7
			if axon {
				spread = 0.52
			}
			split := heading.add(random.unit().scale(spread)).unit()
			childLength := length * (0.58 + random.float64()*0.18)
			branch(at, split, childLength, level+1, maxLevel, axon)
		}
	}

	for i := 0; i < 14; i++ {
		direction := random.unit()
		branch(direction.scale(0.055), direction, 0.31+random.float64()*0.12, 0, 2, false)
	}
	for i := 0; i < 4; i++ {
		angle := float64(i)*math.Pi/2 + (random.float64()-0.5)*0.28
		direction := orbVector3{math.Cos(angle) * 0.23, math.Sin(angle) * 0.23, 0.94}.unit()
		branch(direction.scale(0.06), direction, 0.45+random.float64()*0.13, 0, 2, true)
	}

	maxRadius := 0.0
	for _, path := range geometry.paths {
		for _, point := range path.points {
			maxRadius = math.Max(maxRadius, point.magnitude())
		}
	}
	scalePoint := func(point orbVector3) orbVector3 {
		radius := point.magnitude()
		if radius == 0 || maxRadius == 0 {
			return point
		}
		k := maxRadius * 0.42
		fitted := 0.88 * math.Tanh(radius/k) / math.Tanh(maxRadius/k)
		return point.scale(fitted / radius)
	}
	for pathIndex := range geometry.paths {
		for pointIndex := range geometry.paths[pathIndex].points {
			geometry.paths[pathIndex].points[pointIndex] = scalePoint(geometry.paths[pathIndex].points[pointIndex])
		}
	}
	for nodeIndex := range geometry.nodes {
		geometry.nodes[nodeIndex].point = scalePoint(geometry.nodes[nodeIndex].point)
	}
	return geometry
}

type orbNeuronRandom struct{ state uint64 }

func (r *orbNeuronRandom) float64() float64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return float64(r.state>>11) / float64(uint64(1)<<53)
}

func (r *orbNeuronRandom) unit() orbVector3 {
	for {
		v := orbVector3{r.float64()*2 - 1, r.float64()*2 - 1, r.float64()*2 - 1}
		if v.magnitude() <= 1 {
			return v.unit()
		}
	}
}

func (v orbVector3) add(other orbVector3) orbVector3 {
	return orbVector3{v.x + other.x, v.y + other.y, v.z + other.z}
}

func (v orbVector3) scale(amount float64) orbVector3 {
	return orbVector3{v.x * amount, v.y * amount, v.z * amount}
}

func (v orbVector3) magnitude() float64 {
	return math.Sqrt(v.x*v.x + v.y*v.y + v.z*v.z)
}

func (v orbVector3) unit() orbVector3 {
	length := v.magnitude()
	if length == 0 {
		return orbVector3{z: 1}
	}
	return v.scale(1 / length)
}

func drawOrbNeuronSegment(frame []byte, width, height int, cx, cy, sphereR, x1, y1, x2, y2 float64, color orbRGB, alpha, glowRadius, coreRadius float64) {
	minX := max(0, int(math.Floor(math.Min(x1, x2)-glowRadius*2)))
	maxX := min(width-1, int(math.Ceil(math.Max(x1, x2)+glowRadius*2)))
	minY := max(0, int(math.Floor(math.Min(y1, y2)-glowRadius*2)))
	maxY := min(height-1, int(math.Ceil(math.Max(y1, y2)+glowRadius*2)))
	dx, dy := x2-x1, y2-y1
	lengthSq := dx*dx + dy*dy
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			px, py := float64(x)-cx, float64(y)-cy
			if px*px+py*py > sphereR*sphereR {
				continue
			}
			t := 0.0
			if lengthSq > 0 {
				t = math.Max(0, math.Min(1, ((float64(x)-x1)*dx+(float64(y)-y1)*dy)/lengthSq))
			}
			distance := math.Hypot(float64(x)-(x1+t*dx), float64(y)-(y1+t*dy))
			if distance > glowRadius*2 {
				continue
			}
			halo := 0.23 * math.Exp(-distance*distance/(2*glowRadius*glowRadius))
			core := math.Max(0, 1-distance/(coreRadius+0.7)) * 0.84
			coverage := math.Min(0.95, alpha*(halo+core))
			if coverage <= 0.002 {
				continue
			}
			pixelColor := mixOrbColor(color, orbRGB{220, 255, 255}, core*0.45)
			blendOrbPixel(frame, (y*width+x)*4, pixelColor, coverage)
		}
	}
}

func drawOrbNeuronNode(frame []byte, width, height int, cx, cy, sphereR, x, y float64, color orbRGB, alpha, radius float64) {
	minX := max(0, int(math.Floor(x-radius*3)))
	maxX := min(width-1, int(math.Ceil(x+radius*3)))
	minY := max(0, int(math.Floor(y-radius*3)))
	maxY := min(height-1, int(math.Ceil(y+radius*3)))
	for py := minY; py <= maxY; py++ {
		for px := minX; px <= maxX; px++ {
			localX, localY := float64(px)-cx, float64(py)-cy
			if localX*localX+localY*localY > sphereR*sphereR {
				continue
			}
			distance := math.Hypot(float64(px)-x, float64(py)-y)
			if distance > radius*3 {
				continue
			}
			halo := 0.3 * math.Exp(-distance*distance/(2*radius*radius))
			core := math.Max(0, 1-distance/(radius*0.72))
			coverage := math.Min(0.96, alpha*(halo+core*0.94))
			if coverage <= 0.002 {
				continue
			}
			pixelColor := mixOrbColor(color, orbRGB{235, 255, 255}, core*0.58)
			blendOrbPixel(frame, (py*width+px)*4, pixelColor, coverage)
		}
	}
}

func blendOrbPixel(frame []byte, index int, color orbRGB, alpha float64) {
	alpha = math.Max(0, math.Min(1, alpha))
	inverse := 1 - alpha
	frame[index+0] = byte(math.Round(math.Min(255, color.b)*alpha + float64(frame[index+0])*inverse))
	frame[index+1] = byte(math.Round(math.Min(255, color.g)*alpha + float64(frame[index+1])*inverse))
	frame[index+2] = byte(math.Round(math.Min(255, color.r)*alpha + float64(frame[index+2])*inverse))
	frame[index+3] = byte(math.Round(255*alpha + float64(frame[index+3])*inverse))
}

func mixOrbColor(a, b orbRGB, amount float64) orbRGB {
	amount = math.Max(0, math.Min(1, amount))
	return orbRGB{
		r: a.r + (b.r-a.r)*amount,
		g: a.g + (b.g-a.g)*amount,
		b: a.b + (b.b-a.b)*amount,
	}
}

func scaleOrbColor(c orbRGB, scale float64) orbRGB {
	return orbRGB{
		r: math.Min(255, c.r*scale),
		g: math.Min(255, c.g*scale),
		b: math.Min(255, c.b*scale),
	}
}

func wrapOrbAngle(value float64) float64 {
	wrapped := math.Mod(value+math.Pi, 2*math.Pi)
	if wrapped < 0 {
		wrapped += 2 * math.Pi
	}
	return wrapped - math.Pi
}

func fractOrb(value float64) float64 {
	return value - math.Floor(value)
}

func smoothOrb(value float64) float64 {
	value = math.Max(0, math.Min(1, value))
	return value * value * (3 - 2*value)
}
