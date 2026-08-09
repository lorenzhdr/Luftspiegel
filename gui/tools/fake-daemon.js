'use strict';

/**
 * Fake-Daemon für die Luftspiegel-GUI.
 *
 * Simuliert den Control-Channel des Go-Sidecars auf 127.0.0.1:7654:
 * eine TCP-Verbindung pro Befehl, eine Zeile JSON rein, eine Zeile
 * JSON-Antwort raus, Verbindung schließt.
 *
 * Nützlich, um die GUI ohne den echten (evtl. noch nicht TCP-fähigen)
 * Go-Daemon zu testen. Unterstützt discover/devices/connect/disconnect/
 * mute/unmute/status inkl. eines PIN-Falls (Ziel-IP "192.168.178.200"
 * verlangt beim ersten connect-Versuch eine PIN "1234").
 *
 * "discover" antwortet absichtlich VERZÖGERT (Standard ~1.5s, siehe
 * DISCOVER_DELAY_MS) und liefert die Geräte direkt in derselben Antwort
 * mit - das bildet nach, wie der echte Daemon perspektivisch arbeiten
 * soll (synchron warten, bis der mDNS/Fallback-Scan fertig ist, der bei
 * aktivem VPN auf dieser Maschine real bis zu ~5s dauern kann).
 *
 * Start: node tools/fake-daemon.js
 * Simuliert "nichts gefunden" (z.B. VPN-Fall): node tools/fake-daemon.js --empty
 * Verzögerung anpassen: DISCOVER_DELAY_MS=3000 node tools/fake-daemon.js
 */

const net = require('net');

const PORT = 7654;
const HOST = '127.0.0.1';
const DISCOVER_DELAY_MS = Number(process.env.DISCOVER_DELAY_MS) || 1500;
const EMPTY_MODE = process.argv.includes('--empty');

const FAKE_DEVICES = EMPTY_MODE
  ? []
  : [
      { name: 'h264zyyx', model: 'AppleTV11,1', ip: '192.168.178.125', port: 7000, device_id: 'AA:BB:CC:DD:EE:01' },
      { name: 'Wohnzimmer', model: 'AppleTV14,1', ip: '192.168.178.200', port: 7000, device_id: 'AA:BB:CC:DD:EE:02' },
    ];

/** @type {{state:string, device:string, device_ip:string, has_audio:boolean, audio_muted:boolean}[]} */
let streams = [];

// Merkt sich, welche IPs bereits eine PIN eingegeben haben (simuliert
// den needs_pin-Fall nur beim ersten Verbindungsversuch je Ziel).
const pinConfirmed = new Set();

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// ---------------------------------------------------------------------------
// stats-Simulation (siehe GUI-Vertrag: stats.history in 500ms-Auflösung,
// bis zu 120 Punkte = 60s, ältester zuerst). Läuft als eigener Timer,
// solange mindestens ein Stream aktiv ist, damit der Statistik-Tab der GUI
// auch ohne echten Go-Daemon plausible, sich leicht ändernde Werte zum
// Testen bekommt.
let statsHistory = [];
let statsTimer = null;
let statsStartedAt = null;

function statsSample() {
  const fps = 28 + Math.random() * 4;
  const bitrate_kbps = 3800 + Math.random() * 1200;
  const au_hold_ms = 4 + Math.random() * 8;
  return {
    fps: Number(fps.toFixed(1)),
    bitrate_kbps: Number(bitrate_kbps.toFixed(1)),
    au_hold_ms: Number(au_hold_ms.toFixed(1)),
  };
}

function startStatsSimulation() {
  if (statsTimer) return;
  statsStartedAt = Date.now();
  statsHistory = [];
  statsTimer = setInterval(() => {
    statsHistory.push(statsSample());
    if (statsHistory.length > 120) statsHistory.shift();
  }, 500);
}

function stopStatsSimulation() {
  if (statsTimer) {
    clearInterval(statsTimer);
    statsTimer = null;
  }
  statsHistory = [];
  statsStartedAt = null;
}

/** Baut einen kompletten stats-Block nach dem in der GUI dokumentierten Schema, oder null ohne aktiven Stream. */
function buildStats() {
  if (!statsStartedAt) return null;
  const uptime_sec = (Date.now() - statsStartedAt) / 1000;
  const latest = statsHistory.length ? statsHistory[statsHistory.length - 1] : statsSample();
  const bytesPerSec = (latest.bitrate_kbps * 1000) / 8;

  return {
    uptime_sec: Number(uptime_sec.toFixed(1)),
    frames_sent: Math.round(uptime_sec * latest.fps),
    keyframes: Math.max(1, Math.round(uptime_sec / 4)),
    bytes_sent: Math.round(bytesPerSec * uptime_sec),
    fps: latest.fps,
    bitrate_kbps: latest.bitrate_kbps,
    au_hold_ms_p50: latest.au_hold_ms,
    au_hold_ms_p95: Number((latest.au_hold_ms * 1.8).toFixed(1)),
    socket_write_ms_p50: 0.4,
    socket_write_ms_p95: 2.9,
    rtt_ms: Number((2 + Math.random() * 3).toFixed(1)),
    receiver_render_latency_ms: 62.0,
    audio_frames_sent: Math.round(uptime_sec * 50),
    audio_buffer_ms: 118.3,
    audio_underruns: 2,
    audio_silence_ms: 41.0,
    audio_drain_ms: 180.0,
    width: 1728,
    height: 1080,
    encoder: 'h264_mf',
    rate_control: 'display_remoting',
    target_latency_ms: 100,
    // ältester zuerst, wie vom Daemon-Vertrag gefordert - statsHistory wird
    // in derselben Reihenfolge (push ans Ende) aufgebaut.
    history: statsHistory.slice(),
  };
}

function baseResponse(extra) {
  return Object.assign(
    {
      ok: true,
      state: streams.length ? streams[streams.length - 1].state : 'getrennt',
      device: streams.length ? streams[streams.length - 1].device : '',
      device_ip: streams.length ? streams[streams.length - 1].device_ip : '',
      has_audio: false,
      audio_muted: false,
      needs_pin: false,
      error: '',
      devices: FAKE_DEVICES,
      streams,
      // Bewusst undefined statt null, wenn kein Stream aktiv ist -
      // JSON.stringify lässt undefined-Felder ganz weg, das Feld fehlt dann
      // komplett in der Antwort (siehe Vertrag: "fehlt das Feld").
      stats: streams.length ? buildStats() : undefined,
    },
    extra
  );
}

/** Kann ein einfaches Objekt ODER eine Promise davon zurückgeben (für die verzögerte discover-Antwort). */
function handleCommand(cmd) {
  console.log('[fake-daemon] Befehl:', JSON.stringify(cmd));

  switch (cmd.cmd) {
    case 'status':
      return baseResponse({});

    case 'discover':
      // Simuliert den mDNS+Fallback-Scan des echten Daemons: synchron
      // warten, dann die Geräte direkt in der discover-Antwort liefern.
      return delay(DISCOVER_DELAY_MS).then(() => {
        console.log(
          `[fake-daemon] discover abgeschlossen nach ${DISCOVER_DELAY_MS}ms - ${FAKE_DEVICES.length} Gerät(e) gefunden`
        );
        return baseResponse({});
      });

    case 'devices':
      return baseResponse({});

    case 'connect': {
      const target = cmd.target || (FAKE_DEVICES[0] && FAKE_DEVICES[0].ip);
      const dev = FAKE_DEVICES.find((d) => d.ip === target) || { name: target, ip: target };

      // Simuliert: Gerät "Wohnzimmer" (192.168.178.200) verlangt beim
      // ersten Verbindungsversuch eine PIN.
      if (target === '192.168.178.200' && !pinConfirmed.has(target)) {
        if (!cmd.pin) {
          return baseResponse({ needs_pin: true, state: 'wartet auf pin', device: dev.name, device_ip: target });
        }
        if (cmd.pin !== '1234') {
          return { ok: false, error: 'Falsche PIN.', needs_pin: true, devices: FAKE_DEVICES, streams };
        }
        pinConfirmed.add(target);
      }

      // Simuliert: unbekannte/nicht erreichbare IP schlägt fehl.
      if (target === '10.10.10.10') {
        return { ok: false, error: 'Gerät nicht erreichbar (Timeout).', devices: FAKE_DEVICES, streams };
      }

      streams = streams.filter((s) => s.device_ip !== target);
      streams.push({ device: dev.name || target, device_ip: target, state: 'verbunden', has_audio: false, audio_muted: false });
      startStatsSimulation();

      return baseResponse({ state: 'verbunden', device: dev.name || target, device_ip: target });
    }

    case 'disconnect': {
      if (cmd.target) {
        streams = streams.filter((s) => s.device_ip !== cmd.target);
      } else {
        streams = [];
      }
      if (streams.length === 0) stopStatsSimulation();
      return baseResponse({ state: 'getrennt', device: '', device_ip: '' });
    }

    case 'mute': {
      const s = streams.find((x) => x.device_ip === cmd.target);
      if (s) s.audio_muted = true;
      return baseResponse({});
    }

    case 'unmute': {
      const s = streams.find((x) => x.device_ip === cmd.target);
      if (s) s.audio_muted = false;
      return baseResponse({});
    }

    default:
      return { ok: false, error: `Unbekannter Befehl: ${cmd.cmd}`, devices: FAKE_DEVICES, streams };
  }
}

const server = net.createServer((socket) => {
  let buffer = '';

  socket.on('data', (chunk) => {
    buffer += chunk.toString('utf-8');
    const nl = buffer.indexOf('\n');
    if (nl === -1) return;

    const line = buffer.slice(0, nl);
    let responseOrPromise;
    try {
      const cmd = JSON.parse(line);
      responseOrPromise = handleCommand(cmd);
    } catch (err) {
      responseOrPromise = { ok: false, error: 'Ungültiges JSON empfangen.' };
    }

    Promise.resolve(responseOrPromise).then((response) => {
      if (socket.destroyed) return;
      socket.write(JSON.stringify(response) + '\n');
      socket.end();
    });
  });

  socket.on('error', () => {
    /* Client hat evtl. abrupt getrennt - ignorieren */
  });
});

server.listen(PORT, HOST, () => {
  console.log(`[fake-daemon] lauscht auf ${HOST}:${PORT}`);
  if (EMPTY_MODE) {
    console.log('[fake-daemon] --empty aktiv: discover/devices liefert bewusst keine Geräte (VPN-Simulation).');
  } else {
    console.log('[fake-daemon] Bekannte Geräte:', FAKE_DEVICES.map((d) => `${d.name} (${d.ip})`).join(', '));
    console.log('[fake-daemon] PIN-Testfall: connect auf 192.168.178.200 (PIN "1234")');
    console.log('[fake-daemon] Fehler-Testfall: connect auf 10.10.10.10');
  }
  console.log(`[fake-daemon] discover-Verzögerung: ${DISCOVER_DELAY_MS}ms`);
});

server.on('error', (err) => {
  console.error('[fake-daemon] Fehler:', err.message);
  process.exit(1);
});
