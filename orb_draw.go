package main

import (
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

// The orb is drawn on the CPU, so the drawing is spread over a few cores in horizontal bands. Every
// layer works on whole bands of rows and never touches a row outside its band, so workers share the one
// frame without locks and each pixel still receives its layers in the same order as a single thread would
// give it. Cost per frame is what decides how smooth the desktop widget looks: it shows source frames
// blended at the display rate, and a slow renderer turns shimmer into smear.

const orbBandRows = 16

// orbWorkers is how many cores one frame may use. Half the machine's threads, at most six: the widget is
// always on, and must not take over the computer.
// orbWorkerOverride, when positive, fixes the worker count (tests use it to compare one against many).
var orbWorkerOverride int

func orbWorkers(requested int) int {
	if orbWorkerOverride > 0 {
		return orbWorkerOverride
	}
	if requested > 0 {
		return min(requested, 12)
	}
	n := runtime.NumCPU() / 2
	return min(max(n, 1), 6)
}

// orbParallelRows calls fn on bands of rows until all of [0, height) is done.
func orbParallelRows(height, requested int, fn func(y0, y1 int)) {
	bands := (height + orbBandRows - 1) / orbBandRows
	workers := min(orbWorkers(requested), bands)
	if workers < 2 {
		fn(0, height)
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				band := int(next.Add(1)) - 1
				if band >= bands {
					return
				}
				fn(band*orbBandRows, min((band+1)*orbBandRows, height))
			}
		}()
	}
	wg.Wait()
}

// orbParallelRange splits [0, n) into chunks for the workers.
func orbParallelRange(n, requested int, fn func(from, to int)) {
	workers := orbWorkers(requested)
	if workers < 2 || n < 4096 {
		fn(0, n)
		return
	}
	chunk := (n + workers*4 - 1) / (workers * 4)
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				from := int(next.Add(int64(chunk))) - chunk
				if from >= n {
					return
				}
				fn(from, min(from+chunk, n))
			}
		}()
	}
	wg.Wait()
}

// --- cheap powers -----------------------------------------------------------------------------------------

// orbPowTable is x^exponent on [0, 1] by linear interpolation: the shaders raise a value in that range
// to a fixed power for every pixel, and math.Pow is the slowest thing the renderer does.
type orbPowTable [1025]float32

func newOrbPowTable(exponent float64) *orbPowTable {
	var t orbPowTable
	for i := range t {
		t[i] = float32(math.Pow(float64(i)/1024, exponent))
	}
	return &t
}

func (t *orbPowTable) at(x float64) float64 {
	if !(x > 0) {
		return 0
	}
	if x >= 1 {
		return 1
	}
	p := x * 1024
	i := int(p)
	f := p - float64(i)
	return float64(t[i])*(1-f) + float64(t[i+1])*f
}

// orbSpriteFalloff is a galaxy point's brightness against u, its squared distance from the centre over the
// squared radius: smoothstep(0.5, 0, d)^2 as the web shader has it, where d is the distance in sprite sizes.
// With d = sqrt(u)/2 that is (smoothstep of 1-sqrt(u))^2.
type orbSpriteTable [1025]float32

func (t *orbSpriteTable) at(u float64) float64 {
	p := u * 1024
	i := int(p)
	if i >= 1024 {
		return 0
	}
	f := p - float64(i)
	return float64(t[i])*(1-f) + float64(t[i+1])*f
}

var orbSpriteFalloff = func() *orbSpriteTable {
	var t orbSpriteTable
	for i := range t {
		u := float64(i) / 1024
		v := 1 - math.Sqrt(u)
		soft := v * v * (3 - 2*v)
		t[i] = float32(soft * soft)
	}
	return &t
}()

// orbGaussian is exp(-u) for u in [0, 8]: the neuron's glow falls off as exp(-d^2 / 2r^2).
type orbGaussianTable [1025]float32

func (t *orbGaussianTable) at(u float64) float64 {
	if !(u > 0) {
		return 1
	}
	if u >= 8 {
		return 0
	}
	p := u * 128
	i := int(p)
	f := p - float64(i)
	return float64(t[i])*(1-f) + float64(t[i+1])*f
}

var orbGaussian = func() *orbGaussianTable {
	var t orbGaussianTable
	for i := range t {
		t[i] = float32(math.Exp(-float64(i) / 128))
	}
	return &t
}()

var (
	orbPow17 = newOrbPowTable(1.7)
	orbPow22 = newOrbPowTable(2.2)
)

// --- the sphere ---------------------------------------------------------------------------------------------

type orbTriangle struct {
	a, b, c        *orbSphereVertex
	area           float64
	x0, x1, y0, y1 int
	ia, ib, ic     int // vertex numbers, to find each vertex's gradient colour
}

// drawSurface paints the translucent body: every front-facing triangle, alpha-blended in index order,
// with no depth buffer (the web material does not write depth either).
func (s *orbSphereScene) drawSurface(frame []byte) {
	mesh := getOrbSphere()
	tris := make([]orbTriangle, 0, len(mesh.tris)/2+64)
	for _, tri := range mesh.tris {
		a, b, c := &s.verts[tri[0]], &s.verts[tri[1]], &s.verts[tri[2]]
		area := (b.sx-a.sx)*(c.sy-a.sy) - (c.sx-a.sx)*(b.sy-a.sy)
		if area >= 0 { // screen y points down, so a counter-clockwise (outward) face has negative area
			continue
		}
		tris = append(tris, orbTriangle{a: a, b: b, c: c, area: area,
			x0: int(math.Floor(min(a.sx, b.sx, c.sx))), x1: int(math.Ceil(max(a.sx, b.sx, c.sx))),
			y0: int(math.Floor(min(a.sy, b.sy, c.sy))), y1: int(math.Ceil(max(a.sy, b.sy, c.sy)))})
	}
	bases := make([]orbRGB, len(s.verts))
	for i := range s.verts {
		v := &s.verts[i]
		bases[i] = s.surfaceBase(v.h, v.warp, v.disp)
	}
	base0 := &s.verts[0]
	index := func(v *orbSphereVertex) int {
		return int((uintptr(unsafe.Pointer(v)) - uintptr(unsafe.Pointer(base0))) / unsafe.Sizeof(*v))
	}
	for i := range tris {
		tris[i].ia, tris[i].ib, tris[i].ic = index(tris[i].a), index(tris[i].b), index(tris[i].c)
	}
	orbParallelRows(s.height, s.workers, func(rowFrom, rowTo int) {
		for i := range tris {
			t := &tris[i]
			if t.y1 < rowFrom || t.y0 >= rowTo {
				continue
			}
			a, b, c := t.a, t.b, t.c
			for y := max(t.y0, rowFrom); y <= min(t.y1, rowTo-1); y++ {
				py := float64(y)
				for x := max(t.x0, 0); x <= min(t.x1, s.width-1); x++ {
					px := float64(x)
					w0 := ((b.sx-px)*(c.sy-py) - (c.sx-px)*(b.sy-py)) / t.area
					w1 := ((c.sx-px)*(a.sy-py) - (a.sx-px)*(c.sy-py)) / t.area
					w2 := 1 - w0 - w1
					if w0 < -1e-9 || w1 < -1e-9 || w2 < -1e-9 {
						continue
					}
					pa, pb, pc := w0*a.invDepth, w1*b.invDepth, w2*c.invDepth
					sum := pa + pb + pc
					pa, pb, pc = pa/sum, pb/sum, pc/sum
					ba, bb, bc := bases[t.ia], bases[t.ib], bases[t.ic]
					base := orbRGB{ba.r*pa + bb.r*pb + bc.r*pc, ba.g*pa + bb.g*pb + bc.g*pc, ba.b*pa + bb.b*pb + bc.b*pc}
					color, alpha := s.surfaceShade(base,
						a.nx*pa+b.nx*pb+c.nx*pc, a.ny*pa+b.ny*pb+c.ny*pc, a.nz*pa+b.nz*pb+c.nz*pc,
						a.vx*pa+b.vx*pb+c.vx*pc, a.vy*pa+b.vy*pb+c.vy*pc, a.vz*pa+b.vz*pb+c.vz*pc)
					blendOrbPixel(frame, (y*s.width+x)*4, color, alpha)
				}
			}
		}
	})
}

type orbMeshVertex struct {
	color orbRGB
	alpha float64
}

type orbLine struct {
	a, b   *orbSphereVertex
	ca, cb *orbMeshVertex
	y0, y1 float64
}

// drawWire outlines every triangle edge, back ones included (a wireframe material draws all of them),
// once per triangle, so shared edges are drawn twice as they are on the web.
func (s *orbSphereScene) drawWire(frame []byte) {
	mesh := getOrbSphere()
	// The mesh shader's colour changes slowly along an edge, so it is worked out once at each vertex and
	// blended along the line instead of at every pixel of it.
	colors := make([]orbMeshVertex, len(s.verts))
	for i := range s.verts {
		v := &s.verts[i]
		colors[i].color, colors[i].alpha = s.meshColor(orbFragment{v.nx, v.ny, v.nz, v.vx, v.vy, v.vz, v.disp, v.warp, v.h})
	}
	lines := make([]orbLine, 0, len(mesh.tris)*3)
	for _, tri := range mesh.tris {
		for e := 0; e < 3; e++ {
			ia, ib := tri[e], tri[(e+1)%3]
			a, b := &s.verts[ia], &s.verts[ib]
			lines = append(lines, orbLine{a: a, b: b, ca: &colors[ia], cb: &colors[ib], y0: min(a.sy, b.sy) - 1, y1: max(a.sy, b.sy) + 1})
		}
	}
	orbParallelRows(s.height, s.workers, func(rowFrom, rowTo int) {
		for i := range lines {
			l := &lines[i]
			if l.y1 < float64(rowFrom) || l.y0 >= float64(rowTo) {
				continue
			}
			s.drawLine(frame, l, rowFrom, rowTo)
		}
	})
}

// drawLine is an anti-aliased (Wu) line whose fragments take the mesh shader's colour. Only pixels in
// rows [rowFrom, rowTo) are written.
func (s *orbSphereScene) drawLine(frame []byte, l *orbLine, rowFrom, rowTo int) {
	a, b := l.a, l.b
	dx, dy := b.sx-a.sx, b.sy-a.sy
	steps := int(math.Ceil(math.Max(math.Abs(dx), math.Abs(dy))))
	if steps < 1 {
		return
	}
	steep := math.Abs(dy) > math.Abs(dx)
	for i := 0; i <= steps; i++ {
		f := float64(i) / float64(steps)
		x, y := a.sx+dx*f, a.sy+dy*f
		var near, far int
		var cover float64
		if steep {
			near, far, cover = int(math.Floor(x)), int(math.Round(y)), x-math.Floor(x)
			if far < rowFrom || far >= rowTo {
				continue
			}
		} else {
			near, far, cover = int(math.Floor(y)), int(math.Round(x)), y-math.Floor(y)
			if near+1 < rowFrom || near >= rowTo {
				continue
			}
		}
		color := orbRGB{l.ca.color.r + (l.cb.color.r-l.ca.color.r)*f, l.ca.color.g + (l.cb.color.g-l.ca.color.g)*f, l.ca.color.b + (l.cb.color.b-l.ca.color.b)*f}
		alpha := l.ca.alpha + (l.cb.alpha-l.ca.alpha)*f
		if steep {
			s.plot(frame, near, far, rowFrom, rowTo, color, alpha*(1-cover))
			s.plot(frame, near+1, far, rowFrom, rowTo, color, alpha*cover)
		} else {
			s.plot(frame, far, near, rowFrom, rowTo, color, alpha*(1-cover))
			s.plot(frame, far, near+1, rowFrom, rowTo, color, alpha*cover)
		}
	}
}

func (s *orbSphereScene) plot(frame []byte, x, y, rowFrom, rowTo int, color orbRGB, alpha float64) {
	if x < 0 || x >= s.width || y < rowFrom || y >= rowTo || alpha <= 0.003 {
		return
	}
	blendOrbPixel(frame, (y*s.width+x)*4, color, alpha)
}

// drawHalo adds the halo: a sphere of radius 2.45 (times the group scale and 1+0.16*level) seen from
// inside, so only its far side shows, brightest where it meets the silhouette. The camera looks straight
// at the sphere, so the halo depends only on a pixel's distance from the centre: it is worked out once
// along a radius, and every pixel just looks its distance up.
func (s *orbSphereScene) drawHalo(frame []byte, bass float64) {
	radius := orbHaloRadius * s.scale * (1 + s.level*0.16)
	reach := radius * s.focal / math.Sqrt(orbCameraZ*orbCameraZ-radius*radius) // silhouette radius in pixels
	const steps = 512
	var table [steps + 2]float32
	for i := 0; i <= steps; i++ {
		rho := reach * float64(i) / steps
		dirX, dirY := rho/s.focal, 0.0
		inv := 1 / math.Sqrt(dirX*dirX+dirY*dirY+1)
		dirX, dirZ := dirX*inv, -inv
		b := orbCameraZ * -dirZ
		disc := b*b - (orbCameraZ*orbCameraZ - radius*radius)
		if disc <= 0 {
			continue
		}
		t := b + math.Sqrt(disc) // the far hit (b is minus the dot of origin and direction): the back face is the one drawn
		hx, hz := dirX*t, orbCameraZ+dirZ*t
		nx, nz := hx/radius, hz/radius
		vx, vz := -hx, orbCameraZ-hz
		vl := math.Sqrt(vx*vx + vz*vz)
		facing := clampFloat(math.Abs(nx*vx/vl+nz*vz/vl), 0, 1)
		fres := math.Pow(1-facing, 3.4)
		table[i] = float32(clampFloat(fres*(0.3+s.level*1.5+s.flash*1.8)*s.palette.glow, 0, 1))
	}
	rimColor := orbRGB{s.palette.rim.r * (1 + s.level), s.palette.rim.g * (1 + s.level), s.palette.rim.b * (1 + s.level)}
	color := mixOrbColor(rimColor, orbFlashPink, s.flash)
	x0, x1 := max(int(s.cx-reach)-1, 0), min(int(s.cx+reach)+1, s.width-1)
	y0, y1 := max(int(s.cy-reach)-1, 0), min(int(s.cy+reach)+1, s.height-1)
	scale := steps / reach
	// Towards the middle the halo is too faint to show; start where it first reaches a visible level.
	inner := 0.0
	for i := 0; i <= steps; i++ {
		if table[i] > 0.002 {
			inner = float64(max(i-1, 0))
			break
		}
	}
	innerSq := (inner / scale) * (inner / scale)
	orbParallelRows(s.height, s.workers, func(rowFrom, rowTo int) {
		for y := max(y0, rowFrom); y <= min(y1, rowTo-1); y++ {
			dy := float64(y) - s.cy
			for x := x0; x <= x1; x++ {
				dx := float64(x) - s.cx
				d2 := dx*dx + dy*dy
				if d2 < innerSq {
					continue
				}
				p := math.Sqrt(d2) * scale
				if p >= steps {
					continue
				}
				i := int(p)
				f := p - float64(i)
				a := float64(table[i])*(1-f) + float64(table[i+1])*f
				if a > 0.002 {
					addOrbPixel(frame, (y*s.width+x)*4, color, a)
				}
			}
		}
	})
}
