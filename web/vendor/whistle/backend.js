// onnxruntime (web or node) backend for Whistle: encoder session + decoder step session
// with KV cache. On WebGPU the KV cache and encoder outputs stay on the GPU.
export class OrtBackend {
  constructor(ort, enc, dec, { encGpu = false, decGpu = false } = {}) {
    Object.assign(this, { ort, enc, dec, encGpu, decGpu });
    this.state = null;
  }
  _free(...ts) { for (const t of ts) if (t && t.location === 'gpu-buffer' && t !== this.ck && t !== this.cv) t.dispose(); }
  async encode(mel, T) {
    const { ort } = this;
    this.release();
    const out = await this.enc.run({ mel: new ort.Tensor('float32', mel, [T, 80]) });
    let ck = out.cross_k, cv = out.cross_v;
    if (this.encGpu && !this.decGpu) {  // decoder on CPU: bring encoder output back
      const [a, b] = await Promise.all([ck.getData(true), cv.getData(true)]);
      ck = new ort.Tensor('float32', a, ck.dims); cv = new ort.Tensor('float32', b, cv.dims);
    }
    this.ck = ck; this.cv = cv;
    this.S = ck.dims[2];
    this.prev = new ort.Tensor('float32', new Float32Array(8 * 2 * 608), [8, 2, 608]);
    this.pk = new ort.Tensor('float32', new Float32Array(0), [8, 2, 0, 48]);
    this.pv = new ort.Tensor('float32', new Float32Array(0), [8, 2, 0, 64]);
    return this.S;
  }
  async step(tok, pos, efeat, wantAtt) {
    const { ort } = this;
    const feeds = {
      token: new ort.Tensor('int64', BigInt64Array.of(BigInt(tok)), [1]),
      pos: new ort.Tensor('int64', BigInt64Array.of(BigInt(pos)), [1]),
      efeat: new ort.Tensor('float32', efeat, [2, 4, 512]),
      prev: this.prev, past_k: this.pk, past_v: this.pv, cross_k: this.ck, cross_v: this.cv,
    };
    const o = await this.dec.run(feeds);
    this._free(this.prev, this.pk, this.pv);
    this.prev = o.prev_out; this.pk = o.present_k; this.pv = o.present_v;
    const logits = o.logits.location === 'gpu-buffer' ? await o.logits.getData(true) : o.logits.data;
    let xattn = null;
    if (wantAtt) xattn = o.xattn.location === 'gpu-buffer' ? await o.xattn.getData(true) : o.xattn.data.slice();
    else if (o.xattn.location === 'gpu-buffer') o.xattn.dispose();
    return { logits, xattn };
  }
  release() {
    const ts = [this.prev, this.pk, this.pv];
    this.prev = this.pk = this.pv = null;
    for (const t of ts) if (t && t.location === 'gpu-buffer') t.dispose();
    for (const t of [this.ck, this.cv]) if (t && t.location === 'gpu-buffer') t.dispose();
    this.ck = this.cv = null;
  }
}
