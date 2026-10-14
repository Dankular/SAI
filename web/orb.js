// Audio-reactive sphere at the heart of a spiral galaxy, for the dashboard.
//
// The sphere reacts only to the assistant's own voice: callers feed it the
// level (0..1) of the audio the assistant is speaking via setLevel(). It never
// listens to the microphone. setState() changes colour and motion.
//
// Technique: a wireframe icosahedron whose vertices are displaced by 3D simplex
// noise scaled by the audio level, a fresnel-lit shell, and an additive halo.
// The simplex noise is Ian McEwan / Ashima Arts' public implementation (MIT).
import * as THREE from "/vendor/three.module.min.js";
import { createNeuron } from "/neuron.js";

const NOISE = /* glsl */ `
vec3 mod289(vec3 x){return x-floor(x*(1.0/289.0))*289.0;}
vec4 mod289(vec4 x){return x-floor(x*(1.0/289.0))*289.0;}
vec4 permute(vec4 x){return mod289(((x*34.0)+1.0)*x);}
vec4 taylorInvSqrt(vec4 r){return 1.79284291400159-0.85373472095314*r;}
float snoise(vec3 v){
  const vec2 C=vec2(1.0/6.0,1.0/3.0);
  const vec4 D=vec4(0.0,0.5,1.0,2.0);
  vec3 i=floor(v+dot(v,C.yyy));
  vec3 x0=v-i+dot(i,C.xxx);
  vec3 g=step(x0.yzx,x0.xyz);
  vec3 l=1.0-g;
  vec3 i1=min(g.xyz,l.zxy);
  vec3 i2=max(g.xyz,l.zxy);
  vec3 x1=x0-i1+C.xxx;
  vec3 x2=x0-i2+C.yyy;
  vec3 x3=x0-D.yyy;
  i=mod289(i);
  vec4 p=permute(permute(permute(i.z+vec4(0.0,i1.z,i2.z,1.0))+i.y+vec4(0.0,i1.y,i2.y,1.0))+i.x+vec4(0.0,i1.x,i2.x,1.0));
  float n_=0.142857142857;
  vec3 ns=n_*D.wyz-D.xzx;
  vec4 j=p-49.0*floor(p*ns.z*ns.z);
  vec4 x_=floor(j*ns.z);
  vec4 y_=floor(j-7.0*x_);
  vec4 x=x_*ns.x+ns.yyyy;
  vec4 y=y_*ns.x+ns.yyyy;
  vec4 h=1.0-abs(x)-abs(y);
  vec4 b0=vec4(x.xy,y.xy);
  vec4 b1=vec4(x.zw,y.zw);
  vec4 s0=floor(b0)*2.0+1.0;
  vec4 s1=floor(b1)*2.0+1.0;
  vec4 sh=-step(h,vec4(0.0));
  vec4 a0=b0.xzyw+s0.xzyw*sh.xxyy;
  vec4 a1=b1.xzyw+s1.xzyw*sh.zzww;
  vec3 p0=vec3(a0.xy,h.x);
  vec3 p1=vec3(a0.zw,h.y);
  vec3 p2=vec3(a1.xy,h.z);
  vec3 p3=vec3(a1.zw,h.w);
  vec4 norm=taylorInvSqrt(vec4(dot(p0,p0),dot(p1,p1),dot(p2,p2),dot(p3,p3)));
  p0*=norm.x;p1*=norm.y;p2*=norm.z;p3*=norm.w;
  vec4 m=max(0.6-vec4(dot(x0,x0),dot(x1,x1),dot(x2,x2),dot(x3,x3)),0.0);
  m=m*m;
  return 42.0*dot(m*m,vec4(dot(p0,x0),dot(p1,x1),dot(p2,x2),dot(p3,x3)));
}`;

const MESH_VERT = /* glsl */ `
uniform float uTime;
uniform float uLevel;
uniform float uBass;
uniform float uMid;
uniform float uTreble;
uniform float uCalm;
uniform float uSpeed;
varying vec3 vNormal;
varying vec3 vView;
varying float vDisp;
varying float vHeight;
varying float vWarp;
${NOISE}
void main(){
  float t=uTime*0.35*uSpeed;
  // Three noise octaves, each owned by a part of the voice: bass moves big slow
  // lobes, mids the body of the surface, treble the fine ripple of consonants.
  float low =snoise(position*0.55+vec3(0.0,0.0,t));
  float mid =snoise(position*1.35-vec3(t*1.4,0.0,0.0));
  float high=snoise(position*3.2+vec3(0.0,t*2.2,0.0));
  float rest=0.05+uCalm*0.035;
  float d=low*(rest+uBass*0.66)+mid*(0.018+uMid*0.36)+high*(uTreble*0.17)+uLevel*0.07;
  vec3 pos=position+normal*d;
  vDisp=d;
  vWarp=low*0.5+mid*0.3;
  vHeight=normalize(position).y;
  vNormal=normalize(normalMatrix*normal);
  vec4 mv=modelViewMatrix*vec4(pos,1.0);
  vView=normalize(-mv.xyz);
  gl_Position=projectionMatrix*mv;
}`;

// Shared gradient: rim colour A over mid colour C over core colour B, with the
// blend position warped by noise so it flows like a liquid and not a stripe.
const GRADIENT = /* glsl */ `
vec3 stateGradient(float h,float warp,float level,vec3 A,vec3 C,vec3 B){
  float g=smoothstep(-1.0,1.0,h*0.9+warp*1.1+level*0.3);
  vec3 lower=mix(B,C,smoothstep(0.0,0.55,g));
  return mix(lower,A,smoothstep(0.45,1.0,g));
}`;

const MESH_FRAG = /* glsl */ `
uniform vec3 uColorA;
uniform vec3 uColorB;
uniform vec3 uColorC;
uniform float uLevel;
uniform float uGlow;
uniform float uFlash;
varying vec3 vNormal;
varying vec3 vView;
varying float vDisp;
varying float vHeight;
varying float vWarp;
${GRADIENT}
void main(){
  float fres=pow(1.0-max(dot(normalize(vNormal),normalize(vView)),0.0),1.7);
  vec3 col=stateGradient(vHeight,vWarp+vDisp*1.4,uLevel,uColorA,uColorC,uColorB);
  col=mix(col,vec3(1.0,0.18,0.72),uFlash*0.75);
  col=mix(col,uColorA*1.3,fres*0.45);
  float a=(0.16+fres*0.7)*uGlow*(0.75+uLevel*0.9);
  gl_FragColor=vec4(col*(0.65+uLevel*1.1),clamp(a,0.0,1.0));
}`;

// Soft translucent body under the wireframe: the natural-looking gradient.
const SURFACE_FRAG = /* glsl */ `
uniform vec3 uColorA;
uniform vec3 uColorB;
uniform vec3 uColorC;
uniform float uLevel;
uniform float uGlow;
uniform float uFlash;
varying vec3 vNormal;
varying vec3 vView;
varying float vDisp;
varying float vHeight;
varying float vWarp;
${GRADIENT}
void main(){
  float facing=max(dot(normalize(vNormal),normalize(vView)),0.0);
  float fres=pow(1.0-facing,2.2);
  vec3 col=stateGradient(vHeight,vWarp*1.3+vDisp*1.8,uLevel,uColorA,uColorC,uColorB);
  col=mix(col,vec3(1.0,0.18,0.72),uFlash*0.55);
  float light=0.55+0.45*facing;
  float heart=pow(facing,2.2);
  col+=mix(uColorC,uColorA,0.5)*heart*0.55;
  float a=(0.34+fres*0.35+heart*0.22+uLevel*0.2)*uGlow;
  gl_FragColor=vec4(min(col*light*(0.85+uLevel*0.7),vec3(0.95)),clamp(a,0.0,0.9));
}`;

const HALO_VERT = /* glsl */ `
uniform float uLevel;
varying vec3 vNormal;
varying vec3 vView;
void main(){
  vec3 pos=position*(1.0+uLevel*0.16);
  vNormal=normalize(normalMatrix*normal);
  vec4 mv=modelViewMatrix*vec4(pos,1.0);
  vView=normalize(-mv.xyz);
  gl_Position=projectionMatrix*mv;
}`;

const HALO_FRAG = /* glsl */ `
uniform vec3 uColorA;
uniform float uLevel;
uniform float uGlow;
uniform float uFlash;
varying vec3 vNormal;
varying vec3 vView;
void main(){
  float facing=abs(dot(normalize(vNormal),normalize(vView)));
  float fres=pow(1.0-facing,3.4);
  float a=fres*(0.3+uLevel*1.5+uFlash*1.8)*uGlow;
  gl_FragColor=vec4(mix(uColorA*(1.0+uLevel),vec3(1.0,0.2,0.75),uFlash),clamp(a,0.0,1.0));
}`;


// Spiral galaxy: every particle is placed on the GPU from three random numbers
// (bulge, or one of four logarithmic arms with differential rotation). Its
// colours come from the same gradient as the sphere, so the whole scene shows
// the assistant's state; bass from the assistant's voice makes it swell.
const GALAXY_VERT = /* glsl */ `
uniform float uTime;
uniform float uLevel;
uniform float uBass;
uniform float uGalaxySpeed;
uniform float uGalaxyGlow;
uniform float uCore;
uniform float uPixel;
uniform vec3 uColorA;
uniform vec3 uColorB;
uniform vec3 uColorC;
attribute vec3 aRand;
varying vec3 vColor;
varying float vAlpha;
#define TAU 6.28318530718
void main(){
  float t=uTime*0.12*uGalaxySpeed;
  vec3 r=aRand;
  vec3 pos;
  vec3 col;
  float boost=1.0;
  float star=pow(fract(r.x*13.7+r.z*5.3),5.0);
  float core=uCore;
  if(r.x<0.05){
    // Faint dust ring hugging the sphere, so the arms seem to grow out of it.
    float ang=r.y*TAU;
    float rad=core*(0.95+0.5*pow(r.z,1.5));
    float tilt=(fract(r.y*53.1)-0.5)*0.55*core*0.1;
    pos=vec3(rad*cos(ang-t*0.8),tilt,rad*sin(ang-t*0.8));
    col=uColorA;
    boost=0.55;
  }else{
    // Logarithmic spiral that starts at the sphere's surface and sweeps outward.
    float armId=floor(fract(r.x*37.0)*4.0);
    float theta=r.y*TAU*1.15;
    float radius=core*exp(0.19*theta);
    float spread=(r.z-0.5)*2.0;
    spread=sign(spread)*pow(abs(spread),1.6);
    radius+=spread*radius*0.09;
    float angle=theta+armId*TAU*0.25-t*(9.0/sqrt(radius));
    float lift=(fract(r.y*91.7+r.z*3.1)-0.5)*2.0;
    float y=lift*exp(-(radius-core)*0.05)*0.9;
    pos=vec3(radius*cos(angle),y,radius*sin(angle));
    float out01=smoothstep(core,core*4.0,radius);
    float dust=pow(abs(spread),1.3);
    // Colour flows from the sphere's rim colour out to the dim, dusty tips.
    vec3 outer=mix(uColorC,uColorB,fract(r.z*7.1+armId*0.2));
    col=mix(uColorA,outer,smoothstep(0.0,0.75,out01));
    col=mix(col,vec3(0.02,0.0,0.12),dust*0.55);
    boost=mix(1.15,0.7,out01);
  }
  float swell=1.0+uBass*0.05*sin(length(pos)*0.55-uTime*3.0);
  pos*=swell;
  vec4 mv=modelViewMatrix*vec4(pos,1.0);
  gl_Position=projectionMatrix*mv;
  gl_PointSize=uPixel*(0.8+2.2*star+0.4*uLevel)*(22.0/max(-mv.z,1.0));
  vColor=min(col*(0.7+star*1.6)*(0.45+uGalaxyGlow*0.7+uLevel*0.4)*boost,vec3(0.9));
  vAlpha=0.32+0.6*star;
}`;

const GALAXY_FRAG = /* glsl */ `
varying vec3 vColor;
varying float vAlpha;
void main(){
  float d=length(gl_PointCoord-vec2(0.5));
  float a=smoothstep(0.5,0.0,d);
  gl_FragColor=vec4(vColor,a*a*vAlpha);
}`;

// state -> look. a = rim colour, c = middle, b = core (0xRRGGBB).
// Colours cross-fade when the state changes, and each shades as a gradient.
//   idle       assistant asleep / nothing running
//   active     awake and ready
//   listening  taking in the user's speech (a state only: it never follows the mic level)
//   working    the model is running inference
//   speaking   the assistant's voice is playing (driven by that audio's level)
export const ORB_STATES = {
  idle:      { a: 0x93a6d0, c: 0x5f739f, b: 0x2f3d63, glow: 0.62, spin: 0.02, calm: 0.0, speed: 0.5, pulse: 0.0, galaxy: 0.12 },
  active:    { a: 0x52e8d4, c: 0x38b6e8, b: 0x2563eb, glow: 0.9,  spin: 0.30, calm: 0.4, speed: 1.0, pulse: 0.0, galaxy: 1.0 },
  listening: { a: 0x8dffb8, c: 0x3fd6a0, b: 0x0e9f8e, glow: 1.0,  spin: 0.20, calm: 0.8, speed: 0.8, pulse: 0.035, galaxy: 0.7 },
  working:   { a: 0xff8bd8, c: 0xb05cf0, b: 0x5b3df5, glow: 1.05, spin: 0.42, calm: 1.0, speed: 1.6, pulse: 0.0, galaxy: 1.5 },
  speaking:  { a: 0xcaf8ff, c: 0x86c6ff, b: 0x3f7bff, glow: 1.25, spin: 0.60, calm: 0.2, speed: 1.9, pulse: 0.0, galaxy: 2.1 },
};
const STATES = ORB_STATES;

export function createOrb(container) {
  let renderer;
  try {
    renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true, powerPreference: "high-performance" });
  } catch (error) {
    console.warn("WebGL unavailable; using the CSS fallback orb.", error);
    container.classList.add("orb-fallback");
    return fallbackOrb(container);
  }
  renderer.setPixelRatio(Math.min(window.devicePixelRatio || 1, 2));
  renderer.setClearColor(0x000000, 0);
  container.append(renderer.domElement);
  renderer.domElement.setAttribute("aria-hidden", "true");

  const scene = new THREE.Scene();
  const camera = new THREE.PerspectiveCamera(46, 1, 0.1, 200);
  camera.position.set(0, 0, 15);

  const group = new THREE.Group();
  group.scale.setScalar(1.5);
  scene.add(group);

  // The galaxy lives in its own tilted group behind the sphere.
  const galaxyGroup = new THREE.Group();
  galaxyGroup.rotation.x = 1.0;
  galaxyGroup.rotation.z = -0.22;
  galaxyGroup.scale.setScalar(0.52);
  scene.add(galaxyGroup);

  const uniforms = {
    uTime: { value: 0 },
    uLevel: { value: 0 },
    uBass: { value: 0 },
    uMid: { value: 0 },
    uTreble: { value: 0 },
    uCalm: { value: 0.4 },
    uSpeed: { value: 1.0 },
    uGlow: { value: 0.85 },
    uFlash: { value: 0 },
    uColorA: { value: new THREE.Color(STATES.idle.a) },
    uColorB: { value: new THREE.Color(STATES.idle.b) },
    uColorC: { value: new THREE.Color(STATES.idle.c) },
  };

  const surface = new THREE.Mesh(
    new THREE.IcosahedronGeometry(2, 9),
    new THREE.ShaderMaterial({
      uniforms, vertexShader: MESH_VERT, fragmentShader: SURFACE_FRAG,
      transparent: true, depthWrite: false,
    }),
  );
  group.add(surface);

  // The neuron inside the sphere; it fires with the assistant's work and flashes pink on a reply.
  const neuron = createNeuron({ pixelRatio: Math.min(window.devicePixelRatio || 1, 2) });
  group.add(neuron.group);

  const mesh = new THREE.Mesh(
    new THREE.IcosahedronGeometry(2, 9),
    new THREE.ShaderMaterial({
      uniforms, vertexShader: MESH_VERT, fragmentShader: MESH_FRAG,
      wireframe: true, transparent: true, depthWrite: false,
    }),
  );
  group.add(mesh);

  const halo = new THREE.Mesh(
    new THREE.SphereGeometry(2.45, 48, 48),
    new THREE.ShaderMaterial({
      uniforms, vertexShader: HALO_VERT, fragmentShader: HALO_FRAG,
      transparent: true, side: THREE.BackSide, blending: THREE.AdditiveBlending, depthWrite: false,
    }),
  );
  group.add(halo);

  const COUNT = 160000;
  const galaxyGeometry = new THREE.BufferGeometry();
  const rand = new Float32Array(COUNT * 3);
  for (let i = 0; i < rand.length; i++) rand[i] = Math.random();
  galaxyGeometry.setAttribute("position", new THREE.BufferAttribute(new Float32Array(COUNT * 3), 3));
  galaxyGeometry.setAttribute("aRand", new THREE.BufferAttribute(rand, 3));
  const galaxyUniforms = {
    uTime: uniforms.uTime, uLevel: uniforms.uLevel, uBass: uniforms.uBass,
    uColorA: uniforms.uColorA, uColorB: uniforms.uColorB, uColorC: uniforms.uColorC,
    uGalaxySpeed: { value: 1 }, uGalaxyGlow: { value: 0.9 },
    uCore: { value: 5.4 }, // sphere radius in the galaxy's own units; arms start here
    uPixel: { value: Math.min(window.devicePixelRatio || 1, 2) },
  };
  const galaxy = new THREE.Points(
    galaxyGeometry,
    new THREE.ShaderMaterial({
      uniforms: galaxyUniforms, vertexShader: GALAXY_VERT, fragmentShader: GALAXY_FRAG,
      transparent: true, depthWrite: false, blending: THREE.AdditiveBlending,
    }),
  );
  galaxy.frustumCulled = false;
  galaxyGroup.add(galaxy);

  let target = { ...STATES.idle };
  const colorA = new THREE.Color(STATES.idle.a);
  const colorB = new THREE.Color(STATES.idle.b);
  const colorC = new THREE.Color(STATES.idle.c);
  const targetA = new THREE.Color(STATES.idle.a);
  const targetB = new THREE.Color(STATES.idle.b);
  const targetC = new THREE.Color(STATES.idle.c);
  const bands = { bass: 0, mid: 0, treble: 0 };
  const bandsInput = { bass: 0, mid: 0, treble: 0 };
  let spin = STATES.idle.spin;
  let pulse = 0;
  let level = 0;
  let levelInput = 0;
  let visible = true;
  let raf = 0;
  let last = performance.now();

  const resize = () => {
    const width = Math.max(container.clientWidth, 1);
    const height = Math.max(container.clientHeight, 1);
    renderer.setSize(width, height, false);
    renderer.domElement.style.width = "100%";
    renderer.domElement.style.height = "100%";
    camera.aspect = width / height;
    camera.updateProjectionMatrix();
  };
  const observer = new ResizeObserver(resize);
  observer.observe(container);
  resize();

  const onVisibility = () => { visible = !document.hidden; if (visible) { last = performance.now(); loop(last); } };
  document.addEventListener("visibilitychange", onVisibility);

  function loop(now) {
    if (!visible) return;
    raf = requestAnimationFrame(loop);
    const dt = Math.min((now - last) / 1000, 0.1);
    last = now;
    uniforms.uTime.value += dt;

    // Fast attack, slow release, so speech syllables register but do not flicker.
    const rate = levelInput > level ? 14 : 4;
    level += (levelInput - level) * Math.min(1, rate * dt);
    // A faint breath keeps the sphere alive when the assistant is silent.
    const breath = 0.03 + 0.02 * Math.sin(uniforms.uTime.value * 1.3);
    uniforms.uLevel.value = Math.min(1, level + breath * (1 - Math.min(level * 4, 1)));

    const k = Math.min(1, 2.2 * dt); // ~0.7 s cross-fade between states
    colorA.lerp(targetA, k);
    colorB.lerp(targetB, k);
    colorC.lerp(targetC, k);
    uniforms.uColorA.value.copy(colorA);
    uniforms.uColorB.value.copy(colorB);
    uniforms.uColorC.value.copy(colorC);
    // Each band gets its own attack/release: treble snaps, bass lingers.
    for (const [name, attack, release] of [["bass", 10, 3.2], ["mid", 13, 4.5], ["treble", 18, 7]]) {
      const r = bandsInput[name] > bands[name] ? attack : release;
      bands[name] += (bandsInput[name] - bands[name]) * Math.min(1, r * dt);
    }
    uniforms.uBass.value = bands.bass;
    uniforms.uMid.value = bands.mid;
    uniforms.uTreble.value = bands.treble;
    uniforms.uGlow.value += (target.glow - uniforms.uGlow.value) * k;
    uniforms.uCalm.value += (target.calm - uniforms.uCalm.value) * k;
    uniforms.uSpeed.value += (target.speed - uniforms.uSpeed.value) * k;
    galaxyUniforms.uGalaxySpeed.value += (target.galaxy - galaxyUniforms.uGalaxySpeed.value) * k;
    galaxyUniforms.uGalaxyGlow.value += (target.glow - galaxyUniforms.uGalaxyGlow.value) * k;
    spin += (target.spin - spin) * k;
    pulse += (target.pulse - pulse) * k;

    neuron.update(dt, uniforms.uTime.value, level);
    uniforms.uFlash.value = neuron.flashAmount();

    group.rotation.y += dt * spin;
    group.rotation.x = Math.sin(uniforms.uTime.value * 0.21) * 0.12;
    const scale = 1 + uniforms.uLevel.value * 0.07 + pulse * Math.sin(uniforms.uTime.value * 2.2);
    group.scale.setScalar(1.5 * scale);
    // Slow drift of the whole galaxy, a little faster when the assistant is busy.
    galaxyGroup.rotation.y += dt * 0.012 * (0.4 + target.galaxy);

    renderer.render(scene, camera);
  }
  raf = requestAnimationFrame(loop);

  return {
    /** level of the assistant's speech, 0..1, and its bass/mid/treble energy */
    setLevel(value, parts) {
      levelInput = Math.max(0, Math.min(1, Number(value) || 0));
      for (const name of ["bass", "mid", "treble"]) {
        bandsInput[name] = Math.max(0, Math.min(1, Number(parts?.[name]) || 0));
      }
    },
    setState(name) {
      target = STATES[name] || STATES.idle;
      neuron.setMode(name);
      targetA.set(target.a);
      targetB.set(target.b);
      targetC.set(target.c);
    },
    /** the reply has begun: a pink flash through the neuron and the sphere */
    flash() { neuron.flash(); },
    /** the user's input arrived: one incoming-stimulus pulse through the neuron */
    stimulus() { neuron.stimulus(); },
    /** the assistant is waking up: the neuron breathes its refractory afterglow */
    setWaking(on) { neuron.setWaking(on); },
    dispose() {
      neuron.dispose();
      cancelAnimationFrame(raf);
      observer.disconnect();
      document.removeEventListener("visibilitychange", onVisibility);
      galaxyGeometry.dispose(); galaxy.material.dispose();
      surface.geometry.dispose(); surface.material.dispose();
      mesh.geometry.dispose(); mesh.material.dispose();
      halo.geometry.dispose(); halo.material.dispose();
      renderer.dispose();
      renderer.domElement.remove();
    },
  };
}

// Used when WebGL is unavailable: a CSS orb driven by the same two inputs.
function fallbackOrb(container) {
  const el = document.createElement("div");
  el.className = "orb-css";
  container.append(el);
  let level = 0;
  return {
    setLevel(value) { level = Math.max(0, Math.min(1, Number(value) || 0)); el.style.setProperty("--level", level.toFixed(3)); },
    setState(name) { el.dataset.state = name; },
    flash() {}, stimulus() {}, setWaking() {},
    dispose() { el.remove(); },
  };
}
