'use strict';

/**
 * PCM-Sink zum Testen der Audio-Strecke OHNE die echte Go-Gegenseite.
 *
 * Simuliert die Go-seitige Vertragsrolle auf 127.0.0.1:7655 (dort ist der
 * Go-Prozess der TCP-Server, die GUI der Client - dieses Skript übernimmt
 * testweise die Server-Rolle): lauscht, zählt eingehende Bytes (rohes
 * PCM, s16le, 44100Hz, stereo, interleaved, kein Header) und gibt
 * periodisch die gemessene Datenrate sowie ein paar Beispiel-Samples aus.
 * Erwartete Rate bei aktivem Schalter: 44100 * 2 * 2 = 176400 Byte/s.
 *
 * Start: node tools/pcm-sink.js
 */

const net = require('net');

const PORT = 7655;
const HOST = '127.0.0.1';
const REPORT_INTERVAL_MS = 2000;
const BYTES_PER_SAMPLE = 2; // s16le
const CHANNELS = 2;
const SAMPLE_RATE = 44100;
const EXPECTED_BYTES_PER_SEC = SAMPLE_RATE * CHANNELS * BYTES_PER_SAMPLE;

let totalBytes = 0;
let windowBytes = 0;
let connectedAt = null;
let reportTimer = null;

const server = net.createServer((socket) => {
  console.log(`[pcm-sink] Client verbunden: ${socket.remoteAddress}:${socket.remotePort}`);
  connectedAt = Date.now();
  totalBytes = 0;
  windowBytes = 0;

  socket.on('data', (chunk) => {
    totalBytes += chunk.length;
    windowBytes += chunk.length;

    // Ein paar Beispiel-Samples aus diesem Chunk ausgeben (erste Stereo-
    // Frames), damit Stille (~0) vs. Ton (deutlich != 0) sichtbar ist.
    const frames = Math.min(4, Math.floor(chunk.length / (BYTES_PER_SAMPLE * CHANNELS)));
    const samples = [];
    for (let i = 0; i < frames; i++) {
      const l = chunk.readInt16LE(i * 4);
      const r = chunk.readInt16LE(i * 4 + 2);
      samples.push(`[${l},${r}]`);
    }
    if (samples.length) {
      console.log(`[pcm-sink] +${chunk.length}B  Beispiel-Samples: ${samples.join(' ')}`);
    }
  });

  socket.on('close', () => {
    const elapsedSec = connectedAt ? (Date.now() - connectedAt) / 1000 : 0;
    console.log(
      `[pcm-sink] Client getrennt. Gesamt: ${totalBytes} Byte über ${elapsedSec.toFixed(1)}s ` +
        `(Ø ${(totalBytes / Math.max(elapsedSec, 0.001)).toFixed(0)} B/s)`
    );
    connectedAt = null;
  });

  socket.on('error', (err) => {
    console.warn('[pcm-sink] Socket-Fehler:', err.message);
  });
});

reportTimer = setInterval(() => {
  if (!connectedAt) return;
  const rate = windowBytes / (REPORT_INTERVAL_MS / 1000);
  console.log(
    `[pcm-sink] Rate: ${rate.toFixed(0)} B/s (Soll: ${EXPECTED_BYTES_PER_SEC} B/s) | Gesamt seit Verbindung: ${totalBytes} B`
  );
  windowBytes = 0;
}, REPORT_INTERVAL_MS);

server.listen(PORT, HOST, () => {
  console.log(`[pcm-sink] lauscht auf ${HOST}:${PORT}`);
  console.log(`[pcm-sink] erwarte ${EXPECTED_BYTES_PER_SEC} B/s (44100Hz * 2 Kanäle * 2 Byte) bei aktivem Schalter "Ton übertragen"`);
});

server.on('error', (err) => {
  console.error('[pcm-sink] Fehler:', err.message);
  process.exit(1);
});

process.on('SIGINT', () => {
  clearInterval(reportTimer);
  server.close(() => process.exit(0));
});
