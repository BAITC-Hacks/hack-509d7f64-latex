// AudioWorklet: batches mic samples (already 16 kHz thanks to the AudioContext sampleRate) into ~100 ms Int16 chunks.
class PcmCapture extends AudioWorkletProcessor {
  constructor() { super(); this.buf = new Float32Array(1600); this.n = 0; }
  process(inputs) {
    const ch = inputs[0] && inputs[0][0];
    if (!ch) return true;
    let rms = 0;
    for (let i = 0; i < ch.length; i++) {
      this.buf[this.n++] = ch[i];
      rms += ch[i] * ch[i];
      if (this.n === this.buf.length) {
        const out = new Int16Array(this.n);
        for (let j = 0; j < this.n; j++) { const v = Math.max(-1, Math.min(1, this.buf[j])); out[j] = v < 0 ? v * 32768 : v * 32767; }
        this.port.postMessage({ pcm: out.buffer, rms: Math.sqrt(rms / ch.length) }, [out.buffer]);
        this.n = 0;
      }
    }
    return true;
  }
}
registerProcessor('pcm-capture', PcmCapture);
