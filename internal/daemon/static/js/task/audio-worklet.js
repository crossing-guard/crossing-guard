// AudioWorkletProcessor that forwards mono float frames to the main thread.
// It does no resampling and keeps no state beyond a frame buffer; the main
// thread owns energy, silence, and encoding.

class DictationCaptureProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.buffer = [];
    this.buffered = 0;
    this.target = 2048;
  }

  process(inputs) {
    const channel = inputs[0]?.[0];
    if (!channel) return true;
    this.buffer.push(Float32Array.from(channel));
    this.buffered += channel.length;
    if (this.buffered >= this.target) {
      const merged = new Float32Array(this.buffered);
      let offset = 0;
      for (const chunk of this.buffer) { merged.set(chunk, offset); offset += chunk.length; }
      this.port.postMessage(merged, [merged.buffer]);
      this.buffer = [];
      this.buffered = 0;
    }
    return true;
  }
}

registerProcessor('dictation-capture', DictationCaptureProcessor);
