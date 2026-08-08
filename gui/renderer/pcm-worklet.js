'use strict';

/**
 * AudioWorkletProcessor, der System-Audio (aus getDisplayMedia({ audio:
 * 'loopback' })) in rohes PCM für den Go-Sidecar umwandelt:
 * Float32 (getrennt nach Kanal, Rendering-Quantum = 128 Samples) ->
 * interleaved Int16 (s16le), gesammelt in ~20ms-Blöcken (882 Samples/
 * Kanal bei 44100Hz = 3528 Byte), damit nicht 128-Sample-Häppchen einzeln
 * per postMessage/IPC verschickt werden müssen.
 *
 * Die Float->Int16-Konvertierung passiert bewusst hier im Worklet-Thread
 * und nicht im Main-Thread des Renderers, damit UI-Last keine Aussetzer
 * im Ton verursacht.
 */
class PcmEncoderProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    // sampleRate ist eine globale Konstante im AudioWorkletGlobalScope
    // (== die sampleRate des AudioContext, hier explizit 44100).
    this.targetSamplesPerChannel = Math.round(sampleRate * 0.02); // ~20ms

    // Etwas Reserve über dem Zielwert, damit ein einzelnes Rendering-
    // Quantum (i.d.R. 128 Samples) den Puffer nie überlaufen lässt, bevor
    // flush() zum Zug kommt.
    const capacity = this.targetSamplesPerChannel + 256;
    this.left = new Float32Array(capacity);
    this.right = new Float32Array(capacity);
    this.filled = 0;
  }

  process(inputs) {
    const input = inputs[0];
    if (!input || input.length === 0 || !input[0] || input[0].length === 0) {
      // (Noch) kein Signal verbunden - Prozessor am Leben halten.
      return true;
    }

    const chL = input[0];
    const chR = input.length > 1 ? input[1] : input[0]; // Mono-Quelle auf beide Kanäle duplizieren
    const n = chL.length;

    if (this.filled + n > this.left.length) {
      this.flush();
    }

    this.left.set(chL, this.filled);
    this.right.set(chR, this.filled);
    this.filled += n;

    if (this.filled >= this.targetSamplesPerChannel) {
      this.flush();
    }

    return true;
  }

  flush() {
    const n = this.filled;
    if (n === 0) return;

    const out = new Int16Array(n * 2);
    for (let i = 0; i < n; i++) {
      let l = this.left[i];
      if (l > 1) l = 1;
      else if (l < -1) l = -1;
      let r = this.right[i];
      if (r > 1) r = 1;
      else if (r < -1) r = -1;
      out[i * 2] = Math.round(l * 32767);
      out[i * 2 + 1] = Math.round(r * 32767);
    }

    this.filled = 0;
    // Transferable - kein Kopieren beim Übergang zum Main-Thread.
    this.port.postMessage(out.buffer, [out.buffer]);
  }
}

registerProcessor('pcm-encoder', PcmEncoderProcessor);
