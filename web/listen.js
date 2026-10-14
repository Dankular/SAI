// Hands-free listening: microphone -> TEN VAD -> Smart Turn -> Whistle -> text.
//
//   1. TEN VAD marks each 16 ms of microphone audio as speech or not.
//   2. After a short pause, Smart Turn (server side, ~0.1 s) judges whether the
//      speaker has finished a thought or is just pausing. If they are still going,
//      we keep waiting instead of cutting them off.
//   3. The finished utterance goes to Whistle (in this page) and the text is handed
//      to the app, which sends it to the assistant.
//   4. If speech starts while the assistant is talking, that is a barge-in: the app
//      is told so it can stop the assistant at once.
//
// The microphone is used for listening only; nothing here drives the sphere.
import { createVad, VAD_HOP, VAD_RATE } from "/vad.js";

const HOP_MS = (VAD_HOP / VAD_RATE) * 1000;        // 16 ms
const PREROLL_HOPS = Math.round(400 / HOP_MS);      // keep 400 ms before speech starts
const START_HOPS = 3;                               // ~48 ms of voice starts an utterance
const BARGE_IN_HOPS = 10;                           // ~160 ms while the assistant is talking, to ignore echo
const MIN_SPEECH_MS = 300;                          // shorter than this is a cough or a click
const PAUSE_MS = 400;                               // silence before asking "is the turn over?"
const RECHECK_MS = 350;                             // ask again this often while the pause continues
const MAX_PAUSE_MS = 1800;                          // never wait longer than this for a hesitating speaker
const FALLBACK_PAUSE_MS = 900;                      // if turn detection is unavailable, end after this much silence
const TURN_WINDOW_S = 8;                            // Smart Turn looks at the last 8 seconds

function concat(chunks) {
  let length = 0;
  for (const c of chunks) length += c.length;
  const out = new Float32Array(length);
  let offset = 0;
  for (const c of chunks) { out.set(c, offset); offset += c.length; }
  return out;
}

function toWav16k(float32) {
  const bytes = new ArrayBuffer(44 + float32.length * 2);
  const view = new DataView(bytes);
  const text = (offset, s) => { for (let i = 0; i < s.length; i++) view.setUint8(offset + i, s.charCodeAt(i)); };
  text(0, "RIFF"); view.setUint32(4, 36 + float32.length * 2, true); text(8, "WAVE");
  text(12, "fmt "); view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true);
  view.setUint32(24, VAD_RATE, true); view.setUint32(28, VAD_RATE * 2, true); view.setUint16(32, 2, true); view.setUint16(34, 16, true);
  text(36, "data"); view.setUint32(40, float32.length * 2, true);
  for (let i = 0; i < float32.length; i++) {
    const s = Math.max(-1, Math.min(1, float32[i]));
    view.setInt16(44 + i * 2, s < 0 ? s * 0x8000 : s * 0x7fff, true);
  }
  return new Blob([bytes], { type: "audio/wav" });
}

export class HandsFree {
  /**
   * @param {object} o
   * @param {import("/stt.js").Recognizer} o.recognizer
   * @param {() => boolean} o.assistantBusy  true while the assistant is speaking or answering
   * @param {(s: string, detail?: object) => void} o.onStatus  idle | hearing | checking | transcribing | ignored
   * @param {(rms: number) => void} [o.onLevel]
   * @param {() => void} o.onSpeechStart
   * @param {() => void} o.onBargeIn
   * @param {(text: string) => void} o.onTranscript
   * @param {(error: Error) => void} o.onError
   */
  constructor(o) {
    Object.assign(this, o);
    this.running = false;
    this.context = null;
    this.stream = null;
    this.vad = null;
    this.reset();
  }

  reset() {
    this.ring = [];
    this.inSpeech = false;
    this.voicedRun = 0;
    this.silentHops = 0;
    this.speechHops = 0;
    this.utterance = [];
    this.epoch = 0;            // bumps whenever voice resumes, so stale turn checks are ignored
    this.checking = false;
    this.lastCheckAt = -1;
    this.turnAvailable = true;
    this.pending = new Float32Array(0);
  }

  async start() {
    if (this.running) return;
    this.vad = await createVad({ threshold: 0.5 });
    this.stream = await navigator.mediaDevices.getUserMedia({
      audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true },
    });
    // Ask for a 16 kHz context so no resampling is needed; fall back to resampling ourselves.
    try { this.context = new AudioContext({ sampleRate: VAD_RATE }); } catch { this.context = new AudioContext(); }
    await this.context.audioWorklet.addModule("/capture-worklet.js");
    const source = this.context.createMediaStreamSource(this.stream);
    this.node = new AudioWorkletNode(this.context, "capture");
    this.node.port.onmessage = (event) => this.push(event.data);
    source.connect(this.node); // not connected to the speakers: the microphone is never played back
    this.running = true;
    this.reset();
    this.onStatus("idle");
  }

  stop() {
    this.running = false;
    try { this.node?.disconnect(); } catch { /* already gone */ }
    this.stream?.getTracks().forEach((track) => track.stop());
    this.context?.close().catch(() => {});
    this.vad?.destroy();
    this.node = this.stream = this.context = this.vad = null;
    this.reset();
  }

  // ---- audio plumbing -------------------------------------------------------
  push(block) {
    if (!this.running) return;
    const samples = this.context.sampleRate === VAD_RATE ? block : this.resample(block);
    const merged = new Float32Array(this.pending.length + samples.length);
    merged.set(this.pending); merged.set(samples, this.pending.length);
    let offset = 0;
    while (merged.length - offset >= VAD_HOP) {
      this.hop(merged.slice(offset, offset + VAD_HOP));
      offset += VAD_HOP;
    }
    this.pending = merged.slice(offset);
  }

  resample(block) {
    const ratio = this.context.sampleRate / VAD_RATE;
    const out = new Float32Array(Math.floor(block.length / ratio));
    for (let i = 0; i < out.length; i++) out[i] = block[Math.floor(i * ratio)];
    return out;
  }

  // ---- one 16 ms hop --------------------------------------------------------
  hop(floats) {
    const ints = new Int16Array(VAD_HOP);
    let energy = 0;
    for (let i = 0; i < VAD_HOP; i++) {
      const s = Math.max(-1, Math.min(1, floats[i]));
      ints[i] = s < 0 ? s * 0x8000 : s * 0x7fff;
      energy += s * s;
    }
    this.onLevel?.(Math.sqrt(energy / VAD_HOP));
    const { voice } = this.vad.process(ints);

    if (!this.inSpeech) {
      this.ring.push(floats);
      if (this.ring.length > PREROLL_HOPS) this.ring.shift();
      this.voicedRun = voice ? this.voicedRun + 1 : 0;
      const needed = this.assistantBusy() ? BARGE_IN_HOPS : START_HOPS;
      if (this.voicedRun >= needed) {
        const interrupting = this.assistantBusy();
        this.inSpeech = true;
        this.utterance = this.ring.slice();
        this.speechHops = this.voicedRun;
        this.silentHops = 0;
        this.lastCheckAt = -1;
        this.onStatus("hearing");
        this.onSpeechStart();
        if (interrupting) this.onBargeIn();
      }
      return;
    }

    this.utterance.push(floats);
    if (voice) {
      this.silentHops = 0;
      this.speechHops++;
      this.epoch++;
      this.lastCheckAt = -1;
      if (!this.checking) this.onStatus("hearing");
      return;
    }
    this.silentHops++;
    const silentMs = this.silentHops * HOP_MS;
    if (silentMs >= MAX_PAUSE_MS) return void this.finish();
    if (!this.turnAvailable && silentMs >= FALLBACK_PAUSE_MS) return void this.finish();
    const due = this.lastCheckAt < 0 ? silentMs >= PAUSE_MS : silentMs - this.lastCheckAt >= RECHECK_MS;
    if (this.turnAvailable && due && !this.checking) this.checkTurn(silentMs);
  }

  // ---- is the turn over? ----------------------------------------------------
  async checkTurn(silentMs) {
    this.checking = true;
    this.lastCheckAt = silentMs;
    const epoch = this.epoch;
    this.onStatus("checking");
    try {
      const recent = concat(this.utterance);
      const tail = recent.length > TURN_WINDOW_S * VAD_RATE ? recent.slice(recent.length - TURN_WINDOW_S * VAD_RATE) : recent;
      const response = await fetch("/api/voice/turn", { method: "POST", headers: { "Content-Type": "audio/wav" }, body: toWav16k(tail) });
      if (!response.ok) throw new Error("turn detection unavailable");
      const verdict = await response.json();
      if (this.inSpeech && epoch === this.epoch) {
        if (verdict.complete) this.finish();
        else this.onStatus("hearing", { thinking: true });
      }
    } catch {
      this.turnAvailable = false; // fall back to plain silence timing
    } finally {
      this.checking = false;
    }
  }

  // ---- end of turn: transcribe ----------------------------------------------
  async finish() {
    if (!this.inSpeech) return;
    this.inSpeech = false;
    const speechMs = this.speechHops * HOP_MS;
    const hops = this.utterance;
    this.utterance = [];
    this.ring = [];
    this.voicedRun = 0;
    if (speechMs < MIN_SPEECH_MS) { this.onStatus("ignored"); this.onStatus("idle"); return; }
    // Drop most of the trailing silence, keeping a little for the model.
    const keepTail = Math.round(200 / HOP_MS);
    const trimmed = this.silentHops > keepTail ? hops.slice(0, hops.length - (this.silentHops - keepTail)) : hops;
    this.onStatus("transcribing");
    try {
      const started = performance.now();
      const { text } = await this.recognizer.transcribe(concat(trimmed));
      console.debug(`[listen] transcribed ${Math.round(speechMs)} ms of speech in ${Math.round(performance.now() - started)} ms`);
      const heard = (text || "").trim();
      if (heard.replace(/[^\p{L}\p{N}]/gu, "").length < 2) { this.onStatus("ignored"); }
      else this.onTranscript(heard);
    } catch (error) {
      this.onError(error);
    } finally {
      if (this.running && !this.inSpeech) this.onStatus("idle");
    }
  }
}
