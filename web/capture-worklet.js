// Runs on the audio thread: forwards each block of microphone samples to the page.
class Capture extends AudioWorkletProcessor {
  process(inputs) {
    const channel = inputs[0] && inputs[0][0];
    if (channel && channel.length) this.port.postMessage(channel.slice());
    return true;
  }
}
registerProcessor("capture", Capture);
