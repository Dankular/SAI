// A bioelectric neuron that lives inside the sphere and fires while the assistant works.
//
// Adapted from "Bioelectric consciousness engine" by Techartist (MIT, see
// vendor/BIOELECTRIC_LICENSE.txt): a soma with dendrite trees that collect a signal and
// axons that carry it away. The original runs one hand-triggered firing sequence; here the
// sequence is mapped onto the conversation, one phase per thing that is happening:
//
//   incoming stimulus   (phase 1) the user's input: pulses sweep in along the dendrites
//                       while they speak, and once when a typed or spoken message is sent
//   soma merge          (phase 2) the model is thinking: the signals converge and burn at the core
//   axon outflow        (phase 3) the assistant is speaking: pulses fire out along the axons,
//                       in step with the loud parts of its voice
//   refractory period   (phase 4) the violet afterglow: used while it wakes up, and as the neuron
//                       settles after a reply
//   responding          a hot pink flash through the whole neuron when the reply begins
//
// Pink is reserved for that flash, so it always reads as "it just answered".
import * as THREE from "/vendor/three.module.min.js";

const SOMA = 4.0;
const FIT = 1.78; // the neuron fills the sphere out to here (the sphere's radius is 2)
const R0 = 50;    // the neuron's own units after fitting; the group is scaled by FIT / R0

// phases: 0 rest, 1 incoming stimulus, 2 soma merge, 3 axon outflow, 4 refractory period
const PHASE_SPEED = [0, 0.8, 1.6, 0.95, 0.5];

const f = (n) => n.toFixed(1);

const VERT = /* glsl */ `
uniform float uTime;
varying float vDist;
varying vec3 vNormal;
varying vec3 vView;
varying vec2 vUv;
void main(){
  vUv=uv;
  vDist=length(position);
  vNormal=normalize(normalMatrix*normal);
  vec4 mv=modelViewMatrix*vec4(position,1.0);
  vView=-mv.xyz;
  gl_Position=projectionMatrix*mv;
}`;

const FRAG = /* glsl */ `
uniform float uTime;
uniform int uPhase;
uniform float uProgress;
uniform float uFlash;
uniform float uVisible;
uniform int uIsAxon;
uniform float uSoma;
uniform float uReachD;
uniform float uReachA;
uniform vec3 uCyan;
uniform vec3 uBlue;
uniform vec3 uGold;
uniform vec3 uOrange;
uniform vec3 uViolet;
uniform vec3 uPink;
varying float vDist;
varying vec3 vNormal;
varying vec3 vView;
varying vec2 vUv;
float hash(vec2 p){return fract(sin(dot(p,vec2(127.1,311.7)))*43758.5453);}
float vnoise(vec2 p){
  vec2 i=floor(p),f=fract(p);f=f*f*(3.0-2.0*f);
  return mix(mix(hash(i),hash(i+vec2(1,0)),f.x),mix(hash(i+vec2(0,1)),hash(i+vec2(1,1)),f.x),f.y);
}
void main(){
  float edge=pow(1.0-abs(vUv.y-0.5)*2.0,2.0);
  float fres=pow(1.0-max(dot(normalize(vNormal),normalize(vView)),0.0),2.0);
  vec3 base=mix(uBlue*0.35,uCyan*0.9,fres*edge+0.25*edge)*uVisible;
  float flow=vnoise(vec2(vUv.x*20.0-uTime*2.0,vUv.y*10.0+uTime))*0.5+0.5;
  float axial=0.65+0.35*sin(vUv.x*34.0-uTime*8.0);
  vec3 pulse=vec3(0.0);
  if(uPhase==2){
    // soma merge: the signals converge and burn where the fibres meet the core
    float burn=sin(uProgress*3.14159)*(1.0-smoothstep(uSoma,(uSoma*5.0),vDist));
    pulse+=uGold*burn*(0.9+0.6*flow)*2.6;
  }
  if(uIsAxon==0){
    if(uPhase==1){
      float wave=mix(uReachD,uSoma,uProgress);
      float head=1.0-smoothstep(0.0,7.0,abs(vDist-wave));
      float trail=step(wave,vDist)*exp(-(vDist-wave)*0.06);
      float merge=(1.0-smoothstep(uSoma,(uSoma*3.0),vDist))*smoothstep(0.65,1.0,uProgress);
      pulse=uGold*(max(head*1.6,trail*0.65)+merge*0.8)*flow*axial*3.4;
    }
  }else if(uPhase==3){
    float wave=mix(uSoma,uReachA*1.12,uProgress);
    float head=1.0-smoothstep(0.0,9.0,abs(vDist-wave));
    float trail=step(vDist,wave)*exp(-(wave-vDist)*0.04);
    float launch=(1.0-smoothstep(uSoma,(uSoma*2.75),vDist))*(1.0-smoothstep(0.0,0.28,uProgress));
    pulse=(uOrange+uGold*0.45)*(max(head*2.0,trail*0.9)+launch*1.2)*flow*axial*4.4;
  }
  if(uPhase==4) pulse+=uViolet*(1.0-uProgress)*edge*1.2;
  vec3 col=base+pulse;
  // the pink flash: everything lights hot pink, brightest along the fibre's centre line
  col+=uPink*uFlash*(0.5+edge*2.2);
  gl_FragColor=vec4(col,1.0);
}`;

const SYN_VERT = /* glsl */ `
attribute float aSize;
uniform float uPixel;
varying float vSize;
void main(){
  vSize=aSize;
  vec4 mv=modelViewMatrix*vec4(position,1.0);
  gl_PointSize=uPixel*(3.0+aSize*4.0)*(15.0/max(-mv.z,1.0));
  gl_Position=projectionMatrix*mv;
}`;

const SYN_FRAG = /* glsl */ `
uniform int uPhase;
uniform float uProgress;
uniform float uFlash;
uniform float uVisible;
uniform vec3 uCyan;
uniform vec3 uGold;
uniform vec3 uPink;
varying float vSize;
void main(){
  float d=length(gl_PointCoord-vec2(0.5));
  if(d>0.5) discard;
  float alpha=(0.5-d)*2.0;
  vec3 col=uCyan*0.5*uVisible;
  if(uPhase==1||uPhase==3){
    float spark=step(0.8,fract(vSize*10.0+uProgress*5.0));
    col=mix(col,uGold*1.6,spark);
  }
  col=mix(col,uPink*1.6,clamp(uFlash,0.0,1.0));
  gl_FragColor=vec4(col,alpha*(0.35+0.65*max(uVisible,uFlash)));
}`;

// Concatenates tube geometries into one, so the whole neuron is two draw calls.
function merge(geometries) {
  const names = ["position", "normal", "uv"];
  const sizes = { position: 3, normal: 3, uv: 2 };
  const parts = { position: [], normal: [], uv: [] };
  const index = [];
  let offset = 0;
  for (const g of geometries) {
    for (const name of names) parts[name].push(g.getAttribute(name).array);
    for (const i of g.index.array) index.push(i + offset);
    offset += g.getAttribute("position").count;
    g.dispose();
  }
  const out = new THREE.BufferGeometry();
  for (const name of names) {
    const array = new Float32Array(parts[name].reduce((n, a) => n + a.length, 0));
    let at = 0;
    for (const a of parts[name]) { array.set(a, at); at += a.length; }
    out.setAttribute(name, new THREE.BufferAttribute(array, sizes[name]));
  }
  out.setIndex(index);
  return out;
}

export function createNeuron({ pixelRatio = 1 } = {}) {
  const uniforms = {
    uTime: { value: 0 },
    uPhase: { value: 0 },
    uProgress: { value: 0 },
    uFlash: { value: 0 },
    uVisible: { value: 0.3 },
    uPixel: { value: pixelRatio },
    uCyan: { value: new THREE.Color("#00e5ff") },
    uBlue: { value: new THREE.Color("#0033aa") },
    uGold: { value: new THREE.Color("#ffc24a") },
    uOrange: { value: new THREE.Color("#ff7a1a") },
    uViolet: { value: new THREE.Color("#5a4cff") },
    uPink: { value: new THREE.Color("#ff2fb9") },
  };
  const material = (isAxon) => new THREE.ShaderMaterial({
    uniforms: { ...uniforms, uIsAxon: { value: isAxon } },
    vertexShader: VERT, fragmentShader: FRAG,
    transparent: true, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false,
  });

  const dendrites = [];
  const axons = [];
  const tips = [];
  const jitter = () => new THREE.Vector3(Math.random() - 0.5, Math.random() - 0.5, Math.random() - 0.5);
  function branch(start, dir, length, radius, level, maxLevels, isAxon) {
    const segments = 12;
    const points = [start.clone()];
    const at = start.clone();
    const heading = dir.clone();
    for (let i = 0; i < segments; i++) {
      heading.add(jitter().multiplyScalar(isAxon ? 0.3 : 0.8)).normalize();
      at.add(heading.clone().multiplyScalar(length / segments));
      points.push(at.clone());
    }
    const tube = new THREE.TubeGeometry(new THREE.CatmullRomCurve3(points), segments * 2, radius, 6, false);
    (isAxon ? axons : dendrites).push(tube);
    if (level < maxLevels) {
      const children = isAxon ? (Math.random() > 0.3 ? 1 : 2) : (Math.random() > 0.2 ? 2 : 3);
      for (let j = 0; j < children; j++) {
        const split = heading.clone().add(jitter().multiplyScalar(0.8)).normalize();
        branch(at, split, length * (0.6 + Math.random() * 0.3), radius * 0.65, level + 1, maxLevels, isAxon);
      }
    } else {
      tips.push(at.x, at.y, at.z);
    }
  }
  for (let i = 0; i < 14; i++) {
    const dir = jitter().normalize();
    branch(dir.clone().multiplyScalar(SOMA - 0.5), dir, 12 + Math.random() * 5, 0.75, 0, 2, false);
  }
  for (let i = 0; i < 4; i++) {
    const dir = new THREE.Vector3((Math.random() - 0.5) * 0.5, (Math.random() - 0.5) * 0.5, 1).normalize();
    branch(dir.clone().multiplyScalar(SOMA - 0.5), dir, 25 + Math.random() * 10, 1.0, 0, 2, true);
  }

  const group = new THREE.Group();
  const dendriteGeometry = merge(dendrites);
  const axonGeometry = merge(axons);
  // farthest point of each kind of fibre, so the whole neuron can be fitted to the sphere exactly
  const reach = (g) => { let r = 0; const p = g.getAttribute("position").array; for (let i = 0; i < p.length; i += 3) r = Math.max(r, Math.hypot(p[i], p[i + 1], p[i + 2])); return r; };
  const reachAll = Math.max(reach(dendriteGeometry), reach(axonGeometry), SOMA);
  // Squash the tree radially so it fills the sphere instead of poking out: near the core the
  // fibres keep their spacing, and the longest ones ease in towards the edge rather than being cut.
  const k = reachAll * 0.42;
  const squash = (r) => R0 * Math.tanh(r / k) / Math.tanh(reachAll / k);
  for (const g of [dendriteGeometry, axonGeometry]) {
    const p = g.getAttribute("position");
    for (let i = 0; i < p.count; i++) {
      const x = p.getX(i), y = p.getY(i), z = p.getZ(i);
      const r = Math.hypot(x, y, z) || 1;
      const s = squash(r) / r;
      p.setXYZ(i, x * s, y * s, z * s);
    }
    p.needsUpdate = true;
  }
  const reachD = reach(dendriteGeometry);
  const reachA = reach(axonGeometry);
  const somaR = squash(SOMA);
  uniforms.uSoma = { value: somaR };
  uniforms.uReachD = { value: reachD };
  uniforms.uReachA = { value: reachA };
  const dendriteMesh = new THREE.Mesh(dendriteGeometry, material(0));
  const axonMesh = new THREE.Mesh(axonGeometry, material(1));

  const synGeometry = new THREE.BufferGeometry();
  synGeometry.setAttribute("position", new THREE.Float32BufferAttribute(tips, 3));
  const sizes = new Float32Array(tips.length / 3).map(() => Math.random());
  synGeometry.setAttribute("aSize", new THREE.BufferAttribute(sizes, 1));
  const synapses = new THREE.Points(synGeometry, new THREE.ShaderMaterial({
    uniforms, vertexShader: SYN_VERT, fragmentShader: SYN_FRAG,
    transparent: true, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false,
  }));

  // the soma: a soft glowing core where the signals meet
  const soma = new THREE.Mesh(
    new THREE.IcosahedronGeometry(somaR * 0.62, 3),
    new THREE.MeshBasicMaterial({ color: 0x00e5ff, transparent: true, opacity: 0.18, blending: THREE.AdditiveBlending, wireframe: true, depthWrite: false, depthTest: false }),
  );
  const somaGlow = new THREE.Mesh(
    new THREE.SphereGeometry(somaR * 0.5, 24, 24),
    new THREE.MeshBasicMaterial({ color: 0xffc24a, transparent: true, opacity: 0, blending: THREE.AdditiveBlending, depthWrite: false, depthTest: false }),
  );
  const parts = [dendriteMesh, axonMesh, synapses, soma, somaGlow];
  for (const part of parts) { part.renderOrder = 3; group.add(part); } // after the shell: the neuron glows through it
  group.scale.setScalar(FIT / R0);

  // ---- behaviour ----
  // Each phase is driven by what the assistant is doing, not chained one after another:
  // loopPhase repeats while a state lasts, and one-shot phases (a stimulus, an afterglow) queue up.
  let phase = 0;
  let progress = 0;
  let cooldown = 0;
  let flash = 0;
  let loopPhase = 0;
  let rate = 1.8;
  let waking = false;
  let lastMode = "idle";
  const queue = [];

  const LOOPS = { listening: [1, 1.5], working: [2, 1.3], speaking: [3, 2.4] };
  const VISIBLE = { idle: 0.3, active: 0.7, listening: 0.85, working: 1.3, speaking: 1.3 };
  let visibleTarget = 0.6;

  function applyMode() {
    const mode = waking ? "waking" : lastMode;
    if (waking) { loopPhase = 4; rate = 0.9; }
    else if (LOOPS[mode]) [loopPhase, rate] = LOOPS[mode];
    else { loopPhase = 0; rate = 1.8; }
    visibleTarget = waking ? 0.9 : VISIBLE[lastMode] ?? 0.6;
  }

  return {
    group,
    /** the orb's state: idle | active | listening | working | speaking */
    setMode(name) {
      const was = lastMode;
      lastMode = name;
      // after thinking or speaking the neuron settles through its refractory period
      if ((was === "working" || was === "speaking") && name !== "working" && name !== "speaking" && !waking) queue.push(4);
      applyMode();
    },
    /** the assistant is waking up: the refractory afterglow breathes until it is ready */
    setWaking(on) {
      if (on === waking) return;
      waking = on;
      applyMode();
    },
    /** the user's input arrived (a typed or spoken message was sent): one incoming-stimulus pulse */
    stimulus() { queue.push(1); },
    /** the hot pink flash: the reply has begun */
    flash() { flash = 1; },
    update(dt, time, level) {
      uniforms.uTime.value = time;
      uniforms.uVisible.value += (visibleTarget - uniforms.uVisible.value) * Math.min(1, 3 * dt);
      cooldown = Math.max(0, cooldown - dt);
      if (phase === 0 && cooldown === 0) {
        if (queue.length) { phase = queue.shift(); progress = 0; }
        else if (loopPhase === 3 && level > 0.28) { phase = 3; progress = 0; } // outflow follows the loud syllables
        else if (loopPhase && loopPhase !== 3) { phase = loopPhase; progress = 0; }
      }
      if (phase > 0) {
        progress += dt * PHASE_SPEED[phase] * rate;
        if (progress >= 1) { cooldown = phase === 4 ? 0.2 : 0.06; progress = 0; phase = 0; }
      }
      flash = Math.max(0, flash - dt * 1.15);
      uniforms.uPhase.value = phase;
      uniforms.uProgress.value = progress;
      uniforms.uFlash.value = flash * flash * (3 - 2 * flash);
      const merging = phase === 2 ? Math.sin(progress * Math.PI) : 0;
      somaGlow.material.opacity = Math.min(0.9, merging * 0.7 + (phase === 3 ? 0.25 * (1 - progress) : 0) + flash * 0.8);
      somaGlow.material.color.set(flash > 0.05 ? 0xff2fb9 : phase === 4 ? 0x7a5cff : 0xffc24a);
      soma.material.opacity = 0.14 + uniforms.uVisible.value * 0.12 + flash * 0.4;
      soma.material.color.set(flash > 0.05 ? 0xff5fd2 : 0x00e5ff);
      group.rotation.y += dt * 0.08;
      group.rotation.x = Math.sin(time * 0.17) * 0.15;
    },
    /** 0..1, how much of the pink flash is showing (the sphere reads this to flush pink too) */
    flashAmount: () => uniforms.uFlash.value,
    dispose() {
      for (const mesh of parts) { mesh.geometry.dispose(); mesh.material.dispose(); }
    },
  };
}
