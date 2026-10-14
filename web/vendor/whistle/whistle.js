// Whistle host-side pipeline: log-mel frontend, engram lookup, greedy decoding with
// language ID, keyword biasing, word probabilities and DTW word timestamps.
// The neural network runs in two ONNX graphs (encoder, decoder step) supplied by the caller.
import { dequantRowsInto } from './pack.js';

export const LANGS = ['en', 'de', 'fr', 'es', 'it', 'nl', 'pl'];
const LANG0 = 8192, EOS = 1, BOS = 2, TEXT_VOCAB = 8192;
const SEED = 0x9E3779B9, PRIME = 0x01000193, SLOTS = 18432;
const FRAME_S = 0.08;

// ---------------------------------------------------------------- mel frontend
const N_FFT = 512, HOP = 160, WIN = 400;
const HANN = Float64Array.from({ length: WIN }, (_, n) => 0.5 - 0.5 * Math.cos((2 * Math.PI * n) / (WIN - 1)));
const BITREV = new Uint16Array(N_FFT), COS = new Float64Array(N_FFT / 2), SIN = new Float64Array(N_FFT / 2);
for (let i = 0; i < N_FFT; i++) { let r = 0; for (let b = 0, x = i; b < 9; b++, x >>= 1) r = (r << 1) | (x & 1); BITREV[i] = r; }
for (let i = 0; i < N_FFT / 2; i++) { COS[i] = Math.cos((-2 * Math.PI * i) / N_FFT); SIN[i] = Math.sin((-2 * Math.PI * i) / N_FFT); }

function fft(re, im) {
  for (let i = 0; i < N_FFT; i++) { const j = BITREV[i]; if (j > i) { let t = re[i]; re[i] = re[j]; re[j] = t; t = im[i]; im[i] = im[j]; im[j] = t; } }
  for (let size = 2; size <= N_FFT; size <<= 1) {
    const half = size >> 1, step = N_FFT / size;
    for (let i = 0; i < N_FFT; i += size)
      for (let j = 0, k = 0; j < half; j++, k += step) {
        const a = i + j, b = a + half;
        const tr = re[b] * COS[k] - im[b] * SIN[k], ti = re[b] * SIN[k] + im[b] * COS[k];
        re[b] = re[a] - tr; im[b] = im[a] - ti; re[a] += tr; im[a] += ti;
      }
  }
}

// pcm: Float32Array, mono 16 kHz. fb: Float32Array [257*80]. Returns {mel: Float32Array [T*80], T}.
export function melFeatures(pcm, fb) {
  const n = pcm.length, nch = Math.floor(n / 320);
  let gain = 1;
  if (nch > 0) {
    const r = new Float64Array(nch);
    for (let c = 0; c < nch; c++) { let s = 0; for (let i = c * 320; i < c * 320 + 320; i++) s += pcm[i] * pcm[i]; r[c] = Math.sqrt(s / 320); }
    r.sort(); const p = r[Math.floor((nch - 1) * 0.99)];
    if (p > 1e-6) gain = 0.1 / p;
  }
  const T = Math.floor(n / HOP), M = 80, NB = N_FFT / 2 + 1;
  const L = new Float64Array(T * M), re = new Float64Array(N_FFT), im = new Float64Array(N_FFT), pw = new Float64Array(NB);
  for (let t = 0; t < T; t++) {
    re.fill(0); im.fill(0);
    const s0 = t * HOP - 200;
    for (let i = 0; i < WIN; i++) { const k = s0 + i; if (k >= 0 && k < n) re[i] = pcm[k] * gain * HANN[i]; }
    fft(re, im);
    for (let k = 0; k < NB; k++) pw[k] = re[k] * re[k] + im[k] * im[k];
    for (let m = 0; m < M; m++) { let s = 0; for (let k = 0; k < NB; k++) s += pw[k] * fb[k * M + m]; L[t * M + m] = Math.log(s + 2 ** -24); }
  }
  const mel = new Float32Array(T * M);
  for (let m = 0; m < M; m++) {
    let mu = 0; for (let t = 0; t < T; t++) mu += L[t * M + m]; mu /= T;
    let v = 0; for (let t = 0; t < T; t++) { const d = L[t * M + m] - mu; v += d * d; }
    const sd = T > 1 ? Math.sqrt(v / (T - 1)) : 0;
    for (let t = 0; t < T; t++) mel[t * M + m] = (L[t * M + m] - mu) / (sd + 1e-5);
  }
  return { mel, T };
}

// ---------------------------------------------------------------- tokenizer
export class Tokenizer {
  constructor(tok) {
    this.pieces = tok.pieces; this.scores = tok.scores; this.types = tok.types;
    this.map = new Map(); this.maxLen = 1;
    this.byte = new Int32Array(256).fill(-1);
    tok.pieces.forEach((p, i) => {
      const ty = tok.types[i];
      if (ty === 4) { const m = /^<0x([0-9A-F]{2})>$/.exec(p); if (m) this.byte[parseInt(m[1], 16)] = i; }
      else if ((ty === 0 || ty === 3) && i < TEXT_VOCAB) { this.map.set(p, i); this.maxLen = Math.max(this.maxLen, p.length); }
    });
    this.dec = new TextDecoder();
  }
  decode(ids) {
    const out = [];
    for (const i of ids) {
      const p = this.pieces[i];
      if (this.types[i] === 4) out.push(parseInt(p.slice(3, 5), 16));
      else if (this.types[i] === 0 || this.types[i] === 3) for (const b of new TextEncoder().encode(p.replaceAll('▁', ' '))) out.push(b);
    }
    return this.dec.decode(new Uint8Array(out));
  }
  // SentencePiece BPE (merge by score, byte fallback), as in the reference tokenizer.
  encode(text) {
    const syms = Array.from(text.replaceAll(' ', '\u2581'));
    while (syms.length > 1) {
      let best = -Infinity, bj = -1;
      for (let j = 0; j < syms.length - 1; j++) {
        const id = this.map.get(syms[j] + syms[j + 1]);
        if (id !== undefined && this.scores[id] > best) { best = this.scores[id]; bj = j; }
      }
      if (bj < 0) break;
      syms.splice(bj, 2, syms[bj] + syms[bj + 1]);
    }
    const ids = [];
    for (const s of syms) {
      const id = this.map.get(s);
      if (id !== undefined) ids.push(id);
      else for (const b of new TextEncoder().encode(s)) ids.push(this.byte[b]);
    }
    return ids;
  }
}

// ---------------------------------------------------------------- engram
function engramHash(toks, t, out, o) {
  let k = 0;
  for (let oi = 0; oi < 2; oi++) {
    const order = oi + 2;
    for (let h = 0; h < 2; h++) {
      let acc = Math.imul(SEED, oi * 2 + h + 1) >>> 0;
      for (let j = 0; j < order; j++) {
        const u = t - j >= 0 ? toks[t - j] : 0;
        acc = Math.imul((acc ^ u) >>> 0, PRIME) >>> 0;
      }
      acc = (acc ^ (acc >>> 15)) >>> 0;
      out[o + k++] = acc % SLOTS;
    }
  }
}

export class Engram {
  constructor(pack) { this.pack = pack; this.segs = pack.man.engram; this.cache = [new Map(), new Map()]; }
  row(e, table, idx) {
    const key = table * SLOTS + idx, c = this.cache[e];
    let r = c.get(key);
    if (!r) { r = new Float32Array(128); dequantRowsInto(this.pack, this.segs[e], key, key + 1, r); c.set(key, r); }
    return r;
  }
  // [2, 4 taps, 4 tables * 128] for position t
  feat(toks, t) {
    const f = new Float32Array(2 * 4 * 512), idx = new Int32Array(4);
    for (let j = 0; j < 4; j++) {
      const p = t - 3 * j; if (p < 0) continue;
      engramHash(toks, p, idx, 0);
      for (let i = 0; i < 4; i++) {
        if (p < (i < 2 ? 1 : 2)) continue;  // n-gram of order 2 (tables 0,1) / 3 (tables 2,3) not yet complete
        for (let e = 0; e < 2; e++) f.set(this.row(e, i, idx[i]), (e * 4 + j) * 512 + i * 128);
      }
    }
    return f;
  }
}

// ---------------------------------------------------------------- keyword biasing
// Trie over keyword token sequences: a small boost for a keyword's first token and a
// larger one for continuing a partially emitted keyword.
export function buildKeywordBias(tokenizer, keywords) {
  const seqs = [];
  for (const raw of keywords) {
    const kw = raw.trim(); if (!kw) continue;
    const forms = new Set([kw, kw[0].toUpperCase() + kw.slice(1)]);
    for (const f of forms) seqs.push(tokenizer.encode(' ' + f));
  }
  return seqs.filter((s) => s.length);
}
function applyBias(logits, seqs, gen, first = 2.0, cont = 5.0) {
  if (!seqs.length) return;
  const boost = new Map();
  for (const s of seqs) {
    for (let k = Math.min(s.length - 1, gen.length); k >= 0; k--) {
      let ok = true;
      for (let q = 0; q < k; q++) if (gen[gen.length - k + q] !== s[q]) { ok = false; break; }
      if (ok) { const b = k ? cont : first; const id = s[k]; boost.set(id, Math.max(boost.get(id) || 0, b)); break; }
    }
  }
  for (const [id, b] of boost) logits[id] += b;
}

// ---------------------------------------------------------------- timestamps
function dtw(C, N, M) {  // C: Float64Array N*M cost; whisper-style steps (diag, up, left)
  const cost = new Float64Array((N + 1) * (M + 1)).fill(Infinity), tr = new Uint8Array((N + 1) * (M + 1));
  cost[0] = 0;
  for (let i = 1; i <= N; i++)
    for (let j = 1; j <= M; j++) {
      const c0 = cost[(i - 1) * (M + 1) + j - 1], c1 = cost[(i - 1) * (M + 1) + j], c2 = cost[i * (M + 1) + j - 1];
      let k = 0, c = c0; if (c1 < c) { c = c1; k = 1; } if (c2 < c) { c = c2; k = 2; }
      cost[i * (M + 1) + j] = C[(i - 1) * M + j - 1] + c; tr[i * (M + 1) + j] = k;
    }
  const path = []; let i = N, j = M;
  while (i > 0 && j > 0) { path.push([i - 1, j - 1]); const k = tr[i * (M + 1) + j]; if (k === 0) { i--; j--; } else if (k === 1) i--; else j--; }
  return path.reverse();
}

// att: array (one per row) of Float32Array [H*S] (H = 4 layers * 8 heads). Returns start frame per row.
function tokenFrames(att, S) {
  const R = att.length, H = att[0].length / S, m = new Float64Array(R * S);
  for (let h = 0; h < H; h++)
    for (let s = 0; s < S; s++) {
      let mu = 0; for (let r = 0; r < R; r++) mu += att[r][h * S + s]; mu /= R;
      let v = 0; for (let r = 0; r < R; r++) { const d = att[r][h * S + s] - mu; v += d * d; }
      const sd = Math.sqrt(v / R) + 1e-9;
      for (let r = 0; r < R; r++) m[r * S + s] -= (att[r][h * S + s] - mu) / sd / H;
    }
  const st = new Int32Array(R).fill(-1);
  for (const [i, j] of dtw(m, R, S)) if (st[i] < 0) st[i] = j;
  return st;
}

function words(tok, ids, probs, frames) {
  const groups = [];
  ids.forEach((id, i) => { if (!groups.length || tok.pieces[id].startsWith('▁')) groups.push([i]); else groups[groups.length - 1].push(i); });
  const r2 = (x) => Math.round(x * 100) / 100;
  return groups.map((g, wi) => {
    const word = tok.decode(g.map((i) => ids[i])).trim();
    const p = g.reduce((a, i) => a + probs[i], 0) / g.length;
    const out = { word, probability: Math.round(p * 1000) / 1000 };
    if (frames) {
      out.start = r2(frames[g[0]] * FRAME_S);
      out.end = r2((wi + 1 < groups.length ? frames[groups[wi + 1][0]] : frames[ids.length]) * FRAME_S);
    }
    return out;
  }).filter((w) => w.word.length);
}

// ---------------------------------------------------------------- decoding loop
// backend: { encode(mel, T) -> {ck, cv, S}, step(feeds) -> {logits: Float32Array, xattn: Float32Array|null},
//            reset() } where the backend keeps prev/KV state itself.
export async function transcribe(backend, ctx, pcm, opts = {}) {
  const { tok, fb, engram } = ctx;
  const t0 = performance.now();
  const { mel, T } = melFeatures(pcm, fb);
  if (T < 4) return { text: '', language: null, words: [], ttft_ms: 0, decode_tps: 0 };
  const tMel = performance.now();
  const S = await backend.encode(mel, T);
  const tEnc = performance.now();
  const maxTokens = opts.maxTokens || Math.min(320, 16 + Math.ceil(T / 100) * 12);
  const bias = opts.keywords && opts.keywords.length ? buildKeywordBias(tok, opts.keywords) : [];
  const toks = [BOS], ids = [], probs = [], att = [];
  let lang = opts.language || null, ttft = 0;
  for (let t = 0; ; t++) {
    const lg = await backend.step(toks[t], t, engram.feat(toks, t), opts.timestamps && t >= 1);
    if (t === 0) {
      if (!lang) { let b = 0; for (let i = 1; i < 7; i++) if (lg.logits[LANG0 + i] > lg.logits[LANG0 + b]) b = i; lang = LANGS[b]; }
      toks.push(LANG0 + LANGS.indexOf(lang));
      continue;
    }
    if (opts.timestamps) att.push(lg.xattn);
    const x = lg.logits.subarray(0, TEXT_VOCAB);
    if (bias.length) applyBias(x, bias, ids);
    let k = 0; for (let i = 1; i < TEXT_VOCAB; i++) if (x[i] > x[k]) k = i;
    let z = 0; for (let i = 0; i < TEXT_VOCAB; i++) z += Math.exp(x[i] - x[k]);
    probs.push(1 / z);
    if (t === 1) ttft = performance.now() - t0;
    if (k === EOS || ids.length >= maxTokens) break;
    ids.push(k); toks.push(k);
  }
  const tDec = performance.now();
  const text = tok.decode(ids).trim();
  const frames = opts.timestamps && ids.length ? tokenFrames(att, S) : null;
  const ws = words(tok, ids, probs, frames);
  const nsteps = ids.length + 2;
  return {
    text, language: lang, words: ws, ids,
    ttft_ms: ttft, decode_tps: (ids.length + 1) / ((tDec - tEnc) / 1000),
    timing: { mel_ms: tMel - t0, encode_ms: tEnc - tMel, decode_ms: tDec - tEnc, steps: nsteps, ms_per_step: (tDec - tEnc) / nsteps, total_ms: performance.now() - t0 },
  };
}
