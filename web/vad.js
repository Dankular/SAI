// Voice activity detection in the page, using TEN VAD (WebAssembly, ~280 KB).
// TEN VAD is by the TEN Framework authors, Apache-2.0 with additional conditions;
// see vendor/tenvad/LICENSE.
//
// It looks at 16 kHz mono audio in hops of 256 samples (16 ms) and says, for each
// hop, how likely it is that someone is speaking.
import createVADModule from "/vendor/tenvad/ten_vad.js";

export const VAD_RATE = 16000;
export const VAD_HOP = 256;

export async function createVad({ threshold = 0.5 } = {}) {
  const module = await createVADModule();
  const handleBox = module._malloc(4);
  if (module._ten_vad_create(handleBox, VAD_HOP, threshold) !== 0) throw new Error("TEN VAD could not start");
  const handle = new Int32Array(module.HEAP32.buffer, handleBox, 1)[0];
  const audioPtr = module._malloc(VAD_HOP * 2);
  const probPtr = module._malloc(4);
  const flagPtr = module._malloc(4);

  return {
    /** @param {Int16Array} hop exactly VAD_HOP samples. Returns {probability, voice}. */
    process(hop) {
      // WebAssembly memory can grow, so views are taken fresh on every call.
      new Int16Array(module.HEAP16.buffer, audioPtr, VAD_HOP).set(hop);
      if (module._ten_vad_process(handle, audioPtr, VAD_HOP, probPtr, flagPtr) !== 0) return { probability: 0, voice: false };
      return {
        probability: new Float32Array(module.HEAPF32.buffer, probPtr, 1)[0],
        voice: new Int32Array(module.HEAP32.buffer, flagPtr, 1)[0] === 1,
      };
    },
    destroy() {
      module._ten_vad_destroy(handleBox);
      module._free(audioPtr); module._free(probPtr); module._free(flagPtr); module._free(handleBox);
    },
  };
}
