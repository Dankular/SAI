// Whistle speech recognition in a module worker.
//
// Runs entirely in the page on onnxruntime-web's WASM (CPU) engine, so it uses no GPU
// memory and adds no server process. The ~17 MB weight pack is expanded into the ONNX
// external-data buffers in the browser, which is why only ~18 MB is stored on disk.
//
// Adapted from the reference worker in the Whistle ONNX release
// (huggingface.co/mrfakename/whistle-ONNX, Apache-2.0): WebGPU and CDN loading removed,
// local paths used. Host-side mel, tokenizer, engram lookup and decoding are that
// release's pack.js / whistle.js / backend.js, vendored unmodified.
import * as ort from "/vendor/ort/ort.wasm.min.mjs";
import { parsePack, buildModelData } from "/vendor/whistle/pack.js";
import { Tokenizer, Engram, transcribe } from "/vendor/whistle/whistle.js";
import { OrtBackend } from "/vendor/whistle/backend.js";

ort.env.wasm.wasmPaths = "/vendor/ort/";
// Multi-threading needs cross-origin isolation (the app sends COOP/COEP); otherwise 1 thread.
ort.env.wasm.numThreads = self.crossOriginIsolated ? Math.min(4, navigator.hardwareConcurrency || 4) : 1;

const BASE = "/models/whistle/";
let backend = null;
let ctx = null;
let info = {};
const post = (message) => postMessage(message);

async function fetchBytes(url) {
  const response = await fetch(url);
  if (!response.ok) throw new Error(`could not load ${url} (HTTP ${response.status})`);
  return new Uint8Array(await response.arrayBuffer());
}

function warmupAudio() {
  // 1.5 s of faint noise so the first real request is not the one that pays for set-up.
  const x = new Float32Array(24000);
  let s = 1;
  for (let i = 0; i < x.length; i++) { s = (s * 16807) % 2147483647; x[i] = (s / 2147483647 - 0.5) * 0.01; }
  return x;
}

async function init() {
  const t0 = performance.now();
  post({ type: "status", text: "Loading speech model" });
  const [packBytes, encGraph, decGraph] = await Promise.all([
    fetchBytes(BASE + "whistle.pack"),
    fetchBytes(BASE + "onnx/encoder.onnx"),
    fetchBytes(BASE + "onnx/decoder.onnx"),
  ]);
  const t1 = performance.now();
  const pack = parsePack(packBytes.buffer.slice(packBytes.byteOffset, packBytes.byteOffset + packBytes.length));
  const manifest = pack.man;
  const filterbank = new Float32Array(pack.buf.slice(pack.base + manifest.melfb.src, pack.base + manifest.melfb.src + 257 * 80 * 4));
  ctx = { tok: new Tokenizer(manifest.tok), fb: filterbank, engram: new Engram(pack) };
  const encData = buildModelData(pack, "encoder");
  const decData = buildModelData(pack, "decoder");
  const t2 = performance.now();
  const options = (name, data) => ({
    executionProviders: ["wasm"],
    graphOptimizationLevel: "all",
    externalData: [{ path: `${name}.onnx.data`, data }],
  });
  // Sequential: creating sessions concurrently can stall in onnxruntime-web.
  const enc = await ort.InferenceSession.create(encGraph, options("encoder", encData));
  const dec = await ort.InferenceSession.create(decGraph, options("decoder", decData));
  backend = new OrtBackend(ort, enc, dec, { encGpu: false, decGpu: false });
  const t3 = performance.now();
  await transcribe(backend, ctx, warmupAudio(), { language: "en", maxTokens: 4, timestamps: false });
  const t4 = performance.now();
  info = { threads: ort.env.wasm.numThreads, isolated: !!self.crossOriginIsolated, load_ms: Math.round(t1 - t0), expand_ms: Math.round(t2 - t1), session_ms: Math.round(t3 - t2), warmup_ms: Math.round(t4 - t3) };
  post({ type: "ready", info });
}

let queue = Promise.resolve();
onmessage = ({ data }) => {
  if (data.type === "init") {
    queue = init().catch((error) => { post({ type: "error", text: String(error.message || error) }); });
    return;
  }
  if (data.type === "transcribe") {
    queue = queue.then(async () => {
      try {
        const started = performance.now();
        const result = await transcribe(backend, ctx, data.pcm, { language: data.language || null, keywords: [], timestamps: false });
        post({ type: "result", id: data.id, text: (result.text || "").trim(), language: result.language, ms: Math.round(performance.now() - started) });
      } catch (error) {
        post({ type: "error", id: data.id, text: String(error.message || error) });
      }
    });
  }
};
