'use strict';

const { app, BrowserWindow, ipcMain, screen } = require('electron');
const path = require('path');
const fs = require('fs');
const net = require('net');
const { spawn } = require('child_process');

// ---------------------------------------------------------------------------
// Konstanten
// ---------------------------------------------------------------------------

const CONTROL_HOST = '127.0.0.1';
const CONTROL_PORT = 7654;

const STATUS_POLL_INTERVAL_MS = 2000;
const SIDECAR_READY_TIMEOUT_MS = 6000;
const SIDECAR_READY_POLL_MS = 350;
const CONNECT_TIMEOUT_MS = 15000;
const DISCONNECT_TIMEOUT_MS = 8000;
const DISCOVER_TIMEOUT_MS = 12000; // Netzwerk-Scan-Fallback (z.B. bei aktivem VPN) kann bis zu ~5s dauern
const DEFAULT_TIMEOUT_MS = 3000;

const SETTINGS_FILE = 'settings.json';

const DEFAULT_SETTINGS = {
  fps: 30,
  maxHeight: 1080, // 0 = native
  outputIndex: 0,
  bitrate: 0, // 0 = automatisch
};

const BACKGROUND_COLOR = '#0f1417';

// ---------------------------------------------------------------------------
// Globaler Zustand
// ---------------------------------------------------------------------------

let mainWindow = null;
let statusPollTimer = null;
let settings = { ...DEFAULT_SETTINGS };
let lastSidecarError = null;

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

function sanitizeSettings(input) {
  const src = input && typeof input === 'object' ? input : {};

  const fps = [30, 60].includes(Number(src.fps)) ? Number(src.fps) : DEFAULT_SETTINGS.fps;

  const allowedHeights = [0, 720, 1080];
  const maxHeight = allowedHeights.includes(Number(src.maxHeight))
    ? Number(src.maxHeight)
    : DEFAULT_SETTINGS.maxHeight;

  let outputIndex = Number.isInteger(Number(src.outputIndex)) ? Number(src.outputIndex) : 0;
  if (outputIndex < 0) outputIndex = 0;

  let bitrate = Number.isFinite(Number(src.bitrate)) ? Math.trunc(Number(src.bitrate)) : 0;
  if (bitrate < 0) bitrate = 0;
  if (bitrate > 100000) bitrate = 100000; // Plausibilitätsdeckel (kbps)

  return { fps, maxHeight, outputIndex, bitrate };
}

/** Vergleicht nur die Felder, die den Sidecar-Neustart erzwingen. */
function daemonFlagsChanged(a, b) {
  return (
    a.fps !== b.fps ||
    a.maxHeight !== b.maxHeight ||
    a.outputIndex !== b.outputIndex ||
    a.bitrate !== b.bitrate
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
// Sidecar-Prozessverwaltung
// ---------------------------------------------------------------------------

function resolveSidecarPath() {
  const exeName = 'luftspiegel.exe';
  const candidate = app.isPackaged
    ? path.join(process.resourcesPath, exeName)
    : path.join(__dirname, '..', 'bin', exeName);

  if (!fs.existsSync(candidate)) {
    throw new Error(
      `Sidecar-Programm nicht gefunden: ${candidate}\n` +
        'Bitte sicherstellen, dass luftspiegel.exe im bin-Ordner liegt.'
    );
  }
  return candidate;
}

function buildSidecarArgs(s) {
  const args = ['-daemonize', '-no-audio', '-fps', String(s.fps), '-output-index', String(s.outputIndex)];
  args.push('-max-height', String(s.maxHeight));
  if (s.bitrate && s.bitrate > 0) {
    args.push('-bitrate', String(s.bitrate));
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

  child.stdout.on('data', (d) => process.stdout.write(`[luftspiegel] ${d}`));
  child.stderr.on('data', (d) => process.stderr.write(`[luftspiegel:err] ${d}`));

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
    return;
  } catch (probeErr) {
    // Niemand antwortet - selbst starten.
  }

  await spawnSidecarProcess(currentSettings);
}

/**
 * Sauberes Stoppen: zuerst über den Steuerkanal disconnect (TEARDOWN),
 * erst danach darf der Prozess beendet werden. Wird auch vor einem
 * Neustart wegen geänderter Einstellungen verwendet.
 */
async function stopSidecarClean() {
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

function startStatusPolling() {
  stopStatusPolling();
  statusPollTimer = setInterval(async () => {
    if (!sidecar.child && !sidecar.adopted) return; // kein Sidecar erreichbar -> nichts zu pollen
    try {
      const result = await sendControlCommand({ cmd: 'status' }, DEFAULT_TIMEOUT_MS);
      notifyRenderer('mirror:status', result);
    } catch (err) {
      notifyRenderer('mirror:status', { ok: false, error: err.message });
    }

    // Zusätzlich die Geräteliste aus dem Cache aktualisieren (billiger
    // "devices"-Aufruf), damit später gefundene Geräte im Hintergrund
    // nachgetragen werden - OHNE erneut einen "discover"-Scan auszulösen.
    try {
      const devicesResult = await sendControlCommand({ cmd: 'devices' }, DEFAULT_TIMEOUT_MS);
      notifyRenderer('device:updated', devicesResult);
    } catch (err) {
      // Stiller Fehlschlag - ein Hintergrund-Refresh der Geräteliste soll
      // keine Fehlermeldung auslösen, die Liste bleibt einfach wie sie ist.
    }
  }, STATUS_POLL_INTERVAL_MS);
}

function stopStatusPolling() {
  if (statusPollTimer) {
    clearInterval(statusPollTimer);
    statusPollTimer = null;
  }
}

// ---------------------------------------------------------------------------
// Fenster
// ---------------------------------------------------------------------------

function createWindow() {
  mainWindow = new BrowserWindow({
    width: 1080,
    height: 720,
    minWidth: 860,
    minHeight: 600,
    backgroundColor: BACKGROUND_COLOR,
    autoHideMenuBar: true,
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
      return { ok: true, state: 'getrennt' };
    }
    try {
      return await sendControlCommand({ cmd: 'status' }, DEFAULT_TIMEOUT_MS);
    } catch (err) {
      return { ok: false, error: err.message };
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

  ipcMain.handle('settings:get', async () => {
    return settings;
  });

  ipcMain.handle('settings:set', async (_event, payload) => {
    const oldSettings = settings;
    const newSettings = sanitizeSettings(payload);
    settings = newSettings;
    persistSettings();

    try {
      const { restarted, note } = await applySettingsRestartIfNeeded(oldSettings, newSettings);
      return { ok: true, settings, restarted, note };
    } catch (err) {
      return { ok: false, settings, error: `Neustart des Sidecars fehlgeschlagen: ${err.message}` };
    }
  });
}

// ---------------------------------------------------------------------------
// App-Lebenszyklus
// ---------------------------------------------------------------------------

let quitting = false;

async function cleanShutdown() {
  stopStatusPolling();
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
      mainWindow.focus();
    }
  });

  app.whenReady().then(async () => {
    loadSettings();
    registerIpcHandlers();
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
    if (process.platform !== 'darwin') {
      app.quit();
    }
  });

  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) {
      createWindow();
    }
  });

  app.on('before-quit', (event) => {
    if (quitting) return;
    event.preventDefault();
    quitting = true;
    cleanShutdown().finally(() => {
      app.exit(0);
    });
  });
}
