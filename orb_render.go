package main

import (
	"math"
	"sync"
	"time"
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

// orbPaletteHex keeps the palettes as written (sRGB hex); orbPalettes is what is drawn with.
var orbPaletteHex = func() map[string]orbPalette {
	copied := make(map[string]orbPalette, len(orbPalettes))
	for state, palette := range orbPalettes {
		copied[state] = palette
	}
	return copied
}()

// orbPaletteGamma is the exponent the palette colours are raised to (1 draws the hex codes as they are,
// 2.2 treats them as sRGB and works in linear light). It is a measured value: see TestOrbPaletteGamma.
var orbPaletteGamma = 2.2

func init() { rebuildOrbPalettes() }

func rebuildOrbPalettes() {
	for state, palette := range orbPaletteHex {
		palette.rim, palette.middle, palette.core = linearOrb(palette.rim), linearOrb(palette.middle), linearOrb(palette.core)
		orbPalettes[state] = palette
	}
}

// linearOrb converts an sRGB colour to the linear values three.js hands the web orb's shaders. Those
// shaders write the linear value straight to the screen, which is why the web orb's colours are deeper
// than the same hex codes; doing the same here keeps the two the same colour.
func linearOrb(c orbRGB) orbRGB {
	f := func(v float64) float64 { return math.Pow(clampFloat(v/255, 0, 1), orbPaletteGamma) * 255 }
	return orbRGB{f(c.r), f(c.g), f(c.b)}
}

// stylePalette applies the user's colour for a state: the three shades are derived from the one colour
// they pick, the same way the stock palettes step from rim to core.
func stylePalette(base orbPalette, c orbRGB) orbPalette {
	middle := linearOrb(c)
	base.middle = middle
	base.rim = mixOrbColor(middle, orbRGB{255, 255, 255}, 0.45)
	base.core = mixOrbColor(middle, orbRGB{4, 6, 18}, 0.55)
	return base
}

// orbSphereHeightFraction is the sphere's silhouette radius over the frame height: radius 3 (2 * the
// group scale 1.5) seen from 15 units away through a 46 degree vertical field of view.
var orbSphereHeightFraction = 3 / math.Sqrt(15*15-3*3) / (2 * math.Tan(23*math.Pi/180))

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
	// PixelRatio is the display scale (1 at 96 dpi): the web orb sizes its points in device pixels
	// times this, so the desktop frame does the same. Zero means 1.
	PixelRatio float64
	// Workers is how many threads may draw this frame; 0 picks a default. A calm orb should use one,
	// because short parallel phases cost more in waking threads than they save.
	Workers int
	Style   orbStyle
}

// orbWantsFullRate says whether the orb is doing something worth drawing at full speed: voice in it, a
// pulse or flash passing through, a change of state, or the model at work. A calm orb is drawn slowly,
// because every frame is drawn on the CPU and the widget is always on the desktop.
func orbWantsFullRate(in orbFrameInputs) bool {
	switch in.State {
	case "working", "speaking", "listening":
		return true
	}
	return in.Waking || in.Level > 0.02 || in.Bass > 0.02 || in.Mid > 0.02 || in.Treble > 0.02 ||
		(in.StimulusAgeMs >= 0 && in.StimulusAgeMs < 2000) || (in.FlashAgeMs >= 0 && in.FlashAgeMs < 2000) ||
		(in.ChangedAgeMs >= 0 && in.ChangedAgeMs < 1200)
}

// orbCalmInterval is the time between frames while the orb is calm. With the spiral on it is longer: 160,000
// particles cost far more than the sphere, and their arms barely move when nothing is happening.
func orbCalmInterval(spiral bool) time.Duration {
	if spiral {
		return time.Second / 24
	}
	return time.Second / 20
}

// orbWorkersFor is how many threads a frame gets. A calm orb is drawn at 20 frames a second, so one
// thread is enough and wastes nothing; a busy one needs 60, which the plain sphere does on two and the
// 160,000-particle spiral on four. More threads than that only burn CPU waking each other.
func orbWorkersFor(busy, spiral bool) int {
	switch {
	case busy && spiral:
		return 4
	case busy || spiral:
		return 2
	}
	return 1
}

// orbStyle is what the user can change from the Settings page. The zero value is the stock look.
type orbStyle struct {
	NoSpiral   bool               // hide the spiral arms
	SpiralSize float64            // 1 is stock; 0 means 1
	Colors     map[string]orbRGB  // per state, in sRGB 0-255: replaces that state's colours
}

func (st orbStyle) spiralSize() float64 {
	if st.SpiralSize <= 0 {
		return 1
	}
	return clampFloat(st.SpiralSize, 0.2, 2)
}

type orbGalaxyParticle struct {
	radius, angle, lift, star, outward, colorMix, dust float64
	baseSin, baseCos                                   float64
	rotationRate                                       float64
	weight                                             float32 // how much light the point adds, before it is dimmed by depth
	rank                                               float32 // a fixed random order, so the same points are always the ones dropped
	rotationBin, swellBin                              uint16
	arm                                                uint8
	isDust                                             bool
}

// orbGalaxyDetail is the least share of faint one-pixel points that is still drawn (1 draws every one,
// as the web page does). orbGalaxyLODKnee is the brightness-times-alpha above which a point is always kept.
var (
	orbGalaxyDetail  = 0.35
	orbGalaxyLODKnee = 0.9
)

// orbSoftSprite is the point size (pixels) from which a galaxy point is drawn in 2x2 blocks.
const orbSoftSprite = 4.0

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
	return renderOrbFrameInto(make([]byte, width*height*4), width, height, inputs, elapsed)
}

// renderOrbFrameInto draws a frame into frame (width*height*4 bytes, any contents) and returns it.
func renderOrbFrameInto(frame []byte, width, height int, inputs orbFrameInputs, elapsed float64) []byte {
	if width <= 0 || height <= 0 || len(frame) != width*height*4 {
		return nil
	}
	clear(frame)
	palette := orbPaletteFor(inputs, elapsed)
	inputs.Level = clampOrb(inputs.Level)
	inputs.Bass = clampOrb(inputs.Bass)
	inputs.Mid = clampOrb(inputs.Mid)
	inputs.Treble = clampOrb(inputs.Treble)
	minSide := float64(min(width, height))
	breath := 0.03 + 0.02*math.Sin(elapsed*1.3)
	displayLevel := math.Min(1, inputs.Level+breath*(1-math.Min(inputs.Level*4, 1)))
	groupPulse := 1 + displayLevel*0.07 + palette.pulse*math.Sin(elapsed*2.2)
	flash := orbFlashAmount(inputs.FlashAgeMs)

	// The same layers, in the same order, as the web scene (web/orb.js): the translucent body, the neuron
	// inside it, the triangle mesh, the halo, and last the galaxy, which glows on top of all of it.
	scene := newOrbSphereScene(width, height, palette, inputs, displayLevel, flash, groupPulse, elapsed)
	scene.drawSurface(frame)
	sphereR := float64(height) * orbSphereHeightFraction * groupPulse
	renderOrbNeuronNetworkWithInputs(frame, width, height, sphereR, inputs, elapsed)
	scene.drawWire(frame)
	scene.drawHalo(frame, inputs.Bass)
	if !inputs.Style.NoSpiral {
		renderOrbGalaxyParticles(frame, width, height, minSide, palette, inputs, displayLevel, elapsed)
	}
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
		particle := orbGalaxyParticle{star: star, colorMix: fractOrb(r3 * 7.1), weight: float32((0.32 + 0.6*star) * (0.7 + 1.6*star)), rank: float32(fractOrb(r1*977.3 + r2*131.7 + r3*17.9))}
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

// orbSprite is one galaxy particle once it has been placed on the screen.
type orbSprite struct {
	x, y, size float32 // centre and diameter in pixels
	r, g, b, a float32 // colour (0-255) and alpha
	ok         bool
}

var orbSpritePool = sync.Pool{New: func() any {
	sprites := make([]orbSprite, orbGalaxyParticleCount)
	return &sprites
}}

// renderOrbGalaxyParticles places all 160,000 particles in parallel, then draws them in row bands.
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
	pixelRatio := inputs.PixelRatio
	if pixelRatio <= 0 {
		pixelRatio = 1
	}
	galaxyScale := orbGalaxyScale * inputs.Style.spiralSize()
	var rotations [orbGalaxyRotationBins]orbRotationPair
	for i := range rotations {
		rate := orbGalaxyMaxRotationRate * float64(i) / (orbGalaxyRotationBins - 1)
		rotations[i].sin, rotations[i].cos = math.Sincos(galaxyTime*rate + groupDrift)
	}
	swellOffset := int(math.Mod(elapsed*3/(2*math.Pi)*orbGalaxySineBins, orbGalaxySineBins))
	brightnessBase := 0.45 + palette.glow*0.7 + displayLevel*0.4
	sizeBase := 0.4 * displayLevel
	width64, height64 := float64(width), float64(height)

	spritesPtr := orbSpritePool.Get().(*[]orbSprite)
	defer orbSpritePool.Put(spritesPtr)
	sprites := *spritesPtr
	orbParallelRange(len(orbGalaxyParticles), inputs.Workers, func(from, to int) {
		for i := from; i < to; i++ {
			particle := &orbGalaxyParticles[i]
			// Screen-space-error culling: a faint point adds almost nothing to the picture, so only a share of
			// the faint ones is drawn, each brighter by the same share to keep the total light. The share is
			// fixed per point, so the same points always go and nothing flickers.
			keep := 1.0
			if orbGalaxyDetail < 1 {
				keep = math.Max(orbGalaxyDetail, min(1, float64(particle.weight)/orbGalaxyLODKnee))
				if float64(particle.rank) >= keep {
					sprites[i].ok = false
					continue
				}
			}
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
			viewDepth := 15 - rotatedZ*galaxyScale
			if viewDepth <= 1 {
				sprites[i].ok = false
				continue
			}
			projection := focalLength / viewDepth * galaxyScale
			pixelX := centerX + screenX*projection
			pixelY := centerY - screenY*projection
			if pixelX < -8 || pixelX > width64+8 || pixelY < -8 || pixelY > height64+8 {
				sprites[i].ok = false // off the frame, as it would be off the web canvas
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
			brightness := (0.7 + particle.star*1.6) * brightnessBase * boost
			r, g, b := min(color.r*brightness, 230), min(color.g*brightness, 230), min(color.b*brightness, 230)
			diameter := pixelRatio * (0.8 + 2.2*particle.star + sizeBase) * (22 / viewDepth)
			alpha := 0.32 + 0.6*particle.star
			alpha = min(1, alpha/keep)
			sprites[i] = orbSprite{x: float32(pixelX), y: float32(pixelY), size: float32(max(diameter, 1)),
				r: float32(r), g: float32(g), b: float32(b), a: float32(alpha), ok: true}
		}
	})
	// Sort the sprites into the bands they touch, so each band's worker visits only its own.
	bands := (height + orbBandRows - 1) / orbBandRows
	span := func(sp *orbSprite) (first, last int, visible bool) {
		half := float64(sp.size) / 2
		lo, hi := int(math.Floor(float64(sp.y)-half))-1, int(math.Ceil(float64(sp.y)+half))+1
		if hi < 0 || lo > height-1 {
			return 0, 0, false
		}
		return max(lo, 0) / orbBandRows, min(hi, height-1) / orbBandRows, true
	}
	starts := make([]int32, bands+1)
	for i := range sprites {
		if !sprites[i].ok {
			continue
		}
		if first, last, visible := span(&sprites[i]); visible {
			for b := first; b <= last; b++ {
				starts[b+1]++
			}
		}
	}
	for b := 0; b < bands; b++ {
		starts[b+1] += starts[b]
	}
	items := make([]int32, starts[bands])
	fill := append([]int32(nil), starts[:bands]...)
	for i := range sprites {
		if !sprites[i].ok {
			continue
		}
		if first, last, visible := span(&sprites[i]); visible {
			for b := first; b <= last; b++ {
				items[fill[b]] = int32(i)
				fill[b]++
			}
		}
	}
	orbParallelRows(height, inputs.Workers, func(rowFrom, rowTo int) {
		for band := rowFrom / orbBandRows; band*orbBandRows < rowTo && band < bands; band++ {
			from, to := max(band*orbBandRows, rowFrom), min((band+1)*orbBandRows, rowTo)
			for _, index := range items[starts[band]:starts[band+1]] {
				drawOrbSprite(frame, width, from, to, &sprites[index])
			}
		}
	})
}

// drawOrbSprite draws one galaxy point the way WebGL draws a gl_POINTS sprite: a square of "size" pixels
// (never under one pixel), covering the pixels whose centres fall inside it, each with the web fragment
// shader's alpha, smoothstep(0.5, 0, distance)^2, and ADDED to what is there. Only rows
// [rowFrom, rowTo) are written, so bands can be drawn at the same time.
func drawOrbSprite(frame []byte, width, rowFrom, rowTo int, sp *orbSprite) {
	x, y, size := float64(sp.x), float64(sp.y), float64(sp.size)
	if size <= 1.0001 {
		// By far the most common case: a one-pixel point. Only the pixel it sits in can be lit, and only
		// when its centre is within half a pixel of the point's. (x and y are never below -8.)
		px, py := int(x+16.5)-16, int(y+16.5)-16
		if py < rowFrom || py >= rowTo || px < 0 || px >= width {
			return
		}
		dx, dy := float64(px)-x, float64(py)-y
		u := (dx*dx + dy*dy) * 4 // squared distance over the squared radius (0.5)
		if u >= 1 {
			return
		}
		addSpritePixel(frame, (py*width+px)*4, sp, float32(orbSpriteFalloff.at(u))*sp.a)
		return
	}
	half := size / 2
	inverseSq := 1 / (half * half)
	x0, x1 := int(math.Ceil(x-half)), int(math.Floor(x+half))
	y0, y1 := int(math.Ceil(y-half)), int(math.Floor(y+half))
	if size >= orbSoftSprite {
		// A big point is close to the camera and soft, so it gets the cheaper "level of detail": its glow is
		// worked out once for each 2x2 block of pixels instead of once for every pixel. Blocks start on even
		// rows and bands are an even number of rows, so a block never crosses into another band's rows.
		for by := max(y0, rowFrom) &^ 1; by <= min(y1, rowTo-1); by += 2 {
			dy := float64(by) + 0.5 - y
			dySq := dy * dy
			for bx := max(x0, 0) &^ 1; bx <= min(x1, width-1); bx += 2 {
				dx := float64(bx) + 0.5 - x
				u := (dx*dx + dySq) * inverseSq
				if u >= 1 {
					continue
				}
				alpha := float32(orbSpriteFalloff.at(u)) * sp.a
				for py := max(by, rowFrom); py <= min(by+1, rowTo-1); py++ {
					for px := bx; px <= min(bx+1, width-1); px++ {
						addSpritePixel(frame, (py*width+px)*4, sp, alpha)
					}
				}
			}
		}
		return
	}
	for py := max(y0, rowFrom); py <= min(y1, rowTo-1); py++ {
		dy := float64(py) - y
		dySq := dy * dy
		for px := max(x0, 0); px <= min(x1, width-1); px++ {
			dx := float64(px) - x
			u := (dx*dx + dySq) * inverseSq
			if u >= 1 {
				continue
			}
			addSpritePixel(frame, (py*width+px)*4, sp, float32(orbSpriteFalloff.at(u))*sp.a)
		}
	}
}

// addSpritePixel adds a sprite's colour, scaled by alpha, to a pixel (additive blending).
func addSpritePixel(frame []byte, index int, sp *orbSprite, alpha float32) {
	if !(alpha > 0.0005) {
		return
	}
	frame[index+0] = addScaled(frame[index+0], sp.b*alpha)
	frame[index+1] = addScaled(frame[index+1], sp.g*alpha)
	frame[index+2] = addScaled(frame[index+2], sp.r*alpha)
	frame[index+3] = addScaled(frame[index+3], 255*alpha)
}

func addScaled(have byte, add float32) byte {
	sum := int(have) + int(add+0.5)
	if sum > 255 {
		return 255
	}
	return byte(sum)
}

// addOrbPixel is additive blending (SRC_ALPHA, ONE) into the premultiplied frame: glow adds to what is
// behind it, on the desktop as it does on the web page.
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
	paletteOf := func(state string) (orbPalette, bool) {
		palette, ok := orbPalettes[state]
		if ok {
			if c, styled := inputs.Style.Colors[state]; styled {
				palette = stylePalette(palette, c)
			}
		}
		return palette, ok
	}
	current, ok := paletteOf(inputs.State)
	if !ok {
		current, _ = paletteOf("idle")
	}
	previous, ok := paletteOf(inputs.PreviousState)
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
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
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
	// Thinking is the web neuron's "soma merge" phase: a gold burn where the fibres meet the core that swells
	// and fades about twice a second (progress runs at 1.6*1.3 per second, with a 0.06 s rest between
	// bursts), strongest within 0.08 of the radius and gone by 0.4.
	const thinkingRate = 1.6 * 1.3
	thinkingProgress := math.Min(1, math.Mod(elapsed, 1/thinkingRate+0.06)*thinkingRate)
	workingPulse := math.Sin(thinkingProgress * math.Pi)
	thinkingBurn := func(radius float64) float64 { return workingPulse * (1 - smoothOrbRange(0.08, 0.4, radius)) }
	// After thinking or speaking the neuron settles through a violet refractory afterglow (phase 4, which
	// takes 2 s at 0.5 per second).
	afterglow := 0.0
	if (inputs.PreviousState == "working" || inputs.PreviousState == "speaking") && state != "working" && state != "speaking" &&
		!inputs.Waking && inputs.ChangedAgeMs >= 0 && inputs.ChangedAgeMs < 2000 {
		afterglow = 1 - float64(inputs.ChangedAgeMs)/2000
	}
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
				pulse = math.Max(pulse, thinkingBurn(radius))
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
			} else if afterglow > 0 {
				signalColor = orbRGB{122, 92, 255}
				pulse = math.Max(pulse, afterglow*0.85)
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
			pulse = thinkingBurn(radial)
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
		} else if afterglow > 0 {
			color = orbRGB{122, 92, 255}
			pulse = math.Max(pulse, afterglow*0.85)
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
		somaColor = mixOrbColor(somaColor, orbRGB{255, 194, 74}, workingPulse*0.92) // the gold glow swells and fades; the membrane stays cyan
	} else if state == "speaking" {
		somaPulse = 0.2 + level*0.7
		somaColor = mixOrbColor(somaColor, orbRGB{255, 147, 66}, somaPulse*0.72)
	}
	if inputs.Waking {
		somaColor = orbRGB{122, 92, 255}
		somaPulse = 0.55 + 0.3*math.Sin(elapsed*1.7)
	} else if afterglow > 0 {
		somaColor = mixOrbColor(somaColor, orbRGB{122, 92, 255}, afterglow)
		somaPulse = afterglow * 0.6
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
	minX := max(0, int(math.Floor(min(x1, x2)-glowRadius*2)))
	maxX := min(width-1, int(math.Ceil(max(x1, x2)+glowRadius*2)))
	minY := max(0, int(math.Floor(min(y1, y2)-glowRadius*2)))
	maxY := min(height-1, int(math.Ceil(max(y1, y2)+glowRadius*2)))
	dx, dy := x2-x1, y2-y1
	lengthSq := dx*dx + dy*dy
	reach := glowRadius * 2
	reachSq := reach * reach
	sphereSq := sphereR * sphereR
	haloScale := 1 / (2 * glowRadius * glowRadius)
	coreSpan := coreRadius + 0.7
	for y := minY; y <= maxY; y++ {
		py := float64(y) - cy
		for x := minX; x <= maxX; x++ {
			px := float64(x) - cx
			if px*px+py*py > sphereSq {
				continue
			}
			t := 0.0
			if lengthSq > 0 {
				t = ((float64(x)-x1)*dx + (float64(y)-y1)*dy) / lengthSq
				if t < 0 {
					t = 0
				} else if t > 1 {
					t = 1
				}
			}
			ex, ey := float64(x)-(x1+t*dx), float64(y)-(y1+t*dy)
			distanceSq := ex*ex + ey*ey
			if distanceSq > reachSq {
				continue
			}
			distance := math.Sqrt(distanceSq)
			halo := 0.23 * orbGaussian.at(distanceSq*haloScale)
			core := 1 - distance/coreSpan
			if core < 0 {
				core = 0
			}
			core *= 0.84
			coverage := min(0.95, alpha*(halo+core))
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
	sphereSq := sphereR * sphereR
	reachSq := radius * 3 * radius * 3
	haloScale := 1 / (2 * radius * radius)
	for py := minY; py <= maxY; py++ {
		localY := float64(py) - cy
		for px := minX; px <= maxX; px++ {
			localX := float64(px) - cx
			if localX*localX+localY*localY > sphereSq {
				continue
			}
			ex, ey := float64(px)-x, float64(py)-y
			distanceSq := ex*ex + ey*ey
			if distanceSq > reachSq {
				continue
			}
			distance := math.Sqrt(distanceSq)
			halo := 0.3 * orbGaussian.at(distanceSq*haloScale)
			core := 1 - distance/(radius*0.72)
			if core < 0 {
				core = 0
			}
			coverage := min(0.96, alpha*(halo+core*0.94))
			if coverage <= 0.002 {
				continue
			}
			pixelColor := mixOrbColor(color, orbRGB{235, 255, 255}, core*0.58)
			blendOrbPixel(frame, (py*width+px)*4, pixelColor, coverage)
		}
	}
}

func addOrbPixel(frame []byte, index int, color orbRGB, alpha float64) {
	if !(alpha > 0) { // also rejects NaN, which would otherwise be written as a black pixel
		return
	}
	frame[index+0] = addByte(frame[index+0], color.b, alpha)
	frame[index+1] = addByte(frame[index+1], color.g, alpha)
	frame[index+2] = addByte(frame[index+2], color.r, alpha)
	frame[index+3] = addByte(frame[index+3], 255, alpha)
}

func addByte(have byte, value, alpha float64) byte {
	if value > 255 {
		value = 255
	}
	sum := float64(have) + value*alpha + 0.5
	if sum >= 255 {
		return 255
	}
	return byte(sum)
}

func blendOrbPixel(frame []byte, index int, color orbRGB, alpha float64) {
	if alpha != alpha {
		return // NaN: leave the pixel alone rather than blacken it
	}
	if alpha <= 0 {
		return
	}
	if alpha > 1 {
		alpha = 1
	}
	inverse := 1 - alpha
	frame[index+0] = blendByte(frame[index+0], color.b, alpha, inverse)
	frame[index+1] = blendByte(frame[index+1], color.g, alpha, inverse)
	frame[index+2] = blendByte(frame[index+2], color.r, alpha, inverse)
	frame[index+3] = blendByte(frame[index+3], 255, alpha, inverse)
}

func blendByte(have byte, value, alpha, inverse float64) byte {
	if value > 255 {
		value = 255
	}
	sum := value*alpha + float64(have)*inverse + 0.5
	if sum >= 255 {
		return 255
	}
	if sum < 0 {
		return 0
	}
	return byte(sum)
}

func mixOrbColor(a, b orbRGB, amount float64) orbRGB {
	if amount < 0 {
		amount = 0
	} else if amount > 1 {
		amount = 1
	}
	return orbRGB{
		r: a.r + (b.r-a.r)*amount,
		g: a.g + (b.g-a.g)*amount,
		b: a.b + (b.b-a.b)*amount,
	}
}

func scaleOrbColor(c orbRGB, scale float64) orbRGB {
	return orbRGB{
		r: min(255, c.r*scale),
		g: min(255, c.g*scale),
		b: min(255, c.b*scale),
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
	if value < 0 {
		value = 0
	} else if value > 1 {
		value = 1
	}
	return value * value * (3 - 2*value)
}
