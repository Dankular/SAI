// Speech recognition: a thin promise wrapper around the Whistle worker (stt-worker.js).
// Everything runs in the page; see stt-worker.js for how.
export class Recognizer {
  constructor() {
    this.worker = null;
    this.ready = null;
    this.pending = new Map();
    this.nextId = 1;
    this.onStatus = null;
  }

  /** Starts loading the model (once). Resolves when it can transcribe. */
  start() {
    if (this.ready) return this.ready;
    this.ready = new Promise((resolve, reject) => {
      this.worker = new Worker("/stt-worker.js", { type: "module" });
      this.worker.onmessage = ({ data }) => {
        if (data.type === "status") this.onStatus?.(data.text);
        else if (data.type === "ready") resolve(data.info);
        else if (data.type === "result") { this.pending.get(data.id)?.resolve(data); this.pending.delete(data.id); }
        else if (data.type === "error") {
          const waiting = this.pending.get(data.id);
          if (waiting) { waiting.reject(new Error(data.text)); this.pending.delete(data.id); }
          else reject(new Error(data.text));
        }
      };
      this.worker.onerror = (event) => reject(new Error(event.message || "the speech worker failed"));
      this.worker.postMessage({ type: "init" });
    });
    // A failed start can be retried (for example while the model is still downloading).
    this.ready.catch(() => { this.worker?.terminate(); this.worker = null; this.ready = null; });
    return this.ready;
  }

  /** @param {Float32Array} pcm 16 kHz mono samples. Returns {text, language, ms}. */
  async transcribe(pcm) {
    await this.start();
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.worker.postMessage({ type: "transcribe", id, pcm }, [pcm.buffer]);
    });
  }

  stop() {
    this.worker?.terminate();
    this.worker = null;
    this.ready = null;
    for (const waiting of this.pending.values()) waiting.reject(new Error("stopped"));
    this.pending.clear();
  }
}
