// The assistant's voice: turns streamed reply text into speech, plays it, and
// reports the playback level so the sphere can react to what is actually being
// said. Only the assistant's audio passes through here; the microphone is never
// connected to the analyser.

// Clean model output for speaking: drop code blocks, markdown markers and URLs.
export function speakable(text) {
  return text
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/`([^`]*)`/g, "$1")
    .replace(/!\[[^\]]*\]\([^)]*\)/g, " ")
    .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
    .replace(/https?:\/\/\S+/g, "a link")
    .replace(/^[#>\-*\s]+/gm, "")
    .replace(/[*_~|]+/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

// Splits a streaming reply into sentence-sized chunks so speech can start
// before the whole answer has been written.
export class SentenceSplitter {
  constructor(minLength = 28) { this.buffer = ""; this.minLength = minLength; this.inCode = false; }
  push(delta) {
    this.buffer += delta;
    const out = [];
    for (;;) {
      const fences = (this.buffer.match(/```/g) || []).length;
      if (fences % 2 === 1) break; // inside a code block: wait for it to close
      const match = /([.!?…]+["')\]]*\s+|\n{1,}\s*)/.exec(this.buffer.slice(this.minLength));
      if (!match) break;
      const end = this.minLength + match.index + match[0].length;
      const sentence = speakable(this.buffer.slice(0, end));
      this.buffer = this.buffer.slice(end);
      if (sentence) out.push(sentence);
    }
    return out;
  }
  flush() {
    const rest = speakable(this.buffer);
    this.buffer = "";
    return rest ? [rest] : [];
  }
}

export class AgentSpeech {
  /**
   * @param {{onLevel:(n:number)=>void, onSpeaking:(b:boolean)=>void, onUnavailable:(why:string)=>void}} hooks
   */
  constructor({ onLevel, onSpeaking, onUnavailable }) {
    this.onLevel = onLevel;
    this.onSpeaking = onSpeaking;
    this.onUnavailable = onUnavailable;
    this.ctx = null;
    this.analyser = null;
    this.samples = null;
    this.queue = [];
    this.playing = false;
    this.abort = new AbortController();
    this.unavailable = false;
    this.raf = 0;
    this.current = null;
  }

  /** Must be called from a user gesture before audio can play. */
  async prime() {
    if (!this.ctx) {
      const Ctx = window.AudioContext || window.webkitAudioContext;
      this.ctx = new Ctx();
      this.analyser = this.ctx.createAnalyser();
      this.analyser.fftSize = 1024;
      this.analyser.smoothingTimeConstant = 0.4;
      this.samples = new Float32Array(this.analyser.fftSize);
      this.spectrum = new Uint8Array(this.analyser.frequencyBinCount);
      this.analyser.connect(this.ctx.destination);
    }
    if (this.ctx.state === "suspended") await this.ctx.resume();
  }

  get busy() { return this.playing || this.queue.length > 0; }

  /** Queue a sentence. Synthesis starts immediately; playback stays in order. */
  speak(text) {
    if (!text || this.unavailable) return;
    const signal = this.abort.signal;
    const audio = this.synthesize(text, signal);
    audio.catch((error) => {
      if (signal.aborted) return;
      this.unavailable = true;
      this.onUnavailable?.(error.message);
    });
    this.queue.push({ text, audio });
    this.pump();
  }

  /** Fetch speech for one sentence, waiting out a voice server that is still warming up. */
  async synthesize(text, signal) {
    for (let attempt = 0; ; attempt++) {
      const response = await fetch("/api/voice/tts", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ text }),
        signal,
      });
      if (response.status === 503 && attempt < 40) {
        await new Promise((resolve) => setTimeout(resolve, 1500));
        continue;
      }
      if (!response.ok) throw new Error((await response.text()).trim() || response.statusText);
      return this.ctx.decodeAudioData(await response.arrayBuffer());
    }
  }

  async pump() {
    if (this.playing) return;
    this.playing = true;
    this.onSpeaking?.(true);
    this.startMeter();
    while (this.queue.length) {
      const item = this.queue.shift();
      let buffer;
      try { buffer = await item.audio; } catch { continue; }
      if (this.abort.signal.aborted) break;
      await new Promise((resolve) => {
        const source = this.ctx.createBufferSource();
        source.buffer = buffer;
        source.connect(this.analyser);
        source.onended = resolve;
        this.current = source;
        source.start();
      });
      this.current = null;
    }
    this.playing = false;
    this.stopMeter();
    this.onLevel?.(0, { bass: 0, mid: 0, treble: 0 });
    this.onSpeaking?.(false);
  }

  /** Stop speaking immediately (used when the user interrupts). */
  stop() {
    this.abort.abort();
    this.abort = new AbortController();
    this.queue = [];
    this.unavailable = false;
    try { this.current?.stop(); } catch { /* already stopped */ }
  }

  startMeter() {
    // Split the assistant's voice into bass / mid / treble so the sphere can
    // distort differently for vowels, body and sibilants.
    const band = (lowHz, highHz) => {
      const hz = this.ctx.sampleRate / this.analyser.fftSize;
      const from = Math.max(1, Math.floor(lowHz / hz));
      const to = Math.min(this.spectrum.length - 1, Math.ceil(highHz / hz));
      let sum = 0;
      for (let i = from; i <= to; i++) sum += this.spectrum[i];
      return sum / ((to - from + 1) * 255);
    };
    const tick = () => {
      this.analyser.getFloatTimeDomainData(this.samples);
      this.analyser.getByteFrequencyData(this.spectrum);
      let sum = 0;
      for (let i = 0; i < this.samples.length; i++) sum += this.samples[i] * this.samples[i];
      const rms = Math.sqrt(sum / this.samples.length);
      const clamp = (n) => Math.max(0, Math.min(1, n));
      this.onLevel?.(clamp(rms * 6), {
        bass: clamp(band(60, 250) * 1.7),
        mid: clamp(band(250, 2000) * 2.0),
        treble: clamp(band(2000, 7000) * 3.4),
      });
      this.raf = requestAnimationFrame(tick);
    };
    cancelAnimationFrame(this.raf);
    this.raf = requestAnimationFrame(tick);
  }

  stopMeter() { cancelAnimationFrame(this.raf); }
}
