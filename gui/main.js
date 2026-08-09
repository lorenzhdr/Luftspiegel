'use strict';

const { app, BrowserWindow, ipcMain, screen, session, desktopCapturer, Menu, Tray, nativeImage, shell } = require('electron');
const path = require('path');
const fs = require('fs');
const net = require('net');
const { spawn } = require('child_process');

// ---------------------------------------------------------------------------
// Konstanten
// ---------------------------------------------------------------------------

const CONTROL_HOST = '127.0.0.1';
const CONTROL_PORT = 7654;

// TCP-Gegenstelle für rohes PCM (s16le, 44100 Hz, stereo, kein Header):
// der Go-Sidecar ist hier der Server, die GUI verbindet sich als Client.
const AUDIO_HOST = '127.0.0.1';
const AUDIO_PORT = 7655;
const AUDIO_RECONNECT_MIN_MS = 500;
const AUDIO_RECONNECT_MAX_MS = 8000;

// status wird schneller gepollt, solange gestreamt wird (Statistik-Tab
// braucht frischere Werte), sonst reicht der träge 2s-Rhythmus. Der
// devices-Poll bleibt bewusst unabhängig davon immer bei 2s.
const STATUS_POLL_INTERVAL_IDLE_MS = 2000;
const STATUS_POLL_INTERVAL_STREAMING_MS = 1000;
const DEVICES_POLL_INTERVAL_MS = 2000;
const SIDECAR_READY_TIMEOUT_MS = 6000;
const SIDECAR_READY_POLL_MS = 350;
const CONNECT_TIMEOUT_MS = 15000;
const DISCONNECT_TIMEOUT_MS = 8000;
const DISCOVER_TIMEOUT_MS = 12000; // Netzwerk-Scan-Fallback (z.B. bei aktivem VPN) kann bis zu ~5s dauern
const DEFAULT_TIMEOUT_MS = 3000;

const SETTINGS_FILE = 'settings.json';

// Die Qualitäts-Presets (welche Werte "Niedrigste Latenz" / "Ausgewogen" /
// "Beste Qualität" setzen) sind reine UI-Logik und leben daher im Renderer
// (app.js) - main.js validiert nur die resultierenden Einzelwerte.

const DEFAULT_SETTINGS = {
  fps: 30,
  maxHeight: 1080, // 0 = native
  outputIndex: 0,
  bitrate: 0, // 0 = automatisch
  audioEnabled: false, // Video ist erprobt, Audio ist neu - Default aus

  // Qualitäts-Preset
  preset: 'balanced',
  expertMode: false,

  // Latenz & Encoder (nur im Expertenmodus editierbar, sonst vom Preset gesetzt)
  targetLatencyMs: 100,
  gopSeconds: 4,
  rateControl: 'display_remoting',
  hwaccel: 'auto',
  audioBufferMs: 120,

  // Video
  showCursor: true,

  // App-Verhalten (reine UI-Einstellungen, kein Sidecar-Neustart)
  rememberLastDevice: true,
  autoConnect: false,
  startMinimized: false,
  trayIcon: false,
  debugLogging: false,

  // Interner UI-Zustand
  activeTab: 'connect',
  lastDevice: null, // { name, ip, port } - nur gesetzt, wenn rememberLastDevice aktiv ist
};

const BACKGROUND_COLOR = '#0f1417';

// ---------------------------------------------------------------------------
// Globaler Zustand
// ---------------------------------------------------------------------------

let mainWindow = null;
let statusPollTimer = null; // setTimeout-Kette (dynamisches Intervall, siehe startStatusPolling)
let statusPollActive = false;
let statusPollInFlight = false; // Overlap-Schutz: ein Tick wird übersprungen, solange der vorige noch offen ist
let devicesPollTimer = null; // eigener setInterval, entkoppelt vom status-Poll
let settings = { ...DEFAULT_SETTINGS };
let lastSidecarError = null;
let tray = null;
let logStream = null;

/**
 * TCP-Client-Verbindung zum Audio-Port des Go-Sidecars (127.0.0.1:7655).
 * Wird nur gehalten, während der Sidecar läuft/adoptiert ist UND
 * settings.audioEnabled true ist (siehe syncAudioBridgeWanted). Reißt die
 * Verbindung ab, wird mit exponentiellem Backoff erneut versucht, statt
 * aufzugeben - ein Abbruch hier darf die App nie zum Absturz bringen und
 * das Video (das über den Go-Sidecar separat läuft) nicht beeinträchtigen.
 */
const audioBridge = {
  socket: null,
  wantConnected: false,
  reconnectTimer: null,
  reconnectDelay: AUDIO_RECONNECT_MIN_MS,
  writable: false, // false während socket.write() zuletzt Rückstau meldete
  state: 'disconnected', // 'disconnected' | 'connecting' | 'connected' | 'error'
  droppedChunks: 0, // Blöcke, die verworfen wurden (keine Verbindung/Rückstau/Größenlimit) - siehe writeAudioChunk
};

const sidecar = {
  child: null,
  starting: false,
  stopping: false,
  path: null,
  // true, wenn bereits vor dem eigenen Start ein Daemon auf 7654 antwortete
  // (extern gestarteter Prozess oder Test-/Fake-Daemon) - dann verwaltet die
  // GUI den Prozess nicht selbst (kein eigenständiges Kill, kein Neustart
  // bei Einstellungsänderung).
  adopted: false,
};

// ---------------------------------------------------------------------------
// Einstellungen (persistiert unter userData/settings.json)
// ---------------------------------------------------------------------------

function settingsFilePath() {
  return path.join(app.getPath('userData'), SETTINGS_FILE);
}

function logDirPath() {
  return path.join(app.getPath('userData'), 'logs');
}

/** Öffnet (bzw. legt neu an) die Log-Datei, in die Sidecar-stdout/stderr fortlaufend gespiegelt wird. */
function openLogStream() {
  try {
    fs.mkdirSync(logDirPath(), { recursive: true });
    logStream = fs.createWriteStream(path.join(logDirPath(), 'sidecar.log'), { flags: 'a' });
  } catch (err) {
    console.error('[log] Konnte Log-Datei nicht öffnen:', err);
    logStream = null;
  }
}

function loadSettings() {
  try {
    const raw = fs.readFileSync(settingsFilePath(), 'utf-8');
    const parsed = JSON.parse(raw);
    settings = sanitizeSettings(parsed);
  } catch (err) {
    settings = { ...DEFAULT_SETTINGS };
  }
  return settings;
}

function persistSettings() {
  try {
    fs.mkdirSync(path.dirname(settingsFilePath()), { recursive: true });
    fs.writeFileSync(settingsFilePath(), JSON.stringify(settings, null, 2), 'utf-8');
  } catch (err) {
    console.error('[settings] Konnte Einstellungen nicht speichern:', err);
  }
}

/** Ganzzahl aus src[key] im Bereich [min,max], sonst fallback. */
function sanitizeIntInRange(src, key, min, max, fallback) {
  const n = Math.trunc(Number(src[key]));
  if (!Number.isFinite(n)) return fallback;
  return Math.min(max, Math.max(min, n));
}

function sanitizeEnum(src, key, allowed, fallback) {
  return allowed.includes(src[key]) ? src[key] : fallback;
}

/** Validiert ein gemerktes Gerät ({name, ip, port}) oder gibt null zurück. */
function sanitizeLastDevice(value) {
  if (!value || typeof value !== 'object') return null;
  const ip = typeof value.ip === 'string' ? value.ip.trim() : '';
  if (!isValidHost(ip)) return null;
  const port = isValidPort(value.port) ? Number(value.port) : 7000;
  const name = typeof value.name === 'string' ? value.name.trim().slice(0, 128) : ip;
  return { name: name || ip, ip, port };
}

function sanitizeSettings(input) {
  const src = input && typeof input === 'object' ? input : {};

  const fps = [24, 30, 45, 60].includes(Number(src.fps)) ? Number(src.fps) : DEFAULT_SETTINGS.fps;

  const allowedHeights = [0, 720, 1080, 1440];
  const maxHeight = allowedHeights.includes(Number(src.maxHeight))
    ? Number(src.maxHeight)
    : DEFAULT_SETTINGS.maxHeight;

  let outputIndex = Number.isInteger(Number(src.outputIndex)) ? Number(src.outputIndex) : 0;
  if (outputIndex < 0) outputIndex = 0;

  let bitrate = Number.isFinite(Number(src.bitrate)) ? Math.trunc(Number(src.bitrate)) : 0;
  if (bitrate < 0) bitrate = 0;
  if (bitrate > 100000) bitrate = 100000; // Plausibilitätsdeckel (kbps)

  const audioEnabled = src.audioEnabled === true;

  const preset = sanitizeEnum(src, 'preset', ['lowest_latency', 'balanced', 'best_quality', 'custom'], DEFAULT_SETTINGS.preset);
  const expertMode = src.expertMode === true;

  const targetLatencyMs = sanitizeIntInRange(src, 'targetLatencyMs', 5, 2000, DEFAULT_SETTINGS.targetLatencyMs);
  const gopSeconds = sanitizeIntInRange(src, 'gopSeconds', 1, 10, DEFAULT_SETTINGS.gopSeconds);
  const rateControl = sanitizeEnum(src, 'rateControl', ['display_remoting', 'cbr_live'], DEFAULT_SETTINGS.rateControl);
  const hwaccel = sanitizeEnum(src, 'hwaccel', ['auto', 'none'], DEFAULT_SETTINGS.hwaccel);
  const audioBufferMs = sanitizeIntInRange(src, 'audioBufferMs', 40, 500, DEFAULT_SETTINGS.audioBufferMs);

  const showCursor = src.showCursor !== false; // Default true

  const rememberLastDevice = src.rememberLastDevice !== false; // Default true
  const autoConnect = src.autoConnect === true;
  const startMinimized = src.startMinimized === true;
  const trayIcon = src.trayIcon === true;
  const debugLogging = src.debugLogging === true;

  const activeTab = sanitizeEnum(src, 'activeTab', ['connect', 'stats', 'settings'], DEFAULT_SETTINGS.activeTab);
  const lastDevice = rememberLastDevice ? sanitizeLastDevice(src.lastDevice) : null;

  return {
    fps,
    maxHeight,
    outputIndex,
    bitrate,
    audioEnabled,
    preset,
    expertMode,
    targetLatencyMs,
    gopSeconds,
    rateControl,
    hwaccel,
    audioBufferMs,
    showCursor,
    rememberLastDevice,
    autoConnect,
    startMinimized,
    trayIcon,
    debugLogging,
    activeTab,
    lastDevice,
  };
}

/**
 * Vergleicht nur die Felder, die tatsächlich in buildSidecarArgs landen und
 * damit einen Sidecar-Neustart erzwingen. Reine UI-Einstellungen (Preset-
 * Auswahl, Expertenmodus, gemerktes Gerät, aktiver Tab, ...) lösen bewusst
 * KEINEN Neustart aus.
 */
function daemonFlagsChanged(a, b) {
  return (
    a.fps !== b.fps ||
    a.maxHeight !== b.maxHeight ||
    a.outputIndex !== b.outputIndex ||
    a.bitrate !== b.bitrate ||
    a.audioEnabled !== b.audioEnabled ||
    a.targetLatencyMs !== b.targetLatencyMs ||
    a.gopSeconds !== b.gopSeconds ||
    a.rateControl !== b.rateControl ||
    a.hwaccel !== b.hwaccel ||
    a.audioBufferMs !== b.audioBufferMs ||
    a.showCursor !== b.showCursor ||
    a.debugLogging !== b.debugLogging
  );
}

// ---------------------------------------------------------------------------
// Validierung von Renderer-Eingaben
// ---------------------------------------------------------------------------

const IPV4_RE = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;
const HOSTNAME_RE = /^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$/;

function isValidHost(value) {
  if (typeof value !== 'string') return false;
  const s = value.trim();
  if (!s || s.length > 253) return false;
  return IPV4_RE.test(s) || HOSTNAME_RE.test(s);
}

function isValidPort(value) {
  const n = Number(value);
  return Number.isInteger(n) && n > 0 && n < 65536;
}

/** Gibt einen gesäuberten PIN-String zurück, oder null bei ungültiger Eingabe. */
function sanitizePin(value) {
  if (value === undefined || value === null || value === '') return '';
  const s = String(value).trim();
  if (!/^\d{1,8}$/.test(s)) return null;
  return s;
}

function validateMirrorTarget(payload) {
  const p = payload && typeof payload === 'object' ? payload : {};
  const target = typeof p.target === 'string' ? p.target.trim() : '';

  if (!isValidHost(target)) {
    return { error: 'Ungültige IP-Adresse oder Hostname.' };
  }

  let port = p.port === undefined || p.port === null || p.port === '' ? 7000 : p.port;
  if (!isValidPort(port)) {
    return { error: 'Ungültiger Port (1–65535 erwartet).' };
  }
  port = Number(port);

  const pin = sanitizePin(p.pin);
  if (pin === null) {
    return { error: 'Ungültige PIN (nur Ziffern, max. 8 Stellen).' };
  }

  return { target, port, pin };
}

// ---------------------------------------------------------------------------
// Control-Channel-Client: eine TCP-Verbindung pro Befehl
// ---------------------------------------------------------------------------

function sendControlCommand(cmdObj, timeoutMs = DEFAULT_TIMEOUT_MS) {
  return new Promise((resolve, reject) => {
    const socket = new net.Socket();
    let settled = false;
    let buffer = '';

    const finish = (err, result) => {
      if (settled) return;
      settled = true;
      socket.removeAllListeners();
      socket.destroy();
      if (err) reject(err);
      else resolve(result);
    };

    socket.setTimeout(timeoutMs);

    socket.once('timeout', () => {
      finish(new Error('Zeitüberschreitung beim Warten auf den Sidecar (Steuerkanal).'));
    });

    socket.once('error', (err) => {
      if (err && err.code === 'ECONNREFUSED') {
        finish(new Error('Der Sidecar läuft nicht oder nimmt auf Port 7654 keine Verbindungen an.'));
      } else {
        finish(new Error(`Steuerkanal-Fehler: ${err.message}`));
      }
    });

    socket.connect(CONTROL_PORT, CONTROL_HOST, () => {
      socket.write(JSON.stringify(cmdObj) + '\n');
    });

    socket.on('data', (chunk) => {
      buffer += chunk.toString('utf-8');
      const nl = buffer.indexOf('\n');
      if (nl === -1) return;
      const line = buffer.slice(0, nl);
      try {
        const parsed = JSON.parse(line);
        finish(null, parsed);
      } catch (err) {
        finish(new Error('Antwort des Sidecars konnte nicht gelesen werden (kein gültiges JSON).'));
      }
    });

    socket.once('close', () => {
      // Falls die Gegenseite ohne Zeilenumbruch schließt, versuchen wir es
      // trotzdem noch mit dem bisher gepufferten Inhalt zu parsen.
      if (!settled && buffer.trim()) {
        try {
          const parsed = JSON.parse(buffer.trim());
          finish(null, parsed);
          return;
        } catch (err) {
          // fällt durch zu unten
        }
      }
      finish(new Error('Verbindung zum Sidecar wurde unerwartet geschlossen.'));
    });
  });
}

// ---------------------------------------------------------------------------
// Audio-Bridge: TCP-Client zum PCM-Port des Sidecars (127.0.0.1:7655)
// ---------------------------------------------------------------------------

function setAudioState(state, detail) {
  if (audioBridge.state === state) return;
  audioBridge.state = state;
  notifyRenderer('audio:status', { state, detail: detail || null });
}

function scheduleAudioReconnect() {
  if (!audioBridge.wantConnected) return;
  if (audioBridge.reconnectTimer) return;
  audioBridge.reconnectTimer = setTimeout(() => {
    audioBridge.reconnectTimer = null;
    connectAudioBridge();
  }, audioBridge.reconnectDelay);
  audioBridge.reconnectDelay = Math.min(audioBridge.reconnectDelay * 2, AUDIO_RECONNECT_MAX_MS);
}

function connectAudioBridge() {
  if (!audioBridge.wantConnected || audioBridge.socket) return;

  setAudioState('connecting');
  const socket = new net.Socket();
  audioBridge.socket = socket;
  audioBridge.writable = true;

  socket.once('connect', () => {
    audioBridge.reconnectDelay = AUDIO_RECONNECT_MIN_MS;
    setAudioState('connected');
  });

  // Backpressure: sobald write() Rückstau meldet, wird bis zum drain-
  // Event nicht weitergeschrieben (writeAudioChunk verwirft in der
  // Zwischenzeit neue Blöcke, statt sie zu puffern).
  socket.on('drain', () => {
    audioBridge.writable = true;
  });

  socket.once('error', () => {
    // Aufräumen übernimmt das nachfolgende 'close'-Event.
  });

  socket.once('close', () => {
    if (audioBridge.socket === socket) audioBridge.socket = null;
    audioBridge.writable = false;
    if (audioBridge.wantConnected) {
      setAudioState('error', 'Verbindung zum Audio-Port (7655) getrennt, versuche erneut…');
      scheduleAudioReconnect();
    }
  });

  socket.connect(AUDIO_PORT, AUDIO_HOST);
}

function startAudioBridge() {
  if (audioBridge.wantConnected) return;
  audioBridge.wantConnected = true;
  audioBridge.reconnectDelay = AUDIO_RECONNECT_MIN_MS;
  connectAudioBridge();
}

function stopAudioBridge() {
  audioBridge.wantConnected = false;
  if (audioBridge.reconnectTimer) {
    clearTimeout(audioBridge.reconnectTimer);
    audioBridge.reconnectTimer = null;
  }
  audioBridge.reconnectDelay = AUDIO_RECONNECT_MIN_MS;
  if (audioBridge.socket) {
    const s = audioBridge.socket;
    audioBridge.socket = null;
    s.removeAllListeners();
    s.destroy();
  }
  audioBridge.writable = false;
  setAudioState('disconnected');
}

/** Stellt den gewünschten Zustand der Audio-Bridge her (an, wenn Sidecar läuft/adoptiert ist UND audioEnabled). */
function syncAudioBridgeWanted() {
  const shouldRun = (sidecar.child !== null || sidecar.adopted) && settings.audioEnabled === true;
  if (shouldRun) startAudioBridge();
  else stopAudioBridge();
}

/**
 * Schreibt einen PCM-Block auf den Audio-Socket. Ohne Verbindung wird
 * still verworfen. Bei Rückstau (letztes write() lieferte false) wird
 * dieser Block ebenfalls verworfen statt gepuffert - ein Ton, der
 * Sekunden hinterherhinkt, ist wertloser als eine kurze Lücke.
 */
function writeAudioChunk(buffer) {
  const socket = audioBridge.socket;
  if (!socket || socket.destroyed || !audioBridge.writable) {
    audioBridge.droppedChunks += 1;
    return;
  }
  const ok = socket.write(buffer);
  if (!ok) audioBridge.writable = false;
}

// ---------------------------------------------------------------------------
// Sidecar-Prozessverwaltung
// ---------------------------------------------------------------------------

function resolveSidecarPath() {
  // Der Installer legt die Binary als luftspiegel.exe ab, das Makefile des
  // Upstream-Projekts baut sie dagegen als doubletake.exe. Beide Namen werden
  // akzeptiert, damit ein frischer Klon nach einem simplen `make` sofort
  // funktioniert und nicht an einer Umbenennung scheitert.
  const exeNames = ['luftspiegel.exe', 'doubletake.exe'];
  const dir = app.isPackaged
    ? process.resourcesPath
    : path.join(__dirname, '..', 'bin');

  const candidates = exeNames.map((n) => path.join(dir, n));
  const found = candidates.find((c) => fs.existsSync(c));

  if (!found) {
    throw new Error(
      `Sidecar-Programm nicht gefunden. Gesucht in:\n${candidates.join('\n')}\n` +
        'Mit `go build -o bin/luftspiegel.exe ./cmd/doubletake` bauen (oder `make`).'
    );
  }
  return found;
}

function buildSidecarArgs(s) {
  const args = ['-daemonize'];
  if (s.audioEnabled) {
    args.push('-audio-tcp', String(AUDIO_PORT));
  } else {
    args.push('-no-audio');
  }
  args.push('-fps', String(s.fps), '-output-index', String(s.outputIndex));
  args.push('-max-height', String(s.maxHeight));
  if (s.bitrate && s.bitrate > 0) {
    args.push('-bitrate', String(s.bitrate));
  }
  args.push('-target-latency-ms', String(s.targetLatencyMs));
  args.push('-gop-seconds', String(s.gopSeconds));
  args.push('-rate-control', String(s.rateControl));
  args.push('-hwaccel', String(s.hwaccel));
  args.push('-audio-buffer-ms', String(s.audioBufferMs));
  if (s.showCursor === false) {
    // Invertierte Logik: das Flag heißt "-no-cursor" und hat keinen Wert.
    args.push('-no-cursor');
  }
  if (s.debugLogging) {
    args.push('-debug');
  }
  return args;
}

function waitForSidecarReady(timeoutMs) {
  return new Promise((resolve, reject) => {
    const startedAt = Date.now();

    const poll = async () => {
      if (sidecar.child === null) {
        reject(new Error('Sidecar-Prozess wurde beendet, bevor er bereit war.'));
        return;
      }
      try {
        await sendControlCommand({ cmd: 'status' }, 800);
        resolve();
        return;
      } catch (err) {
        if (Date.now() - startedAt >= timeoutMs) {
          reject(
            new Error(
              'Der Sidecar antwortet nicht auf Port 7654 (Zeitüberschreitung). ' +
                'Möglicherweise ist der Port bereits durch einen anderen Prozess belegt, ' +
                'oder eine weitere Instanz von Luftspiegel läuft bereits.'
            )
          );
          return;
        }
        setTimeout(poll, SIDECAR_READY_POLL_MS);
      }
    };

    poll();
  });
}

/** Startet den Sidecar-Prozess mit den übergebenen Einstellungen und wartet, bis er auf dem Steuerkanal antwortet. */
async function spawnSidecarProcess(currentSettings) {
  if (sidecar.child) {
    throw new Error('Sidecar läuft bereits.');
  }

  const exePath = resolveSidecarPath();
  sidecar.path = exePath;
  const args = buildSidecarArgs(currentSettings);

  sidecar.starting = true;
  sidecar.stopping = false;

  const child = spawn(exePath, args, {
    cwd: path.dirname(exePath),
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
  });
  sidecar.child = child;

  child.stdout.on('data', (d) => {
    process.stdout.write(`[luftspiegel] ${d}`);
    if (logStream) logStream.write(d);
  });
  child.stderr.on('data', (d) => {
    process.stderr.write(`[luftspiegel:err] ${d}`);
    if (logStream) logStream.write(d);
  });

  child.once('exit', (code, signal) => {
    const wasStopping = sidecar.stopping;
    sidecar.child = null;
    sidecar.starting = false;
    sidecar.stopping = false;
    if (!wasStopping) {
      console.error(`[luftspiegel] unerwartet beendet (code=${code}, signal=${signal})`);
      reportSidecarError(`Der Sidecar-Prozess wurde unerwartet beendet (Code ${code}).`);
    }
  });

  child.once('error', (err) => {
    sidecar.child = null;
    sidecar.starting = false;
    reportSidecarError(`Sidecar konnte nicht gestartet werden: ${err.message}`);
  });

  try {
    await waitForSidecarReady(SIDECAR_READY_TIMEOUT_MS);
  } finally {
    sidecar.starting = false;
  }
}

/**
 * Prüft zuerst per kurzer status-Anfrage, ob auf 127.0.0.1:7654 bereits ein
 * Daemon antwortet (z.B. manuell/extern gestarteter luftspiegel.exe-Prozess
 * oder ein Test-Daemon wie tools/fake-daemon.js). Falls ja, wird dieser
 * "adoptiert" statt einen zweiten Prozess zu spawnen - so vermeiden wir
 * Portkonflikte und bleiben gegen beliebige kompatible Control-Channel-
 * Implementierungen testbar. Nur wenn niemand antwortet, startet die GUI
 * ihren eigenen Sidecar-Prozess.
 */
async function startOrAdoptSidecar(currentSettings) {
  if (sidecar.child || sidecar.adopted) return;

  try {
    await sendControlCommand({ cmd: 'status' }, 800);
    sidecar.adopted = true;
    console.log('[sidecar] Bestehender Daemon auf Port 7654 gefunden, wird verwendet (nicht von der GUI gestartet).');
    syncAudioBridgeWanted();
    return;
  } catch (probeErr) {
    // Niemand antwortet - selbst starten.
  }

  await spawnSidecarProcess(currentSettings);
  syncAudioBridgeWanted();
}

/**
 * Sauberes Stoppen: zuerst über den Steuerkanal disconnect (TEARDOWN),
 * erst danach darf der Prozess beendet werden. Wird auch vor einem
 * Neustart wegen geänderter Einstellungen verwendet.
 */
async function stopSidecarClean() {
  // Audio-Bridge unabhängig vom Prozess-Ownership-Status immer mit
  // abbauen - sie ist eine separate, von der GUI selbst gehaltene
  // TCP-Verbindung und darf den Sidecar nicht überleben.
  stopAudioBridge();

  if (!sidecar.child && !sidecar.adopted) return;

  try {
    await sendControlCommand({ cmd: 'disconnect' }, DISCONNECT_TIMEOUT_MS);
  } catch (err) {
    // Auch wenn disconnect fehlschlägt (z.B. keine aktive Sitzung),
    // fahren wir mit dem Herunterfahren fort - wir haben unser Bestes
    // versucht, eine saubere TEARDOWN zu senden.
    console.warn('[sidecar] disconnect vor dem Stoppen meldete einen Fehler:', err.message);
  }

  if (sidecar.adopted) {
    // Fremder Prozess - wir haben ihn nicht gestartet, also beenden wir
    // ihn auch nicht. Nur die Zuordnung aufheben.
    sidecar.adopted = false;
    return;
  }

  const child = sidecar.child;
  if (!child) return;

  sidecar.stopping = true;

  await new Promise((resolve) => {
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      resolve();
    };
    child.once('exit', finish);
    child.kill();
    setTimeout(() => {
      if (!done && sidecar.child === child) {
        try {
          child.kill('SIGKILL');
        } catch (err) {
          /* Prozess evtl. schon weg */
        }
      }
      finish();
    }, 3000);
  });

  sidecar.child = null;
  sidecar.stopping = false;
}

/** Startet den Sidecar mit neuen Einstellungen neu, sofern die relevanten Flags sich geändert haben. */
async function applySettingsRestartIfNeeded(oldSettings, newSettings) {
  if (!daemonFlagsChanged(oldSettings, newSettings)) {
    return { restarted: false };
  }

  if (sidecar.adopted) {
    // Ein extern gestarteter Daemon wird von der GUI nicht verwaltet und
    // kann daher nicht automatisch neu gestartet werden.
    return {
      restarted: false,
      note:
        'Der Sidecar läuft außerhalb der GUI (nicht selbst gestartet). ' +
        'Die neuen Einstellungen wirken erst nach einem manuellen Neustart des Sidecars.',
    };
  }

  if (!sidecar.child) {
    // Kein laufender Sidecar - er wird beim nächsten Bedarf mit den neuen
    // Einstellungen gestartet.
    return { restarted: false };
  }

  await stopSidecarClean();
  await spawnSidecarProcess(newSettings);
  return { restarted: true };
}

/** Stellt sicher, dass der Sidecar läuft bzw. erreichbar ist (startet ihn ggf. selbst). */
async function ensureSidecarRunning() {
  if (sidecar.child || sidecar.adopted) return;
  if (sidecar.starting) {
    // Auf laufenden Start warten
    await waitForSidecarReady(SIDECAR_READY_TIMEOUT_MS);
    return;
  }
  await startOrAdoptSidecar(settings);
}

// ---------------------------------------------------------------------------
// Loopback-Audioerfassung (System-Audio, Windows-only)
// ---------------------------------------------------------------------------

/**
 * Registriert den Handler für navigator.mediaDevices.getDisplayMedia() im
 * Renderer. audio: 'loopback' ist laut Electron-Dokumentation der einzige
 * offiziell unterstützte Weg an System-Audio zu kommen und nur unter
 * Windows verfügbar - passt hier. bewusst NICHT verwendet wird
 * getUserMedia({ chromeMediaSource: 'desktop' }) für Audio: das killt den
 * Renderer mit "Terminating renderer for bad IPC message, reason 263"
 * (electron#42765, als "not planned" geschlossen).
 *
 * Der Video-Track wird nur benötigt, um den Audio-Track zu bekommen (das
 * eigentliche Bild liefert der Go-Sidecar per ffmpeg) - er wird im
 * Renderer direkt nach getDisplayMedia() wieder gestoppt.
 */
function registerDisplayMediaHandler() {
  session.defaultSession.setDisplayMediaRequestHandler((request, callback) => {
    desktopCapturer
      .getSources({ types: ['screen'] })
      .then((sources) => {
        if (!sources || sources.length === 0) {
          // Kein Bildschirm gefunden - Anfrage sauber ablehnen statt in
          // einen TypeError beim Zugriff auf sources[0] zu laufen.
          console.error('[audio] desktopCapturer.getSources() lieferte keine Quellen.');
          callback({});
          return;
        }
        callback({ video: sources[0], audio: 'loopback' });
      })
      .catch((err) => {
        console.error('[audio] getSources() für Loopback-Anfrage fehlgeschlagen:', err.message);
        callback({});
      });
  });
}

// ---------------------------------------------------------------------------
// Displays
// ---------------------------------------------------------------------------

function getDisplaysPayload() {
  const displays = screen.getAllDisplays();
  const primaryId = screen.getPrimaryDisplay().id;
  return displays.map((d, index) => ({
    index,
    id: d.id,
    isPrimary: d.id === primaryId,
    width: d.size.width,
    height: d.size.height,
    scaleFactor: d.scaleFactor,
    label: d.label && d.label.trim() ? d.label : `Bildschirm ${index + 1}`,
  }));
}

function broadcastDisplays() {
  notifyRenderer('display:list', getDisplaysPayload());
}

// ---------------------------------------------------------------------------
// Renderer-Kommunikation
// ---------------------------------------------------------------------------

function notifyRenderer(channel, payload) {
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.webContents.send(channel, payload);
  }
}

/**
 * Sidecar-Fehler werden zusätzlich zwischengespeichert (lastSidecarError),
 * weil der Push per webContents.send verloren geht, falls der Renderer
 * seinen Event-Listener noch nicht registriert hat (z.B. Fehler beim
 * automatischen Sidecar-Start unmittelbar nach app.whenReady). Der
 * Renderer holt den letzten Fehler beim Start zusätzlich aktiv über
 * sidecar:lastError ab.
 */
function reportSidecarError(message) {
  lastSidecarError = message;
  console.error('[sidecar]', message);
  notifyRenderer('sidecar:error', { message });
}

/**
 * Fragt "status" per selbst-nachplanendem setTimeout ab (statt setInterval),
 * weil das Intervall dynamisch ist: 1s solange gestreamt wird (der
 * Statistik-Tab will frische Werte), sonst 2s. sendControlCommand() öffnet
 * pro Aufruf eine neue TCP-Verbindung - bei 1s Intervall und einem 3s-
 * Timeout (DEFAULT_TIMEOUT_MS) würden sich Ticks sonst stapeln, deshalb der
 * statusPollInFlight-Guard: ein neuer Tick wird übersprungen, solange der
 * vorige noch offen ist, statt eine zweite Verbindung parallel aufzumachen.
 */
function scheduleNextStatusPoll(delayMs) {
  if (!statusPollActive) return;
  statusPollTimer = setTimeout(statusPollTick, delayMs);
}

async function statusPollTick() {
  statusPollTimer = null;
  if (!statusPollActive) return;

  if (!sidecar.child && !sidecar.adopted) {
    scheduleNextStatusPoll(STATUS_POLL_INTERVAL_IDLE_MS);
    return; // kein Sidecar erreichbar -> nichts zu pollen
  }
  if (statusPollInFlight) {
    scheduleNextStatusPoll(STATUS_POLL_INTERVAL_IDLE_MS);
    return;
  }

  statusPollInFlight = true;
  let nextDelay = STATUS_POLL_INTERVAL_IDLE_MS;
  try {
    const result = await sendControlCommand({ cmd: 'status' }, DEFAULT_TIMEOUT_MS);
    // Zähler für verworfene Audio-Chunks huckepack auf den ohnehin
    // laufenden status-Push legen (kein eigener Kanal nötig) - so kommt er
    // im selben Rhythmus wie alle anderen Live-Werte im Statistik-Tab an.
    if (result && typeof result === 'object') {
      result.guiAudioDropped = audioBridge.droppedChunks;
    }
    notifyRenderer('mirror:status', result);
    // "stats" ist laut Daemon-Vertrag nur gesetzt, während gestreamt wird -
    // daran (statt am Text von "state") hängt das schnellere Poll-Intervall.
    if (result && result.stats) {
      nextDelay = STATUS_POLL_INTERVAL_STREAMING_MS;
    }
  } catch (err) {
    notifyRenderer('mirror:status', { ok: false, error: err.message, guiAudioDropped: audioBridge.droppedChunks });
  } finally {
    statusPollInFlight = false;
  }
  scheduleNextStatusPoll(nextDelay);
}

/**
 * Aktualisiert die Geräteliste aus dem Cache (billiger "devices"-Aufruf),
 * damit später gefundene Geräte im Hintergrund nachgetragen werden - OHNE
 * erneut einen "discover"-Scan auszulösen. Bewusst als eigener setInterval
 * mit festen 2s entkoppelt vom (dynamischen) status-Poll.
 */
function startDevicesPolling() {
  stopDevicesPolling();
  devicesPollTimer = setInterval(async () => {
    if (!sidecar.child && !sidecar.adopted) return;
    try {
      const devicesResult = await sendControlCommand({ cmd: 'devices' }, DEFAULT_TIMEOUT_MS);
      notifyRenderer('device:updated', devicesResult);
    } catch (err) {
      // Stiller Fehlschlag - ein Hintergrund-Refresh der Geräteliste soll
      // keine Fehlermeldung auslösen, die Liste bleibt einfach wie sie ist.
    }
  }, DEVICES_POLL_INTERVAL_MS);
}

function stopDevicesPolling() {
  if (devicesPollTimer) {
    clearInterval(devicesPollTimer);
    devicesPollTimer = null;
  }
}

function startStatusPolling() {
  stopStatusPolling();
  statusPollActive = true;
  scheduleNextStatusPoll(0);
  startDevicesPolling();
}

function stopStatusPolling() {
  statusPollActive = false;
  if (statusPollTimer) {
    clearTimeout(statusPollTimer);
    statusPollTimer = null;
  }
  statusPollInFlight = false;
  stopDevicesPolling();
}

// ---------------------------------------------------------------------------
// Tray-Icon
// ---------------------------------------------------------------------------

/**
 * Baut ein winziges 16x16-Tray-Icon (gefüllter Kreis in Akzentfarbe) direkt
 * als rohes BGRA-Bitmap, statt eine Icon-Datei ins Repo zu legen - das
 * Projekt hat bislang keinerlei Bildassets und die CSP betrifft ohnehin nur
 * den Renderer-Prozess, hier ist das kein Sicherheitsthema.
 */
function buildTrayIcon() {
  const size = 16;
  const buf = Buffer.alloc(size * size * 4);
  const cx = (size - 1) / 2;
  const cy = (size - 1) / 2;
  const r = size / 2 - 1;
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      const i = (y * size + x) * 4;
      const dx = x - cx;
      const dy = y - cy;
      if (dx * dx + dy * dy <= r * r) {
        // Bitmap-Reihenfolge ist BGRA - Akzentfarbe #2fd3c4 (R=0x2f G=0xd3 B=0xc4).
        buf[i] = 0xc4;
        buf[i + 1] = 0xd3;
        buf[i + 2] = 0x2f;
        buf[i + 3] = 0xff;
      }
    }
  }
  return nativeImage.createFromBuffer(buf, { width: size, height: size });
}

function createTray() {
  if (tray) return;
  tray = new Tray(buildTrayIcon());
  tray.setToolTip('Luftspiegel');
  tray.setContextMenu(
    Menu.buildFromTemplate([
      {
        label: 'Öffnen',
        click: () => {
          if (mainWindow) {
            mainWindow.show();
            mainWindow.focus();
          }
        },
      },
      { type: 'separator' },
      {
        // app.quit() (NICHT app.exit()/mainWindow.destroy()) - das ist der
        // einzige Weg, der zuverlässig durch den before-quit-Handler läuft
        // und damit vor dem Beenden ein sauberes RTSP-TEARDOWN auslöst.
        label: 'Beenden',
        click: () => app.quit(),
      },
    ])
  );
  tray.on('click', () => {
    if (!mainWindow) return;
    if (mainWindow.isVisible()) mainWindow.hide();
    else {
      mainWindow.show();
      mainWindow.focus();
    }
  });
}

function destroyTray() {
  if (tray) {
    tray.destroy();
    tray = null;
  }
}

/** Erzeugt/entfernt das Tray-Icon passend zu settings.trayIcon. */
function syncTray() {
  if (settings.trayIcon) createTray();
  else destroyTray();
}

// ---------------------------------------------------------------------------
// Fenster
// ---------------------------------------------------------------------------

function createWindow() {
  mainWindow = new BrowserWindow({
    width: 1080,
    height: 720,
    minWidth: 860,
    minHeight: 640,
    backgroundColor: BACKGROUND_COLOR,
    autoHideMenuBar: true,
    show: false,
    webPreferences: {
      preload: path.join(__dirname, 'preload.js'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  });

  mainWindow.loadFile(path.join(__dirname, 'renderer', 'index.html'));

  if (!app.isPackaged) {
    // Spiegelt die Renderer-Konsole ins Terminal - erleichtert das Prüfen
    // auf Fehler während der Entwicklung.
    mainWindow.webContents.on('console-message', (_event, level, message, line, sourceId) => {
      console.log(`[renderer] ${message} (${sourceId}:${line})`);
    });
  }

  mainWindow.once('ready-to-show', () => {
    if (settings.startMinimized) {
      // Bei aktivem Tray-Icon direkt versteckt starten (das Fenster ist ja
      // jederzeit übers Tray erreichbar); ohne Tray gäbe es sonst keinen Weg
      // zurück, daher dort nur minimieren statt verstecken.
      if (settings.trayIcon) mainWindow.hide();
      else mainWindow.minimize();
    } else {
      mainWindow.show();
    }
  });

  // Mit aktivem Tray-Icon wird "Fenster schließen" zu "verstecken" statt zu
  // "beenden" - das eigentliche Beenden läuft ausschließlich über
  // app.quit() (Tray-Menü "Beenden" oder z.B. Task-Leiste), das den
  // before-quit-Handler unten (mit dem sauberen TEARDOWN) durchläuft. Ohne
  // Tray bleibt das bisherige Verhalten (Schließen = Beenden) unverändert.
  mainWindow.on('close', (event) => {
    if (settings.trayIcon && !quitting) {
      event.preventDefault();
      mainWindow.hide();
    }
  });

  mainWindow.on('closed', () => {
    mainWindow = null;
  });

  startStatusPolling();
}

// ---------------------------------------------------------------------------
// IPC-Handler
// ---------------------------------------------------------------------------

function registerIpcHandlers() {
  ipcMain.handle('device:discover', async () => {
    try {
      await ensureSidecarRunning();
    } catch (err) {
      return { ok: false, error: err.message, devices: [] };
    }
    try {
      // Der Daemon soll (perspektivisch) synchron warten, bis der Scan
      // (mDNS + Fallback, bei aktivem VPN bis zu ~5s) abgeschlossen ist,
      // und die Geräte direkt in dieser Antwort liefern. Falls die
      // discover-Antwort bereits ein devices-Array enthält, wird das
      // direkt verwendet; ältere/asynchrone Daemon-Varianten, die discover
      // ohne devices-Feld sofort beantworten, werden über einen
      // zusätzlichen (billigen, cache-basierten) devices-Aufruf abgefangen.
      const discoverResp = await sendControlCommand({ cmd: 'discover' }, DISCOVER_TIMEOUT_MS);
      if (discoverResp && discoverResp.ok !== false && Array.isArray(discoverResp.devices)) {
        return discoverResp;
      }
      return await sendControlCommand({ cmd: 'devices' }, DEFAULT_TIMEOUT_MS);
    } catch (err) {
      return { ok: false, error: err.message, devices: [] };
    }
  });

  ipcMain.handle('device:list', async () => {
    try {
      await ensureSidecarRunning();
    } catch (err) {
      return { ok: false, error: err.message, devices: [] };
    }
    try {
      return await sendControlCommand({ cmd: 'devices' }, DEFAULT_TIMEOUT_MS);
    } catch (err) {
      return { ok: false, error: err.message, devices: [] };
    }
  });

  ipcMain.handle('mirror:start', async (_event, payload) => {
    const validated = validateMirrorTarget(payload);
    if (validated.error) {
      return { ok: false, error: validated.error };
    }

    try {
      await ensureSidecarRunning();
    } catch (err) {
      return { ok: false, error: err.message };
    }

    try {
      return await sendControlCommand(
        { cmd: 'connect', target: validated.target, port: validated.port, pin: validated.pin },
        CONNECT_TIMEOUT_MS
      );
    } catch (err) {
      return { ok: false, error: err.message };
    }
  });

  ipcMain.handle('mirror:stop', async (_event, payload) => {
    const p = payload && typeof payload === 'object' ? payload : {};
    const cmd = { cmd: 'disconnect' };
    if (typeof p.target === 'string' && p.target.trim()) {
      if (!isValidHost(p.target.trim())) {
        return { ok: false, error: 'Ungültige IP-Adresse oder Hostname.' };
      }
      cmd.target = p.target.trim();
    }
    try {
      return await sendControlCommand(cmd, DISCONNECT_TIMEOUT_MS);
    } catch (err) {
      return { ok: false, error: err.message };
    }
  });

  ipcMain.handle('mirror:status', async () => {
    if (!sidecar.child && !sidecar.adopted) {
      return { ok: true, state: 'getrennt', guiAudioDropped: audioBridge.droppedChunks };
    }
    try {
      const result = await sendControlCommand({ cmd: 'status' }, DEFAULT_TIMEOUT_MS);
      if (result && typeof result === 'object') result.guiAudioDropped = audioBridge.droppedChunks;
      return result;
    } catch (err) {
      return { ok: false, error: err.message, guiAudioDropped: audioBridge.droppedChunks };
    }
  });

  ipcMain.handle('display:list', async () => {
    return getDisplaysPayload();
  });

  ipcMain.handle('sidecar:lastError', async () => {
    const err = lastSidecarError;
    lastSidecarError = null; // einmalig abholen, danach nur noch per Push
    return err;
  });

  /**
   * Pull-Fallback für den aktuellen Audio-Bridge-Zustand: analog zum
   * lastSidecarError-Problem kann der 'audio:status'-Push verloren gehen,
   * wenn der Renderer seinen Event-Listener noch nicht registriert hat
   * (z.B. weil startOrAdoptSidecar() gleich nach app.whenReady() schon
   * die erste Verbindung zu 7655 aufbaut, bevor die Seite fertig geladen
   * ist). Der Renderer fragt diesen Zustand daher beim Start zusätzlich
   * aktiv ab, statt sich allein auf den Push zu verlassen.
   */
  ipcMain.handle('audio:statusGet', async () => ({ state: audioBridge.state, droppedChunks: audioBridge.droppedChunks }));

  ipcMain.handle('settings:get', async () => {
    return settings;
  });

  ipcMain.handle('settings:set', async (_event, payload) => {
    const oldSettings = settings;
    const newSettings = sanitizeSettings(payload);
    settings = newSettings;
    persistSettings();
    syncTray(); // trayIcon ist eine reine UI-Einstellung, aber muss sofort wirken

    try {
      const { restarted, note } = await applySettingsRestartIfNeeded(oldSettings, newSettings);
      // Deckt sowohl den Fall ab, dass audioEnabled sich geändert hat (dann
      // hat applySettingsRestartIfNeeded gerade neu gestartet) als auch den
      // Fall eines adoptierten Fremd-Daemons (der nicht neu startet, dessen
      // Audio-Port aber trotzdem verbunden/getrennt werden soll).
      syncAudioBridgeWanted();
      return { ok: true, settings, restarted, note };
    } catch (err) {
      return { ok: false, settings, error: `Neustart des Sidecars fehlgeschlagen: ${err.message}` };
    }
  });

  // Fire-and-forget: der Renderer schickt hier alle ~10-20ms einen PCM-
  // Block (ArrayBuffer, s16le/44100Hz/stereo, aus dem AudioWorklet). Ein
  // invoke()-Roundtrip wäre für diese Frequenz unnötig teuer.
  ipcMain.on('audio:chunk', (_event, payload) => {
    if (!(payload instanceof ArrayBuffer)) return;
    // Plausibilitätsdeckel: bei 20ms Batches sind das ~3528 Byte; alles
    // jenseits von 64 KiB ist mit Sicherheit keine gültige Nachricht - wird
    // (wie jeder andere verworfene Block auch) mitgezählt.
    if (payload.byteLength === 0 || payload.byteLength > 65536) {
      audioBridge.droppedChunks += 1;
      return;
    }
    writeAudioChunk(Buffer.from(payload));
  });

  // Öffnet das Verzeichnis, in das Sidecar-stdout/stderr gespiegelt werden
  // (siehe openLogStream/spawnSidecarProcess), im System-Dateimanager.
  ipcMain.handle('logs:open', async () => {
    try {
      fs.mkdirSync(logDirPath(), { recursive: true });
      const err = await shell.openPath(logDirPath());
      if (err) return { ok: false, error: err };
      return { ok: true };
    } catch (err) {
      return { ok: false, error: err.message };
    }
  });
}

// ---------------------------------------------------------------------------
// App-Lebenszyklus
// ---------------------------------------------------------------------------

let quitting = false;

async function cleanShutdown() {
  stopStatusPolling();
  destroyTray();
  try {
    await stopSidecarClean();
  } catch (err) {
    console.error('[shutdown] Fehler beim sauberen Beenden des Sidecars:', err);
  }
}

const gotLock = app.requestSingleInstanceLock();
if (!gotLock) {
  app.quit();
} else {
  app.on('second-instance', () => {
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore();
      if (!mainWindow.isVisible()) mainWindow.show();
      mainWindow.focus();
    }
  });

  app.whenReady().then(async () => {
    loadSettings();
    openLogStream();
    registerIpcHandlers();
    registerDisplayMediaHandler();
    syncTray();
    createWindow();

    screen.on('display-added', broadcastDisplays);
    screen.on('display-removed', broadcastDisplays);
    screen.on('display-metrics-changed', broadcastDisplays);

    try {
      await startOrAdoptSidecar(settings);
    } catch (err) {
      reportSidecarError(`Start beim Programmstart fehlgeschlagen: ${err.message}`);
    }
  });

  app.on('window-all-closed', () => {
    // Mit aktivem Tray-Icon läuft die App im Hintergrund weiter (das
    // Fenster wurde nur versteckt, siehe mainWindow.on('close', ...) in
    // createWindow) - "window-all-closed" darf sie dann NICHT beenden,
    // sonst würde eine laufende Sitzung ohne TEARDOWN gekillt.
    if (settings.trayIcon) return;
    if (process.platform !== 'darwin') {
      app.quit();
    }
  });

  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) {
      createWindow();
    } else if (mainWindow) {
      mainWindow.show();
    }
  });

  // Zentraler Ausstiegspunkt für JEDEN Beendigungsweg (Fenster-X ohne Tray,
  // Tray-Menü "Beenden", Task-Leiste, Alt+F4, ...): app.quit() (oder das
  // native Beenden) löst 'before-quit' aus, bevor der Prozess wirklich
  // stirbt. Hier wird das Beenden verzögert (preventDefault), bis
  // stopSidecarClean() das RTSP-TEARDOWN sauber abgeschickt hat - erst
  // danach beendet app.exit(0) den Prozess wirklich. Es gibt bewusst
  // keinen zweiten Pfad, der den Prozess direkt beendet.
  app.on('before-quit', (event) => {
    if (quitting) return;
    event.preventDefault();
    quitting = true;
    cleanShutdown().finally(() => {
      app.exit(0);
    });
  });
}
