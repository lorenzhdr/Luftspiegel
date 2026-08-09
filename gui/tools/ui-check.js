'use strict';

/**
 * Automatisierte Renderer-Prüfung über das Chrome DevTools Protocol (CDP) -
 * OHNE echte Maus-/Tastatur-Events und ohne das Fenster zu fokussieren.
 *
 * Warum CDP statt OS-Eingaben simulieren: auf einer Entwicklungsmaschine
 * können parallel andere Fenster/Sessions offen sein - synthetische
 * Maus-/Tastatur-Events (z.B. über SendInput/SendKeys) landen im
 * *fokussierten* Fenster, das nicht zwangsläufig dieses ist, und können
 * fremde Arbeit stören. CDP spricht dagegen direkt mit dem konkreten
 * Renderer-Prozess über dessen Debug-WebSocket - unabhängig davon, welches
 * Fenster gerade den Eingabefokus hat.
 *
 * Startet einen eigenen fake-daemon- und Electron-Prozess (mit
 * --remote-debugging-port), verbindet sich per WebSocket zum Renderer-Ziel
 * und führt darin JS über Runtime.evaluate aus (derselbe Ausführungskontext
 * wie die normale DevTools-Konsole - contextBridge-APIs wie
 * window.luftspiegel sind dort verfügbar). Beendet beide Prozesse am Ende
 * wieder sauber, auch bei einem fehlgeschlagenen Check.
 *
 * Start: node tools/ui-check.js
 */

const { spawn } = require('child_process');
const path = require('path');
const fs = require('fs');
const os = require('os');

const DEBUG_PORT = 9333;
const GUI_DIR = path.join(__dirname, '..');

function log(...args) {
  console.log('[ui-check]', ...args);
}

async function waitForHttp(url, timeoutMs) {
  const start = Date.now();
  let lastErr = null;
  while (Date.now() - start < timeoutMs) {
    try {
      const res = await fetch(url);
      if (res.ok) return await res.json();
    } catch (err) {
      lastErr = err;
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`Timeout beim Warten auf ${url}: ${lastErr && lastErr.message}`);
}

async function waitForTcp(host, port, timeoutMs) {
  const net = require('net');
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const ok = await new Promise((resolve) => {
      const s = net.createConnection(port, host);
      s.once('connect', () => {
        s.destroy();
        resolve(true);
      });
      s.once('error', () => resolve(false));
    });
    if (ok) return;
    await new Promise((r) => setTimeout(r, 150));
  }
  throw new Error(`Timeout beim Warten auf ${host}:${port}`);
}

/** Dünner CDP-Client auf Basis des in Node eingebauten WebSocket-Clients (Node 22+). */
class Cdp {
  constructor(wsUrl) {
    this.ws = new WebSocket(wsUrl);
    this.nextId = 1;
    this.pending = new Map();
    this.ws.addEventListener('message', (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.id !== undefined && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id);
        this.pending.delete(msg.id);
        if (msg.error) reject(new Error(msg.error.message));
        else resolve(msg.result);
      }
    });
  }

  ready() {
    return new Promise((resolve, reject) => {
      this.ws.addEventListener('open', () => resolve());
      this.ws.addEventListener('error', (ev) => reject(new Error('WebSocket-Fehler: ' + ev.message)));
    });
  }

  send(method, params = {}) {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }

  /** Führt einen JS-Ausdruck im Renderer (Hauptwelt, wie die normale DevTools-Konsole) aus. */
  async evaluate(expression) {
    const result = await this.send('Runtime.evaluate', {
      expression,
      awaitPromise: true,
      returnByValue: true,
    });
    if (result.exceptionDetails) {
      const desc =
        (result.exceptionDetails.exception && result.exceptionDetails.exception.description) ||
        result.exceptionDetails.text;
      throw new Error(`Renderer-Ausdruck warf einen Fehler: ${desc}`);
    }
    return result.result.value;
  }

  close() {
    this.ws.close();
  }
}

async function findRendererTarget() {
  const list = await waitForHttp(`http://127.0.0.1:${DEBUG_PORT}/json/list`, 15000);
  const page = list.find((t) => t.type === 'page' && /index\.html/.test(t.url || ''));
  if (!page) throw new Error('Kein passendes DevTools-Ziel (index.html) gefunden.');
  return page;
}

function assert(cond, message) {
  if (!cond) throw new Error(`FEHLGESCHLAGEN: ${message}`);
  log('OK:', message);
}

async function main() {
  let fakeDaemon = null;
  let electron = null;
  let cdp = null;
  let userDataDir = null;
  const failures = [];

  try {
    log('Starte fake-daemon.js …');
    fakeDaemon = spawn(process.execPath, [path.join(GUI_DIR, 'tools', 'fake-daemon.js')], {
      cwd: GUI_DIR,
      stdio: 'ignore',
    });
    await waitForTcp('127.0.0.1', 7654, 8000);
    log('fake-daemon bereit.');

    log('Starte Electron mit --remote-debugging-port …');
    const electronPath = require('electron');
    // Eigenes, frisches --user-data-dir pro Lauf: sonst landet settings.json
    // im echten Entwickler-Profil (app.getPath('userData')) und Zustand aus
    // einem Testlauf (z.B. Expertenmodus an, Preset "custom") würde in den
    // nächsten Lauf UND in die normale Entwicklungsnutzung durchsickern.
    userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'luftspiegel-ui-check-'));
    electron = spawn(electronPath, ['.', `--remote-debugging-port=${DEBUG_PORT}`, `--user-data-dir=${userDataDir}`], {
      cwd: GUI_DIR,
      stdio: 'ignore',
    });

    const target = await findRendererTarget();
    cdp = new Cdp(target.webSocketDebuggerUrl);
    await cdp.ready();
    await cdp.send('Runtime.enable');
    log('Mit Renderer verbunden:', target.url);

    // App muss ihre init() beendet haben (Settings geladen, Tabs gesetzt).
    await cdp.evaluate(`
      (async () => {
        const start = Date.now();
        while (!document.getElementById('tabBtnConnect') || !window.luftspiegel) {
          if (Date.now() - start > 5000) throw new Error('App nicht initialisiert.');
          await new Promise((r) => setTimeout(r, 100));
        }
        return true;
      })()
    `);

    // -----------------------------------------------------------------
    // Prüfpunkt 1: alle drei Tabs aktivieren, genau ein Panel sichtbar,
    // aria-selected konsistent.
    // -----------------------------------------------------------------
    const tabIds = ['connect', 'stats', 'settings'];
    for (const tab of tabIds) {
      const r = await cdp.evaluate(`
        (() => {
          document.getElementById('tabBtn${cap(tab)}').click();
          const panelState = ${JSON.stringify(tabIds)}.map((id) => ({
            id,
            hidden: document.getElementById('tab' + id[0].toUpperCase() + id.slice(1)).classList.contains('hidden'),
          }));
          const selState = ${JSON.stringify(tabIds)}.map((id) => ({
            id,
            selected: document.getElementById('tabBtn' + id[0].toUpperCase() + id.slice(1)).getAttribute('aria-selected'),
          }));
          return { panelState, selState };
        })()
      `);
      const visiblePanels = r.panelState.filter((p) => !p.hidden);
      // panelState[i].id ist der rohe Tab-Bezeichner ('connect'/'stats'/'settings'),
      // NICHT das DOM-Element-Präfix ("tabConnect") - hier gegen "tab" statt
      // "wantVisible" zu vergleichen war der Bug im ersten ui-check-Lauf.
      try {
        assert(visiblePanels.length === 1 && visiblePanels[0].id === tab, `Tab "${tab}": genau ein Panel sichtbar (tab${cap(tab)})`);
        const selectedOk = r.selState.every((s) => (s.id === tab ? s.selected === 'true' : s.selected === 'false'));
        assert(selectedOk, `Tab "${tab}": aria-selected konsistent (${JSON.stringify(r.selState)})`);
      } catch (err) {
        failures.push(err.message);
      }
    }

    // -----------------------------------------------------------------
    // Verbindung aufbauen (h264zyyx, kein PIN nötig), damit der
    // fake-daemon einen stats-Block liefert - direkt über die reale
    // contextBridge-API, keine Simulation der Steuerkanal-Bytes nötig.
    // -----------------------------------------------------------------
    log('Baue Testverbindung auf (h264zyyx) …');
    await cdp.evaluate(`window.luftspiegel.mirror.start({ target: '192.168.178.125', port: 7000, pin: '' })`);
    // fake-daemon sampelt alle 500ms, der GUI-Statuspoll läuft alle 1s
    // sobald stats vorhanden sind - 2.5s reichen für mehrere Samples plus
    // mindestens einen mirror:status-Push im Renderer.
    await new Promise((r) => setTimeout(r, 2500));

    // -----------------------------------------------------------------
    // Prüfpunkt 2: Statistik-Tab rendert plausible Kachelwerte und
    // nicht-leere Canvases.
    // -----------------------------------------------------------------
    const statsResult = await cdp.evaluate(`
      (() => {
        document.getElementById('tabBtnStats').click();
        const fps = document.getElementById('statFps').textContent;
        const bitrate = document.getElementById('statBitrate').textContent;
        const uptime = document.getElementById('statUptime').textContent;
        const contentHidden = document.getElementById('statsContent').classList.contains('hidden');

        function canvasHasPixels(id) {
          const canvas = document.getElementById(id);
          if (canvas.width === 0 || canvas.height === 0) return { drawn: false, w: canvas.width, h: canvas.height };
          const data = canvas.getContext('2d').getImageData(0, 0, canvas.width, canvas.height).data;
          let nonZeroAlpha = 0;
          for (let i = 3; i < data.length; i += 4) {
            if (data[i] > 0) nonZeroAlpha++;
          }
          return { drawn: nonZeroAlpha > 0, w: canvas.width, h: canvas.height, nonZeroAlpha };
        }

        return {
          fps,
          bitrate,
          uptime,
          contentHidden,
          sparkBitrate: canvasHasPixels('sparkBitrate'),
          sparkAuHold: canvasHasPixels('sparkAuHold'),
          sparkFps: canvasHasPixels('sparkFps'),
        };
      })()
    `);
    log('Statistik-Tab Rohdaten:', JSON.stringify(statsResult));
    try {
      assert(statsResult.contentHidden === false, 'Statistik-Tab: statsContent sichtbar (nicht Leerzustand)');
      assert(statsResult.fps !== '—' && statsResult.fps !== '', `Statistik-Tab: Bildrate-Kachel plausibel (${statsResult.fps})`);
      assert(statsResult.bitrate !== '—' && statsResult.bitrate !== '', `Statistik-Tab: Bitrate-Kachel plausibel (${statsResult.bitrate})`);
      assert(statsResult.uptime !== '—' && statsResult.uptime !== '', `Statistik-Tab: Laufzeit-Kachel plausibel (${statsResult.uptime})`);
      assert(statsResult.sparkBitrate.drawn, 'Sparkline Bitrate: Pixel gezeichnet');
      assert(statsResult.sparkAuHold.drawn, 'Sparkline Sendeverzögerung: Pixel gezeichnet');
      assert(statsResult.sparkFps.drawn, 'Sparkline Bildrate: Pixel gezeichnet');
    } catch (err) {
      failures.push(err.message);
    }

    // -----------------------------------------------------------------
    // Prüfpunkt 3: Preset umschalten -> Latenz&Encoder-Felder übernehmen
    // die Preset-Werte und sind (ohne Expertenmodus) disabled.
    // -----------------------------------------------------------------
    const presetResult = await cdp.evaluate(`
      (async () => {
        document.getElementById('tabBtnSettings').click();
        const sel = document.getElementById('presetSelect');
        sel.value = 'best_quality';
        sel.dispatchEvent(new Event('change'));
        await new Promise((r) => setTimeout(r, 400)); // onSettingsChanged ist async (IPC-Roundtrip)
        const ids = ['bitrate', 'targetLatencyMs', 'gopSeconds', 'audioBufferMs'];
        const values = {};
        const disabled = {};
        ids.forEach((id) => {
          values[id] = document.getElementById(id).value;
          disabled[id] = document.getElementById(id).disabled;
        });
        return { values, disabled, expertChecked: document.getElementById('expertMode').checked };
      })()
    `);
    log('Preset-Test Rohdaten:', JSON.stringify(presetResult));
    try {
      assert(presetResult.values.bitrate === '10000', `Preset "Beste Qualität": Bitrate übernommen (${presetResult.values.bitrate})`);
      assert(presetResult.values.targetLatencyMs === '200', `Preset "Beste Qualität": Ziellatenz übernommen (${presetResult.values.targetLatencyMs})`);
      assert(presetResult.values.gopSeconds === '2', `Preset "Beste Qualität": GOP übernommen (${presetResult.values.gopSeconds})`);
      assert(presetResult.values.audioBufferMs === '200', `Preset "Beste Qualität": Audio-Puffer übernommen (${presetResult.values.audioBufferMs})`);
      assert(!presetResult.expertChecked, 'Expertenmodus ist aus (Ausgangszustand)');
      assert(
        Object.values(presetResult.disabled).every(Boolean),
        `Latenz&Encoder-Felder sind ohne Expertenmodus disabled (${JSON.stringify(presetResult.disabled)})`
      );
    } catch (err) {
      failures.push(err.message);
    }

    // -----------------------------------------------------------------
    // Prüfpunkt 4: Expertenmodus an, Einzelwert ändern -> Preset springt
    // auf "Benutzerdefiniert".
    // -----------------------------------------------------------------
    const expertResult = await cdp.evaluate(`
      (async () => {
        const expertMode = document.getElementById('expertMode');
        expertMode.checked = true;
        expertMode.dispatchEvent(new Event('change'));
        await new Promise((r) => setTimeout(r, 400));
        const disabledAfterExpert = document.getElementById('targetLatencyMs').disabled;

        const targetLatency = document.getElementById('targetLatencyMs');
        targetLatency.value = '333';
        targetLatency.dispatchEvent(new Event('change'));
        await new Promise((r) => setTimeout(r, 400));

        return {
          disabledAfterExpert,
          presetAfterManualEdit: document.getElementById('presetSelect').value,
          targetLatencyValue: document.getElementById('targetLatencyMs').value,
        };
      })()
    `);
    log('Expertenmodus-Test Rohdaten:', JSON.stringify(expertResult));
    try {
      assert(!expertResult.disabledAfterExpert, 'Latenz&Encoder-Felder sind im Expertenmodus bedienbar (nicht disabled)');
      assert(expertResult.targetLatencyValue === '333', 'Manuelle Änderung der Ziellatenz wurde übernommen');
      assert(expertResult.presetAfterManualEdit === 'custom', `Preset springt nach manueller Änderung auf "custom" (war: ${expertResult.presetAfterManualEdit})`);
    } catch (err) {
      failures.push(err.message);
    }

    // Sauber trennen, bevor der Prozess beendet wird (TEARDOWN).
    await cdp.evaluate(`window.luftspiegel.mirror.stop({ target: '192.168.178.125' })`);
  } finally {
    if (cdp) cdp.close();
    if (electron) electron.kill();
    if (fakeDaemon) fakeDaemon.kill();
    if (userDataDir) {
      // Best effort: Electron braucht nach dem kill() einen Moment, bis es
      // Dateisperren auf dem Profilverzeichnis freigibt - ein fehlgeschlagenes
      // Aufräumen ist unkritisch (liegt in os.tmpdir(), kein echtes Profil).
      await new Promise((r) => setTimeout(r, 300));
      try {
        fs.rmSync(userDataDir, { recursive: true, force: true });
      } catch (err) {
        log('Hinweis: temporäres Testprofil konnte nicht entfernt werden:', err.message);
      }
    }
  }

  console.log('');
  if (failures.length) {
    console.log(`[ui-check] ${failures.length} Prüfung(en) fehlgeschlagen:`);
    failures.forEach((f) => console.log('  -', f));
    process.exitCode = 1;
  } else {
    console.log('[ui-check] Alle Prüfungen bestanden.');
  }
}

function cap(s) {
  return s[0].toUpperCase() + s.slice(1);
}

main().catch((err) => {
  console.error('[ui-check] Abbruch:', err.message);
  process.exitCode = 1;
});
