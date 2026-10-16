package main

import (
	"math"
	"sync"
)

// The sphere at the heart of the orb, drawn the way the web orb (web/orb.js) draws it: a geodesic
// icosphere (radius 2, group scale 1.5) whose vertices are pushed out by 3D simplex noise driven by the
// voice, shaded as a translucent surface, outlined by its triangle edges (every edge, front and back),
// and wrapped in an additive halo seen from inside. The camera is the web one: 46 degrees vertically,
// 15 units back. Colours and formulas are the web shaders' own (SURFACE_FRAG, MESH_FRAG, HALO_FRAG), in
// 0-255 units, so what differs between the two is only what a CPU rasteriser cannot do the same way.

const (
	orbSphereDetail = 9 // three's IcosahedronGeometry(2, 9): 10 segments per edge
	orbCameraZ      = 15.0
	orbCameraFOV    = 46.0
	orbSphereRadius = 2.0
	orbHaloRadius   = 2.45
)

type orbMesh struct {
	verts []orbVector3 // unit vectors
	tris  [][3]int32
}

var (
	orbMeshOnce sync.Once
	orbSphere   orbMesh
)

func getOrbSphere() *orbMesh {
	orbMeshOnce.Do(func() { orbSphere = buildOrbIcosphere(orbSphereDetail) })
	return &orbSphere
}

// buildOrbIcosphere subdivides an icosahedron like three's PolyhedronGeometry: every face is cut into
// (detail+1)^2 triangles and every vertex pushed onto the sphere.
func buildOrbIcosphere(detail int) orbMesh {
	t := (1 + math.Sqrt(5)) / 2
	base := []orbVector3{
		{-1, t, 0}, {1, t, 0}, {-1, -t, 0}, {1, -t, 0},
		{0, -1, t}, {0, 1, t}, {0, -1, -t}, {0, 1, -t},
		{t, 0, -1}, {t, 0, 1}, {-t, 0, -1}, {-t, 0, 1},
	}
	faces := [][3]int{
		{0, 11, 5}, {0, 5, 1}, {0, 1, 7}, {0, 7, 10}, {0, 10, 11},
		{1, 5, 9}, {5, 11, 4}, {11, 10, 2}, {10, 7, 6}, {7, 1, 8},
		{3, 9, 4}, {3, 4, 2}, {3, 2, 6}, {3, 6, 8}, {3, 8, 9},
		{4, 9, 5}, {2, 4, 11}, {6, 2, 10}, {8, 6, 7}, {9, 8, 1},
	}
	n := detail + 1
	var mesh orbMesh
	index := map[[3]int32]int32{}
	add := func(v orbVector3) int32 {
		v = v.unit()
		key := [3]int32{int32(math.Round(v.x * 1e5)), int32(math.Round(v.y * 1e5)), int32(math.Round(v.z * 1e5))}
		if i, ok := index[key]; ok {
			return i
		}
		mesh.verts = append(mesh.verts, v)
		index[key] = int32(len(mesh.verts) - 1)
		return index[key]
	}
	lerp := func(a, b orbVector3, f float64) orbVector3 {
		return orbVector3{a.x + (b.x-a.x)*f, a.y + (b.y-a.y)*f, a.z + (b.z-a.z)*f}
	}
	for _, face := range faces {
		a, b, c := base[face[0]], base[face[1]], base[face[2]]
		rows := make([][]int32, n+1)
		for i := 0; i <= n; i++ {
			left, right := lerp(a, c, float64(i)/float64(n)), lerp(b, c, float64(i)/float64(n))
			count := n - i
			rows[i] = make([]int32, count+1)
			for j := 0; j <= count; j++ {
				if count == 0 {
					rows[i][j] = add(left)
				} else {
					rows[i][j] = add(lerp(left, right, float64(j)/float64(count)))
				}
			}
		}
		for i := 0; i < n; i++ {
			for j := 0; j < n-i; j++ {
				mesh.tris = append(mesh.tris, [3]int32{rows[i][j+1], rows[i+1][j], rows[i][j]})
				if j+1 < n-i {
					mesh.tris = append(mesh.tris, [3]int32{rows[i+1][j+1], rows[i+1][j], rows[i][j+1]})
				}
			}
		}
	}
	// Wind every triangle counter-clockwise seen from outside, whatever the grid order gave.
	for i, tri := range mesh.tris {
		a, b, c := mesh.verts[tri[0]], mesh.verts[tri[1]], mesh.verts[tri[2]]
		ab := orbVector3{b.x - a.x, b.y - a.y, b.z - a.z}
		ac := orbVector3{c.x - a.x, c.y - a.y, c.z - a.z}
		cross := orbVector3{ab.y*ac.z - ab.z*ac.y, ab.z*ac.x - ab.x*ac.z, ab.x*ac.y - ab.y*ac.x}
		if cross.x*(a.x+b.x+c.x)+cross.y*(a.y+b.y+c.y)+cross.z*(a.z+b.z+c.z) < 0 {
			mesh.tris[i] = [3]int32{tri[0], tri[2], tri[1]}
		}
	}
	return mesh
}

// --- simplex noise: Ian McEwan / Ashima Arts' snoise, the same function the web shader uses --------------

func orbMod289(x float64) float64  { return x - math.Floor(x*(1.0/289.0))*289.0 }
func orbPermute(x float64) float64 { return orbMod289(((x * 34.0) + 1.0) * x) }

func orbSimplex(vx, vy, vz float64) float64 {
	const cx, cy = 1.0 / 6.0, 1.0 / 3.0
	s := (vx + vy + vz) * cy
	ix, iy, iz := math.Floor(vx+s), math.Floor(vy+s), math.Floor(vz+s)
	t := (ix + iy + iz) * cx
	x0 := [3]float64{vx - ix + t, vy - iy + t, vz - iz + t}
	step := func(edge, x float64) float64 {
		if x < edge {
			return 0
		}
		return 1
	}
	g := [3]float64{step(x0[1], x0[0]), step(x0[2], x0[1]), step(x0[0], x0[2])}
	l := [3]float64{1 - g[0], 1 - g[1], 1 - g[2]}
	lzxy := [3]float64{l[2], l[0], l[1]}
	var i1, i2 [3]float64
	for k := 0; k < 3; k++ {
		i1[k], i2[k] = math.Min(g[k], lzxy[k]), math.Max(g[k], lzxy[k])
	}
	x1 := [3]float64{x0[0] - i1[0] + cx, x0[1] - i1[1] + cx, x0[2] - i1[2] + cx}
	x2 := [3]float64{x0[0] - i2[0] + cy, x0[1] - i2[1] + cy, x0[2] - i2[2] + cy}
	x3 := [3]float64{x0[0] - 0.5, x0[1] - 0.5, x0[2] - 0.5}
	ix, iy, iz = orbMod289(ix), orbMod289(iy), orbMod289(iz)
	zs := [4]float64{iz, iz + i1[2], iz + i2[2], iz + 1}
	ys := [4]float64{iy, iy + i1[1], iy + i2[1], iy + 1}
	xs := [4]float64{ix, ix + i1[0], ix + i2[0], ix + 1}
	var p [4]float64
	for k := 0; k < 4; k++ {
		p[k] = orbPermute(orbPermute(orbPermute(zs[k])+ys[k]) + xs[k])
	}
	// The shader hashes in 32-bit floats, and the result depends on it: at exact multiples of 7 and 49 a
	// float32 product rounds up where a float64 one rounds down, which in double precision sends the hash
	// out of range and gives a wild gradient (noise of +-4 instead of +-1). So these steps are float32.
	const n_ = 0.142857142857
	nsx, nsy, nsz := float32(n_*2.0), float32(n_*0.5-1.0), float32(n_*1.0)
	var xv, yv, h [4]float64
	for k := 0; k < 4; k++ {
		pk := float32(p[k])
		j := pk - 49.0*float32(math.Floor(float64(pk*nsz*nsz)))
		xk := float32(math.Floor(float64(j * nsz)))
		yk := float32(math.Floor(float64(j - 7.0*xk)))
		xv[k] = float64(xk*nsx + nsy)
		yv[k] = float64(yk*nsx + nsy)
		h[k] = 1.0 - math.Abs(xv[k]) - math.Abs(yv[k])
	}
	b0 := [4]float64{xv[0], xv[1], yv[0], yv[1]}
	b1 := [4]float64{xv[2], xv[3], yv[2], yv[3]}
	s0 := [4]float64{math.Floor(b0[0])*2 + 1, math.Floor(b0[1])*2 + 1, math.Floor(b0[2])*2 + 1, math.Floor(b0[3])*2 + 1}
	s1 := [4]float64{math.Floor(b1[0])*2 + 1, math.Floor(b1[1])*2 + 1, math.Floor(b1[2])*2 + 1, math.Floor(b1[3])*2 + 1}
	var sh [4]float64
	for k := 0; k < 4; k++ {
		if h[k] <= 0 {
			sh[k] = -1
		}
	}
	a0 := [4]float64{b0[0] + s0[0]*sh[0], b0[2] + s0[2]*sh[0], b0[1] + s0[1]*sh[1], b0[3] + s0[3]*sh[1]}
	a1 := [4]float64{b1[0] + s1[0]*sh[2], b1[2] + s1[2]*sh[2], b1[1] + s1[1]*sh[3], b1[3] + s1[3]*sh[3]}
	pv := [4][3]float64{{a0[0], a0[1], h[0]}, {a0[2], a0[3], h[1]}, {a1[0], a1[1], h[2]}, {a1[2], a1[3], h[3]}}
	xx := [4][3]float64{x0, x1, x2, x3}
	var dotP, dotX [4]float64
	for k := 0; k < 4; k++ {
		d := pv[k][0]*pv[k][0] + pv[k][1]*pv[k][1] + pv[k][2]*pv[k][2]
		norm := 1.79284291400159 - 0.85373472095314*d
		for c := 0; c < 3; c++ {
			pv[k][c] *= norm
		}
		dotP[k] = pv[k][0]*xx[k][0] + pv[k][1]*xx[k][1] + pv[k][2]*xx[k][2]
		dotX[k] = xx[k][0]*xx[k][0] + xx[k][1]*xx[k][1] + xx[k][2]*xx[k][2]
	}
	var sum float64
	for k := 0; k < 4; k++ {
		m := math.Max(0.6-dotX[k], 0)
		m *= m
		sum += m * m * dotP[k]
	}
	return 42.0 * sum
}

// --- the sphere in the scene -------------------------------------------------------------------------------

type orbSphereVertex struct {
	sx, sy, invDepth float64 // screen position and 1/depth
	nx, ny, nz       float64 // normal, rotated into view space
	vx, vy, vz       float64 // direction to the camera
	disp, warp, h    float64 // noise displacement, warp, and object-space height
	facing           bool    // set per triangle while rasterising
}

type orbSphereScene struct {
	width, height int
	focal         float64
	cx, cy        float64
	scale         float64 // group scale: 1.5 times the voice pulse
	palette       orbPalette
	level, flash  float64
	verts         []orbSphereVertex
	rotationX     float64
	rotationY     float64
	workers       int
}

func newOrbSphereScene(width, height int, palette orbPalette, inputs orbFrameInputs, level, flash, groupScale, elapsed float64) *orbSphereScene {
	scene := &orbSphereScene{
		width: width, height: height,
		focal: float64(height) / (2 * math.Tan(orbCameraFOV/2*math.Pi/180)),
		cx:    float64(width-1) / 2, cy: float64(height-1) / 2,
		scale: 1.5 * groupScale, palette: palette, level: level, flash: flash,
		rotationX: math.Sin(elapsed*0.21) * 0.12,
		rotationY: elapsed * palette.spin,
		workers:   inputs.Workers,
	}
	mesh := getOrbSphere()
	scene.verts = make([]orbSphereVertex, len(mesh.verts))
	t := elapsed * 0.35 * palette.speed
	rest := 0.05 + palette.calm*0.035
	sinX, cosX := math.Sincos(scene.rotationX)
	sinY, cosY := math.Sincos(scene.rotationY)
	rotate := func(v orbVector3) orbVector3 { // Euler XYZ: rotate about y first, then x
		y1, z1 := v.y, v.z
		x2, z2 := v.x*cosY+z1*sinY, -v.x*sinY+z1*cosY
		return orbVector3{x2, y1*cosX - z2*sinX, y1*sinX + z2*cosX}
	}
	for i, u := range mesh.verts {
		px, py, pz := u.x*orbSphereRadius, u.y*orbSphereRadius, u.z*orbSphereRadius
		low := orbSimplex(px*0.55, py*0.55, pz*0.55+t)
		mid := orbSimplex(px*1.35-t*1.4, py*1.35, pz*1.35)
		high := orbSimplex(px*3.2, py*3.2+t*2.2, pz*3.2)
		d := low*(rest+inputs.Bass*0.66) + mid*(0.018+inputs.Mid*0.36) + high*(inputs.Treble*0.17) + level*0.07
		pos := orbVector3{px + u.x*d, py + u.y*d, pz + u.z*d}
		world := rotate(pos)
		world = orbVector3{world.x * scene.scale, world.y * scene.scale, world.z * scene.scale}
		depth := orbCameraZ - world.z
		normal := rotate(u)
		view := orbVector3{-world.x, -world.y, -(world.z - orbCameraZ)}.unit()
		scene.verts[i] = orbSphereVertex{
			sx: scene.cx + world.x*scene.focal/depth, sy: scene.cy - world.y*scene.focal/depth, invDepth: 1 / depth,
			nx: normal.x, ny: normal.y, nz: normal.z, vx: view.x, vy: view.y, vz: view.z,
			disp: d, warp: low*0.5 + mid*0.3, h: u.y,
		}
	}
	return scene
}

// orbGradient is the shaders' stateGradient: rim colour over mid colour over core colour, blend position
// warped by noise so it flows like liquid.
func orbGradient(h, warp, level float64, rim, middle, core orbRGB) orbRGB {
	g := smoothOrbRange(-1, 1, h*0.9+warp*1.1+level*0.3)
	lower := mixOrbColor(core, middle, smoothOrbRange(0, 0.55, g))
	return mixOrbColor(lower, rim, smoothOrbRange(0.45, 1, g))
}

func smoothOrbRange(edge0, edge1, x float64) float64 {
	t := clampFloat((x-edge0)/(edge1-edge0), 0, 1)
	return t * t * (3 - 2*t)
}

var orbFlashPink = orbRGB{255, 46, 184} // vec3(1.0, 0.18, 0.72)

type orbFragment struct{ nx, ny, nz, vx, vy, vz, disp, warp, h float64 }

func (s *orbSphereScene) surfaceColor(f orbFragment) (orbRGB, float64) {
	nl := math.Sqrt(f.nx*f.nx + f.ny*f.ny + f.nz*f.nz)
	vl := math.Sqrt(f.vx*f.vx + f.vy*f.vy + f.vz*f.vz)
	facing := clampFloat((f.nx*f.vx+f.ny*f.vy+f.nz*f.vz)/(nl*vl), 0, 1)
	fres := orbPow22.at(1 - facing)
	col := orbGradient(f.h, f.warp*1.3+f.disp*1.8, s.level, s.palette.rim, s.palette.middle, s.palette.core)
	col = mixOrbColor(col, orbFlashPink, s.flash*0.55)
	light := 0.55 + 0.45*facing
	heart := orbPow22.at(facing)
	glow := mixOrbColor(s.palette.middle, s.palette.rim, 0.5)
	col = orbRGB{col.r + glow.r*heart*0.55, col.g + glow.g*heart*0.55, col.b + glow.b*heart*0.55}
	a := (0.34 + fres*0.35 + heart*0.22 + s.level*0.2) * s.palette.glow
	scale := light * (0.85 + s.level*0.7)
	out := orbRGB{math.Min(col.r*scale, 242), math.Min(col.g*scale, 242), math.Min(col.b*scale, 242)}
	return out, clampFloat(a, 0, 0.9)
}

// surfaceBase is the surface shader's gradient colour for a point (before lighting). It changes slowly
// across a triangle, so the renderer evaluates it at the vertices and blends it between them.
func (s *orbSphereScene) surfaceBase(h, warp, disp float64) orbRGB {
	col := orbGradient(h, warp*1.3+disp*1.8, s.level, s.palette.rim, s.palette.middle, s.palette.core)
	return mixOrbColor(col, orbFlashPink, s.flash*0.55)
}

// surfaceShade is the rest of the surface shader: lighting, the bright core and the alpha.
func (s *orbSphereScene) surfaceShade(base orbRGB, nx, ny, nz, vx, vy, vz float64) (orbRGB, float64) {
	nl := math.Sqrt(nx*nx + ny*ny + nz*nz)
	vl := math.Sqrt(vx*vx + vy*vy + vz*vz)
	facing := clampFloat((nx*vx+ny*vy+nz*vz)/(nl*vl), 0, 1)
	fres := orbPow22.at(1 - facing)
	light := 0.55 + 0.45*facing
	heart := orbPow22.at(facing)
	glow := mixOrbColor(s.palette.middle, s.palette.rim, 0.5)
	col := orbRGB{base.r + glow.r*heart*0.55, base.g + glow.g*heart*0.55, base.b + glow.b*heart*0.55}
	a := (0.34 + fres*0.35 + heart*0.22 + s.level*0.2) * s.palette.glow
	scale := light * (0.85 + s.level*0.7)
	return orbRGB{min(col.r*scale, 242), min(col.g*scale, 242), min(col.b*scale, 242)}, clampFloat(a, 0, 0.9)
}

func (s *orbSphereScene) meshColor(f orbFragment) (orbRGB, float64) {
	nl := math.Sqrt(f.nx*f.nx + f.ny*f.ny + f.nz*f.nz)
	vl := math.Sqrt(f.vx*f.vx + f.vy*f.vy + f.vz*f.vz)
	fres := orbPow17.at(1 - clampFloat((f.nx*f.vx+f.ny*f.vy+f.nz*f.vz)/(nl*vl), 0, 1))
	col := orbGradient(f.h, f.warp+f.disp*1.4, s.level, s.palette.rim, s.palette.middle, s.palette.core)
	col = mixOrbColor(col, orbFlashPink, s.flash*0.75)
	rim := orbRGB{s.palette.rim.r * 1.3, s.palette.rim.g * 1.3, s.palette.rim.b * 1.3}
	col = mixOrbColor(col, rim, fres*0.45)
	a := (0.16 + fres*0.7) * s.palette.glow * (0.75 + s.level*0.9)
	k := 0.65 + s.level*1.1
	return orbRGB{col.r * k, col.g * k, col.b * k}, clampFloat(a, 0, 1)
}

func orbFragmentOf(a, b, c *orbSphereVertex, wa, wb, wc float64) orbFragment {
	// perspective-correct: weights are already divided by depth and renormalised by the caller
	return orbFragment{
		nx: a.nx*wa + b.nx*wb + c.nx*wc, ny: a.ny*wa + b.ny*wb + c.ny*wc, nz: a.nz*wa + b.nz*wb + c.nz*wc,
		vx: a.vx*wa + b.vx*wb + c.vx*wc, vy: a.vy*wa + b.vy*wb + c.vy*wc, vz: a.vz*wa + b.vz*wb + c.vz*wc,
		disp: a.disp*wa + b.disp*wb + c.disp*wc, warp: a.warp*wa + b.warp*wb + c.warp*wc, h: a.h*wa + b.h*wb + c.h*wc,
	}
}
