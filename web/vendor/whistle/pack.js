// Rebuilds ONNX external-data buffers for Whistle from the compact whistle.pack
// (2/4-bit Hadamard-rotated codebook weights, as shipped in whistle.cact).

const f32b = new Float32Array(1), u32b = new Uint32Array(f32b.buffer);

export function f16ToF32(h) {
  const s = (h & 0x8000) << 16, e = (h >> 10) & 0x1f, m = h & 0x3ff;
  if (e === 0) {
    if (m === 0) { u32b[0] = s; return f32b[0]; }
    const v = m * 5.960464477539063e-8; return s ? -v : v;
  }
  if (e === 31) { u32b[0] = s | 0x7f800000 | (m << 13); return f32b[0]; }
  u32b[0] = s | ((e + 112) << 23) | (m << 13); return f32b[0];
}

export function f32ToF16(v) {
  f32b[0] = v; const x = u32b[0];
  const s = (x >>> 16) & 0x8000; let e = (x >>> 23) & 0xff; let m = x & 0x7fffff;
  if (e === 0xff) return s | 0x7c00 | (m ? 0x200 : 0);
  e = e - 127 + 15;
  if (e >= 31) return s | 0x7c00;
  if (e <= 0) {
    if (e < -10) return s;
    m |= 0x800000; const sh = 14 - e;
    let r = m >>> sh; const rem = m & ((1 << sh) - 1), half = 1 << (sh - 1);
    if (rem > half || (rem === half && (r & 1))) r++;
    return s | r;
  }
  let r = (e << 10) | (m >>> 13); const rem = m & 0x1fff;
  if (rem > 0x1000 || (rem === 0x1000 && (r & 1))) r++;
  return s | r;
}

export function parsePack(buf) {
  const u8 = new Uint8Array(buf);
  const magic = String.fromCharCode(...u8.subarray(0, 4));
  if (magic !== 'WPK1') throw new Error('bad pack');
  const jl = new DataView(buf).getUint32(4, true);
  const man = JSON.parse(new TextDecoder().decode(u8.subarray(8, 8 + jl)));
  let base = 8 + jl; while (base % 16) base++;
  return { man, buf, base };
}

// in-place unnormalised Walsh-Hadamard transform of a[o..o+n) (Float64Array, n power of 2)
function fwht(a, o, n) {
  let h = 1;
  for (; h * 4 <= n; h <<= 2)
    for (let i = 0; i < n; i += 4 * h)
      for (let j = o + i, e = o + i + h; j < e; j++) {
        const a0 = a[j], a1 = a[j + h], a2 = a[j + 2 * h], a3 = a[j + 3 * h];
        const s0 = a0 + a1, d0 = a0 - a1, s1 = a2 + a3, d1 = a2 - a3;
        a[j] = s0 + s1; a[j + h] = d0 + d1; a[j + 2 * h] = s0 - s1; a[j + 3 * h] = d0 - d1;
      }
  if (h < n)
    for (let i = 0; i < n; i += 2 * h)
      for (let j = o + i, e = o + i + h; j < e; j++) { const x = a[j], y = a[j + h]; a[j] = x + y; a[j + h] = x - y; }
}

// Dequantise rows [r0, r1) of a CQ record -> Float32Array((r1-r0) * in)
function dequantRows(pack, seg) {
  const res = new Float32Array((seg.rows[1] - seg.rows[0]) * seg.in);
  dequantRowsInto(pack, seg, seg.rows[0], seg.rows[1], res);
  return res;
}
const CB = new WeakMap();
export function dequantRowsInto(pack, seg, r0, r1, res) {
  const { buf, base, man } = pack;
  const { out, bits, group: g } = seg; const inn = seg.in;
  const inPad = Math.ceil(inn / g) * g, ng = inPad / g;
  const rowBytes = inPad * bits / 8;
  const src = base + seg.src;
  const packed = new Uint8Array(buf, src, out * rowBytes);
  const normOff = src + out * rowBytes;
  const nv = new DataView(buf, normOff, out * ng * 2);
  let cbs = CB.get(man); if (!cbs) { cbs = {}; CB.set(man, cbs); }
  const cb = cbs[bits] || (cbs[bits] = Float64Array.from(man.codebooks[bits]));
  const mask = (1 << bits) - 1, sc = 1 / Math.sqrt(g);
  const tmp = new Float64Array(inPad);
  for (let r = r0; r < r1; r++) {
    const pb = r * rowBytes;
    if (bits === 2) {
      for (let b = 0; b < rowBytes; b++) {
        const v = packed[pb + b], k = b * 4;
        tmp[k] = cb[v & 3]; tmp[k + 1] = cb[(v >> 2) & 3]; tmp[k + 2] = cb[(v >> 4) & 3]; tmp[k + 3] = cb[v >> 6];
      }
    } else if (bits === 4) {
      for (let b = 0; b < rowBytes; b++) {
        const v = packed[pb + b], k = b * 2;
        tmp[k] = cb[v & 15]; tmp[k + 1] = cb[v >> 4];
      }
    } else {
      for (let k = 0; k < inPad; k++) {
        const bp = k * bits, by = pb + (bp >> 3), sh = bp & 7;
        const w = packed[by] | ((packed[by + 1] || 0) << 8);
        tmp[k] = cb[(w >> sh) & mask];
      }
    }
    for (let gi = 0; gi < ng; gi++) {
      const nrm = f16ToF32(nv.getUint16((r * ng + gi) * 2, true)) * sc;
      const o = gi * g;
      for (let j = 0; j < g; j++) tmp[o + j] *= nrm;
      fwht(tmp, o, g);
    }
    const ro = (r - r0) * inn;
    for (let j = 0; j < inn; j++) res[ro + j] = tmp[j];
  }
}

// Build the external-data Uint8Array for one model of the pack.
const HAS_F16 = typeof Float16Array !== 'undefined';
function toF16(src, dst) {
  if (HAS_F16) { new Float16Array(dst.buffer, dst.byteOffset, dst.length).set(src); return; }
  for (let i = 0; i < src.length; i++) dst[i] = f32ToF16(src[i]);
}
export function buildModelData(pack, modelName, onProgress) {
  const m = pack.man.models[modelName];
  const out = new Uint8Array(m.size);
  let done = 0; const total = m.inits.reduce((a, it) => a + it.len, 0);
  for (const it of m.inits) {
    if (it.dt === 'i64') {
      const dv = new DataView(out.buffer, it.off, it.len);
      for (const s of it.segs) {
        const sv = new DataView(pack.buf, pack.base + s.src);
        for (let i = 0; i < s.n; i++) dv.setBigInt64(8 * (s.dst + i), BigInt(sv.getInt32(4 * i, true)), true);
      }
      continue;
    }
    const n = it.dt === 'f32' ? it.len / 4 : it.len / 2;
    const T = (it.dt === 'f32' && it.off % 4 === 0) ? new Float32Array(out.buffer, it.off, n) : new Float32Array(n);
    for (const s of it.segs) {
      if (s.k === 'raw') {
        const p = pack.base + s.src;
        if (s.dt === 'f16') {
          const sv = (p % 2 === 0) ? new Uint16Array(pack.buf, p, s.n) : null;
          for (let i = 0; i < s.n; i++) T[s.dst + i] = f16ToF32(sv[i]);
        } else if (s.dt === 'f32') {
          T.set(new Float32Array(pack.buf.slice(p, p + 4 * s.n)), s.dst);
        }
      } else {
        const w = dequantRows(pack, s);
        const rows = s.rows[1] - s.rows[0], inn = s.in, d = s.dst;
        if (s.op === 'id') T.set(w, d);
        else for (let r = 0; r < rows; r++) { const ro = r * inn; for (let c = 0; c < inn; c++) T[d + c * rows + r] = w[ro + c]; }
      }
    }
    if (it.dt === 'f16') toF16(T, new Uint16Array(out.buffer, it.off, n));
    else if (T.buffer !== out.buffer) new Uint8Array(out.buffer, it.off, it.len).set(new Uint8Array(T.buffer));
    done += it.len; if (onProgress) onProgress(done / total);
  }
  return out;
}
