'use strict';

(() => {
  const api = window.luftspiegel;

  // -------------------------------------------------------------------
  // Zustand
  // -------------------------------------------------------------------

  /** @type {{name:string, model:string, ip:string, port:number}|null} */
  let selectedTarget = null;
  let currentSettings = null;
  let devices = [];
  let displays = [];
  let lastKnownConnected = false; // ob wir zuletzt eine aktive Verbindung gesehen haben
  let discovering = false; // eine manuell ausgelöste Suche (discover) läuft gerade
  let unsubscribers = [];

  // Statistik-Tab: der Daemon liefert die Historie bereits fertig aggregiert
  // (siehe stats.history) - hier wird bewusst KEINE eigene Historie
  // aufgebaut, sondern bei jedem Status-Push einfach der zuletzt gesehene
  // stats-Block gecacht und neu gezeichnet (auch bei Tab-Wechsel/Resize,
  // weil ein <canvas> in einem versteckten Tab eine 0x0-Backing-Store hat).
  let lastStats = null;
  let guiAudioDropped = 0;

  // Aktiver Tab ('connect' | 'stats' | 'settings') - siehe activateTab().
  let activeTab = 'connect';

  // -------------------------------------------------------------------
  // DOM-Referenzen
  // -------------------------------------------------------------------

  const el = {
    connPill: document.getElementById('connPill'),
    connLabel: document.getElementById('connLabel'),
    deviceList: document.getElementById('deviceList'),
    btnDiscover: document.getElementById('btnDiscover'),
    manualForm: document.getElementById('manualForm'),
    manualIp: document.getElementById('manualIp'),
    manualPort: document.getElementById('manualPort'),
    targetName: document.getElementById('targetName'),
    targetIp: document.getElementById('targetIp'),
    btnStart: document.getElementById('btnStart'),
    btnStop: document.getElementById('btnStop'),
    errorBanner: document.getElementById('errorBanner'),
    errorBannerText: document.getElementById('errorBannerText'),
    errorBannerDismiss: document.getElementById('errorBannerDismiss'),
    sidecarBanner: document.getElementById('sidecarBanner'),
    fpsField: document.getElementById('fpsField'),
    maxHeight: document.getElementById('maxHeight'),
    bitrate: document.getElementById('bitrate'),
    monitorSelect: document.getElementById('monitorSelect'),
    pinOverlay: document.getElementById('pinOverlay'),
    pinForm: document.getElementById('pinForm'),
    pinInput: document.getElementById('pinInput'),
    pinCancel: document.getElementById('pinCancel'),
    audioToggle: document.getElementById('audioToggle'),
    audioPill: document.getElementById('audioPill'),
    audioLabel: document.getElementById('audioLabel'),

    // Tabs
    tabBtnConnect: document.getElementById('tabBtnConnect'),
    tabBtnStats: document.getElementById('tabBtnStats'),
    tabBtnSettings: document.getElementById('tabBtnSettings'),
    tabConnect: document.getElementById('tabConnect'),
    tabStats: document.getElementById('tabStats'),
    tabSettings: document.getElementById('tabSettings'),

    // Einstellungen: Qualitäts-Preset & Expertenmodus
    presetSelect: document.getElementById('presetSelect'),
    expertMode: document.getElementById('expertMode'),

    // Einstellungen: Latenz & Encoder (nur im Expertenmodus bedienbar)
    targetLatencyMs: document.getElementById('targetLatencyMs'),
    gopSeconds: document.getElementById('gopSeconds'),
    rateControl: document.getElementById('rateControl'),
    hwaccel: document.getElementById('hwaccel'),
    audioBufferMs: document.getElementById('audioBufferMs'),

    // Einstellungen: Video
    showCursor: document.getElementById('showCursor'),

    // Einstellungen: App-Verhalten
    rememberLastDevice: document.getElementById('rememberLastDevice'),
    autoConnect: document.getElementById('autoConnect'),
    startMinimized: document.getElementById('startMinimized'),
    trayIcon: document.getElementById('trayIcon'),
    debugLogging: document.getElementById('debugLogging'),
    btnOpenLog: document.getElementById('btnOpenLog'),

    // Statistik-Tab
    statsEmpty: document.getElementById('statsEmpty'),
    statsContent: document.getElementById('statsContent'),
    statFps: document.getElementById('statFps'),
    statBitrate: document.getElementById('statBitrate'),
    statBitrateUnit: document.getElementById('statBitrateUnit'),
    statLatency: document.getElementById('statLatency'),
    statAuHold: document.getElementById('statAuHold'),
    statAuHoldP95: document.getElementById('statAuHoldP95'),
    statRtt: document.getElementById('statRtt'),
    statUptime: document.getElementById('statUptime'),
    sparkBitrate: document.getElementById('sparkBitrate'),
    sparkBitrateMax: document.getElementById('sparkBitrateMax'),
    sparkAuHold: document.getElementById('sparkAuHold'),
    sparkAuHoldMax: document.getElementById('sparkAuHoldMax'),
    sparkFps: document.getElementById('sparkFps'),
    sparkFpsMax: document.getElementById('sparkFpsMax'),
    statResolution: document.getElementById('statResolution'),
    statEncoder: document.getElementById('statEncoder'),
    statRateControl: document.getElementById('statRateControl'),
    statTargetLatency: document.getElementById('statTargetLatency'),
    statFramesSent: document.getElementById('statFramesSent'),
    statKeyframes: document.getElementById('statKeyframes'),
    statBytesSent: document.getElementById('statBytesSent'),
    statAudioBufferMs: document.getElementById('statAudioBufferMs'),
    statAudioUnderruns: document.getElementById('statAudioUnderruns'),
    statAudioDrainMs: document.getElementById('statAudioDrainMs'),
    statAudioDropped: document.getElementById('statAudioDropped'),
  };

  // -------------------------------------------------------------------
  // Tab-Navigation
  // -------------------------------------------------------------------

  const TAB_IDS = ['connect', 'stats', 'settings'];
  const tabButtons = { connect: el.tabBtnConnect, stats: el.tabBtnStats, settings: el.tabBtnSettings };
  const tabPanels = { connect: el.tabConnect, stats: el.tabStats, settings: el.tabSettings };

  /**
   * Schaltet den sichtbaren Tab um (bestehendes .hidden-Muster, kein
   * Routing). Persistiert standardmäßig in den Einstellungen, damit der
   * aktive Tab einen Neustart übersteht - das ist eine reine UI-Einstellung
   * und löst daher (siehe main.js daemonFlagsChanged) keinen Sidecar-
   * Neustart aus.
   */
  function activateTab(tab, opts = {}) {
    const { persist = true, focus = false } = opts;
    if (!TAB_IDS.includes(tab)) tab = 'connect';
    activeTab = tab;

    TAB_IDS.forEach((id) => {
      const isActive = id === tab;
      tabPanels[id].classList.toggle('hidden', !isActive);
      tabButtons[id].setAttribute('aria-selected', String(isActive));
      tabButtons[id].tabIndex = isActive ? 0 : -1;
    });

    if (focus) tabButtons[tab].focus();
    if (tab === 'stats') renderStatsTab(); // Canvas hatte bis eben ggf. 0x0 Backing-Store
    if (persist) saveUiSetting({ activeTab: tab });
  }

  Object.values(tabButtons).forEach((btn) => {
    btn.addEventListener('click', () => activateTab(btn.dataset.tab));
  });

  /** Pfeiltasten-Navigation nach dem üblichen ARIA-Tabs-Muster (roving tabindex, automatische Aktivierung). */
  function handleTabKeydown(ev) {
    const idx = TAB_IDS.indexOf(activeTab);
    let nextIdx = null;
    if (ev.key === 'ArrowRight' || ev.key === 'ArrowDown') nextIdx = (idx + 1) % TAB_IDS.length;
    else if (ev.key === 'ArrowLeft' || ev.key === 'ArrowUp') nextIdx = (idx - 1 + TAB_IDS.length) % TAB_IDS.length;
    else if (ev.key === 'Home') nextIdx = 0;
    else if (ev.key === 'End') nextIdx = TAB_IDS.length - 1;
    if (nextIdx !== null) {
      ev.preventDefault();
      activateTab(TAB_IDS[nextIdx], { focus: true });
    }
  }
  Object.values(tabButtons).forEach((btn) => btn.addEventListener('keydown', handleTabKeydown));

  // -------------------------------------------------------------------
  // Hilfsfunktionen: Anzeige
  // -------------------------------------------------------------------

  /**
   * Verbindungsfehler bleiben stehen, bis der Nutzer erneut etwas auslöst
   * (Start/Suchen ruft showError(null) selbst zu Beginn auf) oder sie
   * über den ×-Button wegklickt. Der passive Statuspoll darf diese Anzeige
   * NICHT eigenständig setzen oder löschen (siehe applyConnectionState, das
   * den Banner absichtlich nicht anfasst). Steht der Nutzer gerade auf
   * einem anderen Tab, wenn ein echter Fehler auftritt, wird automatisch
   * auf "Verbinden" gewechselt, weil der Banner nur dort sichtbar ist.
   */
  function showError(message) {
    if (!message) {
      el.errorBanner.classList.add('hidden');
      el.errorBannerText.textContent = '';
      return;
    }
    el.errorBannerText.textContent = message;
    el.errorBanner.classList.remove('hidden');
    activateTab('connect');
  }

  el.errorBannerDismiss.addEventListener('click', () => showError(null));

  function showSidecarNotice(message) {
    if (!message) {
      el.sidecarBanner.classList.add('hidden');
      el.sidecarBanner.textContent = '';
      return;
    }
    el.sidecarBanner.textContent = message;
    el.sidecarBanner.classList.remove('hidden');
    activateTab('connect');
  }

  /**
   * Ordnet eine Status-Antwort einer der vier UI-Zustände zu:
   * disconnected / connecting / connected / error.
   * Der genaue Wortlaut des "state"-Feldes ist daemon-seitig nicht
   * abschließend spezifiziert, daher wird tolerant per Schlüsselwort
   * klassifiziert und der Rohtext als Label übernommen, wenn vorhanden.
   */
  function classifyStatus(resp) {
    if (!resp || resp.ok === false) {
      return { bucket: 'error', label: (resp && resp.error) || 'Fehler' };
    }
    const raw = (resp.state || '').toString().trim();
    const low = raw.toLowerCase();

    let bucket = 'disconnected';
    if (/(idle|disconnect|getrennt|none)/.test(low) || low === '') {
      bucket = 'disconnected';
    } else if (/(pair|verbinde|connecting|handshake|discov)/.test(low)) {
      bucket = 'connecting';
    } else if (/(stream|mirror|verbunden|connected|active|running)/.test(low)) {
      bucket = 'connected';
    }

    const fallbackLabel =
      bucket === 'disconnected' ? 'Getrennt' : bucket === 'connecting' ? 'Verbinde…' : bucket === 'connected' ? 'Verbunden' : 'Fehler';

    return { bucket, label: raw || fallbackLabel };
  }

  /**
   * Aktualisiert NUR die Verbindungsanzeige (Pille/Label/Start-Stop-Buttons)
   * aus einer Status-Antwort. Fasst absichtlich weder den Fehlerbanner noch
   * den PIN-Dialog an - beides wird ausschließlich von expliziten
   * Nutzeraktionen (startMirroring/stopMirroring) gesteuert, damit der
   * passive Statuspoll (mirror:status) keine überraschenden Seiteneffekte
   * auslöst (z.B. einen gerade angezeigten Verbindungsfehler wieder
   * verschwinden lassen, oder unaufgefordert den PIN-Dialog öffnen).
   *
   * Cacht zusätzlich den optionalen "stats"-Block und den GUI-seitigen
   * Zähler verworfener Audio-Chunks für den Statistik-Tab - das ist reines
   * Zwischenspeichern für renderStatsTab() und ändert nichts an der oben
   * beschriebenen Zurückhaltung bei Bannern/Dialogen.
   */
  function applyConnectionState(resp) {
    const { bucket, label } = classifyStatus(resp);
    el.connPill.dataset.state = bucket;
    el.connLabel.textContent = label;

    lastKnownConnected = bucket === 'connected' || bucket === 'connecting';
    el.btnStop.disabled = !lastKnownConnected;
    el.btnStart.disabled = lastKnownConnected || !selectedTarget;

    lastStats = resp && resp.stats ? resp.stats : null;
    if (resp && typeof resp.guiAudioDropped === 'number') {
      guiAudioDropped = resp.guiAudioDropped;
    }
    renderStatsTab();
  }

  // -------------------------------------------------------------------
  // Statistik-Tab
  // -------------------------------------------------------------------

  function formatBytesHuman(n) {
    if (!Number.isFinite(n) || n < 0) return '—';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let value = n;
    let unitIndex = 0;
    while (value >= 1024 && unitIndex < units.length - 1) {
      value /= 1024;
      unitIndex += 1;
    }
    return `${value.toFixed(unitIndex === 0 ? 0 : 1)} ${units[unitIndex]}`;
  }

  function formatUptime(sec) {
    if (!Number.isFinite(sec) || sec < 0) return '—';
    const total = Math.floor(sec);
    const h = Math.floor(total / 3600);
    const m = Math.floor((total % 3600) / 60);
    const s = total % 60;
    const pad = (n) => String(n).padStart(2, '0');
    return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
  }

  function fmtMs(v, digits = 1) {
    return Number.isFinite(v) ? v.toFixed(digits) : '—';
  }

  /** "—" statt 0, wenn der Wert schlicht (noch) nicht gemeldet wurde. */
  function fmtMsOrDash(v, digits = 1) {
    return Number.isFinite(v) && v > 0 ? v.toFixed(digits) : '—';
  }

  const RATE_CONTROL_LABELS = { display_remoting: 'Display Remoting', cbr_live: 'CBR Live' };

  // Fallback-Skala, falls alle gültigen Punkte in einem Fenster 0 sind -
  // ohne das gäbe es bei max=0 eine Division durch 0 / eine Linie, die
  // exakt auf der unteren Kante "verschwindet". Rein visuell, geht NICHT
  // in den zurückgegebenen (echten) Max-Wert ein.
  const SPARKLINE_MIN_SCALE = 1;

  /**
   * Zeichnet eine Sparkline aus einem Werte-Array in ein <canvas>.
   * devicePixelRatio wird berücksichtigt (Backing-Store wird skaliert),
   * sonst wirkt die Linie auf HiDPI-Displays unscharf.
   *
   * WICHTIG: 0 ist ein echter Messwert und wird MITGEZEICHNET, nicht als
   * Lücke übersprungen. Der Daemon liefert früh in einer Session ein
   * entsprechend kürzeres history-Array (keine Nullen vor Sessionstart) -
   * eine 0 innerhalb der gelieferten Punkte bedeutet also: in diesem
   * 500ms-Slot wurde tatsächlich kein Frame gesendet, ein echter Stall.
   * Das ist die wichtigste Information in dieser Kurve und darf nicht
   * durch "Glätten" verschwinden. Nur null/undefined/NaN/negative Werte
   * (fehlendes Feld) werden als Lücke übersprungen.
   *
   * Gibt den ECHTEN Max-Wert zurück (auch 0, falls alle Punkte 0 sind),
   * oder null, wenn es überhaupt keinen gültigen Punkt gab.
   */
  function drawSparkline(canvas, values, color, fillColor) {
    const rect = canvas.getBoundingClientRect();
    const w = Math.max(1, Math.round(rect.width));
    const h = Math.max(1, Math.round(rect.height));
    if (rect.width === 0 || rect.height === 0) return null; // Tab evtl. gerade nicht sichtbar (display:none)

    const dpr = window.devicePixelRatio || 1;
    const targetW = Math.round(w * dpr);
    const targetH = Math.round(h * dpr);
    if (canvas.width !== targetW || canvas.height !== targetH) {
      canvas.width = targetW;
      canvas.height = targetH;
    }

    const ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, h);

    const n = values.length;
    if (!n) return null;

    const indexed = values.map((v, i) => (Number.isFinite(v) && v >= 0 ? { i, v } : null));
    const valid = indexed.filter(Boolean);
    if (!valid.length) return null;

    const max = valid.reduce((m, p) => Math.max(m, p.v), 0);
    const scaleMax = max > 0 ? max : SPARKLINE_MIN_SCALE; // nur für die Y-Skalierung, nicht für den Rückgabewert
    const stepX = n > 1 ? w / (n - 1) : w;
    const padY = 3; // etwas Luft oben/unten, damit die Linie nicht am Rand klebt

    ctx.beginPath();
    let started = false;
    indexed.forEach((p) => {
      if (!p) {
        started = false; // Lücke (fehlender Slot): nächster gültiger Punkt beginnt einen neuen Linienabschnitt
        return;
      }
      const x = p.i * stepX;
      const y = h - padY - (p.v / scaleMax) * (h - padY * 2);
      if (!started) {
        ctx.moveTo(x, y);
        started = true;
      } else {
        ctx.lineTo(x, y);
      }
    });
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.5;
    ctx.lineJoin = 'round';
    ctx.stroke();

    // Fläche unter der Kurve, leicht gefüllt - nur über den zusammenhängend
    // gültigen Bereich vom ersten bis zum letzten echten Messpunkt.
    const first = valid[0];
    const last = valid[valid.length - 1];
    ctx.lineTo(last.i * stepX, h);
    ctx.lineTo(first.i * stepX, h);
    ctx.closePath();
    ctx.fillStyle = fillColor;
    ctx.fill();

    return max;
  }

  function getAccentColors() {
    const styles = getComputedStyle(document.documentElement);
    const accent = styles.getPropertyValue('--accent').trim() || '#2fd3c4';
    return { accent, fill: 'rgba(47, 211, 196, 0.15)' };
  }

  /**
   * Zeichnet Kacheln, Sparklines und Detailtabelle aus dem zuletzt gecachten
   * stats-Block (lastStats) neu. Wird sowohl bei jedem Status-Push als auch
   * beim Wechsel auf den Statistik-Tab und bei Fenster-Resize aufgerufen -
   * hier wird NICHTS aus mehreren Ticks aufsummiert, sondern immer nur der
   * aktuellste stats-Block dargestellt (die History liefert der Daemon
   * bereits fertig aggregiert).
   */
  function renderStatsTab() {
    const hasStats = !!lastStats;
    el.statsEmpty.classList.toggle('hidden', hasStats);
    el.statsContent.classList.toggle('hidden', !hasStats);
    if (!hasStats) return;

    const s = lastStats;
    const { accent, fill } = getAccentColors();

    // Kacheln
    el.statFps.textContent = Number.isFinite(s.fps) ? s.fps.toFixed(1) : '—';
    if (Number.isFinite(s.bitrate_kbps) && s.bitrate_kbps >= 1000) {
      el.statBitrate.textContent = (s.bitrate_kbps / 1000).toFixed(2);
      el.statBitrateUnit.textContent = 'Mbit/s';
    } else {
      el.statBitrate.textContent = Number.isFinite(s.bitrate_kbps) ? s.bitrate_kbps.toFixed(0) : '—';
      el.statBitrateUnit.textContent = 'kbit/s';
    }
    el.statLatency.textContent = fmtMsOrDash(s.receiver_render_latency_ms, 0);
    el.statAuHold.textContent = fmtMs(s.au_hold_ms_p50);
    el.statAuHoldP95.textContent = fmtMs(s.au_hold_ms_p95);
    el.statRtt.textContent = fmtMsOrDash(s.rtt_ms);
    el.statUptime.textContent = formatUptime(s.uptime_sec);

    // Sparklines (~60s Fenster aus stats.history, vom Daemon bereits fertig
    // in 500ms-Auflösung geliefert - siehe drawSparkline). drawSparkline()
    // gibt bei einem durchgängig auf 0 liegenden Fenster ebenfalls 0 zurück
    // (kein "kein Wert") - deshalb hier explizit auf "!== null" statt auf
    // Truthy prüfen, sonst würde "max 0" fälschlich als leer behandelt.
    const history = Array.isArray(s.history) ? s.history : [];
    const bitrateMax = drawSparkline(el.sparkBitrate, history.map((h) => h && h.bitrate_kbps), accent, fill);
    el.sparkBitrateMax.textContent =
      bitrateMax !== null ? `max ${(bitrateMax / 1000).toFixed(bitrateMax >= 1000 ? 2 : 0)} ${bitrateMax >= 1000 ? 'Mbit/s' : 'kbit/s'}` : '';
    const auHoldMax = drawSparkline(el.sparkAuHold, history.map((h) => h && h.au_hold_ms), accent, fill);
    el.sparkAuHoldMax.textContent = auHoldMax !== null ? `max ${auHoldMax.toFixed(1)} ms` : '';
    const fpsMax = drawSparkline(el.sparkFps, history.map((h) => h && h.fps), accent, fill);
    el.sparkFpsMax.textContent = fpsMax !== null ? `max ${fpsMax.toFixed(1)} fps` : '';

    // Detailtabelle
    el.statResolution.textContent = s.width && s.height ? `${s.width}×${s.height}` : '—';
    el.statEncoder.textContent = s.encoder || '—';
    el.statRateControl.textContent = RATE_CONTROL_LABELS[s.rate_control] || s.rate_control || '—';
    el.statTargetLatency.textContent = Number.isFinite(s.target_latency_ms) ? `${s.target_latency_ms} ms` : '—';
    el.statFramesSent.textContent = Number.isFinite(s.frames_sent) ? String(s.frames_sent) : '—';
    el.statKeyframes.textContent = Number.isFinite(s.keyframes) ? String(s.keyframes) : '—';
    el.statBytesSent.textContent = formatBytesHuman(s.bytes_sent);
    el.statAudioBufferMs.textContent = Number.isFinite(s.audio_buffer_ms) ? `${s.audio_buffer_ms.toFixed(0)} ms` : '—';
    el.statAudioUnderruns.textContent = Number.isFinite(s.audio_underruns) ? String(s.audio_underruns) : '—';
    el.statAudioDrainMs.textContent = Number.isFinite(s.audio_drain_ms) ? `${s.audio_drain_ms.toFixed(0)} ms` : '—';
    el.statAudioDropped.textContent = String(guiAudioDropped);
  }

  // Bei Fenster-Resize neu zeichnen (rAF-entprellt) - ein <canvas> behält
  // sein zuletzt gesetztes Backing-Store sonst auch bei geänderter
  // Layoutgröße bei und wirkt verzerrt/leer.
  let resizeRaf = null;
  window.addEventListener('resize', () => {
    if (resizeRaf !== null) return;
    resizeRaf = requestAnimationFrame(() => {
      resizeRaf = null;
      if (activeTab === 'stats') renderStatsTab();
    });
  });

  // -------------------------------------------------------------------
  // Geräteliste
  // -------------------------------------------------------------------

  function renderDevices() {
    el.deviceList.innerHTML = '';

    if (discovering) {
      const li = document.createElement('li');
      li.className = 'device-loading';
      const spinner = document.createElement('span');
      spinner.className = 'spinner';
      const text = document.createElement('span');
      text.textContent = 'Suche Geräte im Netzwerk… (kann bei aktivem VPN einige Sekunden dauern)';
      li.appendChild(spinner);
      li.appendChild(text);
      el.deviceList.appendChild(li);
      return;
    }

    if (!devices.length) {
      // Bewusst kein "Fehler" - ein aktives VPN verhindert auf diesem
      // Rechner regelmäßig die automatische mDNS-Suche. Die manuelle
      // IP-Eingabe ist der gleichwertige, empfohlene Weg.
      const li = document.createElement('li');
      li.className = 'device-hint';
      const strongIp = document.createElement('strong');
      strongIp.textContent = '192.168.178.125';
      li.append(
        'Kein Gerät gefunden. Ein aktives VPN kann die automatische Suche verhindern — IP unten direkt eingeben (bekannter Apple TV: ',
        strongIp,
        ').'
      );
      el.deviceList.appendChild(li);
      return;
    }

    devices.forEach((dev) => {
      const li = document.createElement('li');
      li.className = 'device-item';
      if (selectedTarget && selectedTarget.ip === dev.ip && selectedTarget.port === dev.port) {
        li.classList.add('selected');
      }

      const name = document.createElement('span');
      name.className = 'device-name';
      name.textContent = dev.name || dev.ip;

      const meta = document.createElement('span');
      meta.className = 'device-meta';
      meta.textContent = `${dev.model || 'unbekanntes Modell'} · ${dev.ip}:${dev.port || 7000}`;

      li.appendChild(name);
      li.appendChild(meta);

      li.addEventListener('click', () => {
        selectTarget({ name: dev.name || dev.ip, model: dev.model || '', ip: dev.ip, port: dev.port || 7000 });
        renderDevices();
      });

      el.deviceList.appendChild(li);
    });
  }

  async function discoverDevices() {
    // Ein neuer Suchversuch ist eine explizite Nutzeraktion - eine evtl.
    // noch stehende alte Fehlermeldung wird jetzt verworfen.
    showError(null);
    discovering = true;
    el.btnDiscover.disabled = true;
    el.btnDiscover.textContent = 'Suche läuft…';
    renderDevices();

    try {
      // discover kann bei aktivem VPN (kein mDNS) bis zu ~5s dauern, weil
      // der Daemon auf einen Netzwerk-Scan-Fallback ausweicht.
      const resp = await api.devices.discover();
      if (resp && resp.ok === false) {
        showError(resp.error || 'Suche fehlgeschlagen.');
        devices = [];
      } else {
        devices = (resp && resp.devices) || [];
      }
    } catch (err) {
      showError(`Suche fehlgeschlagen: ${err.message}`);
      devices = [];
    } finally {
      discovering = false;
      el.btnDiscover.disabled = false;
      el.btnDiscover.textContent = 'Suchen';
      renderDevices();
    }
  }

  // -------------------------------------------------------------------
  // Zielauswahl
  // -------------------------------------------------------------------

  function selectTarget(target) {
    selectedTarget = target;
    el.targetName.textContent = target.name || target.ip;
    el.targetIp.textContent = `${target.ip}:${target.port}`;
    el.btnStart.disabled = lastKnownConnected;
  }

  // -------------------------------------------------------------------
  // Start / Stop
  // -------------------------------------------------------------------

  async function startMirroring(pin) {
    if (!selectedTarget) return;
    showError(null);
    el.btnStart.disabled = true;
    el.connPill.dataset.state = 'connecting';
    el.connLabel.textContent = 'Verbinde…';

    try {
      const resp = await api.mirror.start({
        target: selectedTarget.ip,
        port: selectedTarget.port,
        pin: pin || '',
      });
      if (resp && resp.needs_pin) {
        openPinDialog();
        return;
      }
      if (!resp || resp.ok === false) {
        showError((resp && resp.error) || 'Verbindung fehlgeschlagen.');
        applyConnectionState(resp);
        return;
      }
      applyConnectionState(resp);

      // Erfolgreiche, vom Nutzer ausgelöste Verbindung merken (nicht bei
      // jedem passiven Statuspoll, sonst würde ständig auf die Platte
      // geschrieben) - nur falls "letztes Gerät merken" aktiv ist.
      if (currentSettings && currentSettings.rememberLastDevice) {
        const changed =
          !currentSettings.lastDevice ||
          currentSettings.lastDevice.ip !== selectedTarget.ip ||
          currentSettings.lastDevice.port !== selectedTarget.port;
        if (changed) {
          saveUiSetting({ lastDevice: { name: selectedTarget.name, ip: selectedTarget.ip, port: selectedTarget.port } });
        }
      }
    } catch (err) {
      showError(`Verbindung fehlgeschlagen: ${err.message}`);
      el.btnStart.disabled = !selectedTarget;
    }
  }

  async function stopMirroring() {
    el.btnStop.disabled = true;
    try {
      const resp = await api.mirror.stop(selectedTarget ? { target: selectedTarget.ip } : {});
      if (!resp || resp.ok === false) {
        showError((resp && resp.error) || 'Trennen fehlgeschlagen.');
      }
      applyConnectionState(resp && resp.ok !== false ? resp : { ok: true, state: 'getrennt' });
    } catch (err) {
      showError(`Trennen fehlgeschlagen: ${err.message}`);
    }
  }

  // -------------------------------------------------------------------
  // PIN-Dialog
  // -------------------------------------------------------------------

  function openPinDialog() {
    el.pinInput.value = '';
    el.pinOverlay.classList.remove('hidden');
    el.pinInput.focus();
    activateTab('connect');
  }

  function closePinDialog() {
    el.pinOverlay.classList.add('hidden');
  }

  el.pinForm.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const pin = el.pinInput.value.trim();
    closePinDialog();
    startMirroring(pin);
  });

  el.pinCancel.addEventListener('click', () => {
    closePinDialog();
    el.btnStart.disabled = !selectedTarget;
    el.connPill.dataset.state = 'disconnected';
    el.connLabel.textContent = 'Getrennt';
  });

  // -------------------------------------------------------------------
  // System-Audio-Erfassung (Loopback -> PCM -> Main-Prozess -> Sidecar)
  // -------------------------------------------------------------------

  /**
   * Alles hier läuft ausschließlich im Renderer, weil getDisplayMedia/
   * AudioWorklet nur dort verfügbar sind. Der eigentliche TCP-Versand
   * (127.0.0.1:7655) passiert im Main-Prozess (sandbox: true, kein
   * nodeIntegration hier) - siehe api.audio.sendChunk / preload.js.
   */
  let audioCaptureActive = false;
  let audioMediaStream = null;
  let audioCtx = null;
  let audioSourceNode = null;
  let audioWorkletNode = null;

  function stopAudioMediaStream() {
    if (audioMediaStream) {
      audioMediaStream.getTracks().forEach((t) => t.stop());
      audioMediaStream = null;
    }
  }

  /** Räumt alle Audio-Erfassungsressourcen auf, ohne die Bridge-Anzeige zu berühren (die kommt vom Main-Prozess). */
  function teardownAudioCapture() {
    audioCaptureActive = false;
    if (audioWorkletNode) {
      audioWorkletNode.port.onmessage = null;
      audioWorkletNode.disconnect();
      audioWorkletNode = null;
    }
    if (audioSourceNode) {
      audioSourceNode.disconnect();
      audioSourceNode = null;
    }
    if (audioCtx) {
      const ctx = audioCtx;
      audioCtx = null;
      ctx.close().catch(() => {});
    }
    stopAudioMediaStream();
  }

  /**
   * Startet die Aufnahme. Muss aus einem Kontext mit aktiver Nutzergeste
   * aufgerufen werden (Klick auf den Schalter) - getDisplayMedia
   * verlangt das. Gibt true bei Erfolg zurück, sonst false (und zeigt
   * einen Fehler im bestehenden Fehlerbanner an).
   */
  async function startAudioCapture() {
    if (audioCaptureActive) return true;
    audioCaptureActive = true;

    let stream;
    try {
      // restrictOwnAudio verhindert (sobald von Electron unterstützt),
      // dass die App ihre eigenen Töne mit erfasst - in dieser gepinnten
      // Electron-Version (v42) noch ohne Wirkung, siehe Hinweistext unter
      // dem Schalter. Der Fix landet mit Electron v44; da unbekannte
      // Constraint-Schlüssel vom Browser ignoriert werden, kann die
      // Option schon jetzt gefahrlos gesetzt werden.
      stream = await navigator.mediaDevices.getDisplayMedia({
        video: true,
        audio: { restrictOwnAudio: true },
      });
    } catch (err) {
      audioCaptureActive = false;
      showError(`Audioaufnahme fehlgeschlagen: ${err.message}`);
      return false;
    }

    audioMediaStream = stream;

    const audioTracks = stream.getAudioTracks();
    if (!audioTracks.length) {
      showError('Kein System-Audio verfügbar (Loopback lieferte keine Tonspur).');
      teardownAudioCapture();
      return false;
    }

    // Der Video-Track wird nur gebraucht, um überhaupt an den Audio-Track
    // zu kommen (das Bild macht der Go-Sidecar per ffmpeg) - sofort
    // stoppen, sobald wir die Tonspur haben.
    stream.getVideoTracks().forEach((t) => t.stop());

    try {
      // sampleRate explizit setzen, sonst könnte die Systemrate abweichen
      // und der Empfänger bekäme ein falsches Format vorgegaukelt. 44100Hz
      // (nicht 48000!) ist vertraglich fest, weil die gesamte ALAC/RTP-
      // Pipeline auf der Go-Seite hart auf 44100 verdrahtet ist.
      audioCtx = new AudioContext({ sampleRate: 44100 });
      console.log(`[audio] AudioContext.sampleRate = ${audioCtx.sampleRate} (angefordert: 44100)`);
      if (audioCtx.sampleRate !== 44100) {
        // Chromium ist NICHT verpflichtet, die angeforderte Rate zu
        // liefern - im Zweifel still auf die Geräterate zurückfallen.
        // Das wäre ein stiller Formatfehler, den nur die gemessene
        // Byterate beim Empfänger aufdecken würde - daher hier laut
        // melden statt nur zu loggen.
        console.error(
          `AudioContext.sampleRate ist ${audioCtx.sampleRate}, nicht die angeforderten 44100 Hz - PCM-Rate würde nicht zum Vertrag passen.`
        );
      }
      await audioCtx.audioWorklet.addModule('pcm-worklet.js');
    } catch (err) {
      showError(`Audio-Worklet konnte nicht geladen werden: ${err.message}`);
      teardownAudioCapture();
      return false;
    }

    audioSourceNode = audioCtx.createMediaStreamSource(stream);
    audioWorkletNode = new AudioWorkletNode(audioCtx, 'pcm-encoder', {
      numberOfInputs: 1,
      numberOfOutputs: 0,
      channelCount: 2,
      channelCountMode: 'explicit',
    });
    audioWorkletNode.port.onmessage = (ev) => {
      api.audio.sendChunk(ev.data);
    };
    audioSourceNode.connect(audioWorkletNode);

    return true;
  }

  function stopAudioCapture() {
    teardownAudioCapture();
  }

  /** Zeigt/verbirgt und beschriftet die Ton-Statuspille (Zustand kommt vom Main-Prozess, also der TCP-Bridge zu Port 7655). */
  function updateAudioPill(payload) {
    const enabled = !!(currentSettings && currentSettings.audioEnabled);
    el.audioPill.classList.toggle('hidden', !enabled);
    if (!enabled) return;

    const state = (payload && payload.state) || 'disconnected';
    const labels = {
      disconnected: 'Ton: aus',
      connecting: 'Ton: verbinde…',
      connected: 'Ton: verbunden',
      error: 'Ton: kein Empfänger',
    };
    el.audioPill.dataset.state = state;
    el.audioLabel.textContent = labels[state] || 'Ton: unbekannt';
    el.audioPill.title = (payload && payload.detail) || '';
    if (payload && typeof payload.droppedChunks === 'number') {
      guiAudioDropped = payload.droppedChunks;
    }
  }

  // -------------------------------------------------------------------
  // Einstellungen
  // -------------------------------------------------------------------

  // Qualitäts-Presets: setzen Bitrate/GOP/Ziellatenz/Audio-Puffer gemeinsam.
  // "custom" wird nicht hier gelistet - das ist der Zustand, wenn der
  // Nutzer im Expertenmodus einen dieser vier Werte manuell verstellt.
  const QUALITY_PRESETS = {
    lowest_latency: { bitrate: 6000, gopSeconds: 8, targetLatencyMs: 50, audioBufferMs: 80 },
    balanced: { bitrate: 0, gopSeconds: 4, targetLatencyMs: 100, audioBufferMs: 120 },
    best_quality: { bitrate: 10000, gopSeconds: 2, targetLatencyMs: 200, audioBufferMs: 200 },
  };

  const LATENCY_ENCODER_INPUTS = [el.bitrate, el.targetLatencyMs, el.gopSeconds, el.rateControl, el.hwaccel, el.audioBufferMs];

  /** Schreibt Werte direkt ins DOM (löst bewusst KEIN change-Event aus), damit ein Preset in einem einzigen settings:set landet statt in vieren. */
  function applyPresetToForm(values) {
    el.bitrate.value = String(values.bitrate);
    el.targetLatencyMs.value = String(values.targetLatencyMs);
    el.gopSeconds.value = String(values.gopSeconds);
    el.audioBufferMs.value = String(values.audioBufferMs);
  }

  function setLatencyGroupDisabled(disabled) {
    LATENCY_ENCODER_INPUTS.forEach((input) => {
      input.disabled = disabled;
    });
  }

  function fillSettingsForm(settings) {
    currentSettings = settings;

    const radios = el.fpsField.querySelectorAll('input[name="fps"]');
    radios.forEach((r) => {
      r.checked = Number(r.value) === Number(settings.fps);
    });
    el.maxHeight.value = String(settings.maxHeight);
    el.bitrate.value = String(settings.bitrate);
    el.audioToggle.checked = !!settings.audioEnabled;
    if (el.monitorSelect.options.length) {
      el.monitorSelect.value = String(settings.outputIndex);
    }

    el.presetSelect.value = settings.preset;
    el.expertMode.checked = !!settings.expertMode;
    setLatencyGroupDisabled(!settings.expertMode);

    el.targetLatencyMs.value = String(settings.targetLatencyMs);
    el.gopSeconds.value = String(settings.gopSeconds);
    el.rateControl.value = settings.rateControl;
    el.hwaccel.value = settings.hwaccel;
    el.audioBufferMs.value = String(settings.audioBufferMs);

    el.showCursor.checked = settings.showCursor !== false;

    el.rememberLastDevice.checked = settings.rememberLastDevice !== false;
    el.autoConnect.checked = !!settings.autoConnect;
    el.startMinimized.checked = !!settings.startMinimized;
    el.trayIcon.checked = !!settings.trayIcon;
    el.debugLogging.checked = !!settings.debugLogging;

    updateAudioPill(null);
    activateTab(settings.activeTab || 'connect', { persist: false });
  }

  /**
   * Baut das vollständige Settings-Objekt aus dem Formular. Startet bewusst
   * mit einer Kopie von currentSettings (statt einem frischen Literal) und
   * überschreibt nur die Felder, die tatsächlich ein Formularelement haben
   * - sonst würden reine UI-Felder ohne eigenes Control (activeTab,
   * lastDevice) bei jeder Formular-Änderung auf ihren Default zurückfallen,
   * weil settings:set das Settings-Objekt komplett ersetzt (main.js).
   */
  function readSettingsFromForm() {
    const checked = el.fpsField.querySelector('input[name="fps"]:checked');
    return {
      ...currentSettings,
      fps: checked ? Number(checked.value) : 30,
      maxHeight: Number(el.maxHeight.value),
      bitrate: Number(el.bitrate.value) || 0,
      outputIndex: Number(el.monitorSelect.value) || 0,
      audioEnabled: !!el.audioToggle.checked,

      preset: el.presetSelect.value,
      expertMode: !!el.expertMode.checked,

      targetLatencyMs: Number(el.targetLatencyMs.value) || 0,
      gopSeconds: Number(el.gopSeconds.value) || 0,
      rateControl: el.rateControl.value,
      hwaccel: el.hwaccel.value,
      audioBufferMs: Number(el.audioBufferMs.value) || 0,

      showCursor: !!el.showCursor.checked,

      rememberLastDevice: !!el.rememberLastDevice.checked,
      autoConnect: !!el.autoConnect.checked,
      startMinimized: !!el.startMinimized.checked,
      trayIcon: !!el.trayIcon.checked,
      debugLogging: !!el.debugLogging.checked,
    };
  }

  async function onSettingsChanged() {
    const newSettings = readSettingsFromForm();
    showSidecarNoticeQuiet('Einstellungen werden übernommen — Sidecar wird ggf. neu gestartet…');
    try {
      const resp = await api.settings.set(newSettings);
      if (!resp || resp.ok === false) {
        showSidecarNoticeQuiet(null);
        showError((resp && resp.error) || 'Einstellungen konnten nicht übernommen werden.');
        return;
      }
      currentSettings = resp.settings;
      updateAudioPill(null);
      if (resp.restarted) {
        showSidecarNoticeQuiet('Einstellungen übernommen, Sidecar wurde neu gestartet.');
        setTimeout(() => showSidecarNoticeQuiet(null), 4000);
      } else if (resp.note) {
        showSidecarNoticeQuiet(resp.note);
      } else {
        showSidecarNoticeQuiet(null);
      }
    } catch (err) {
      showSidecarNoticeQuiet(null);
      showError(`Einstellungen konnten nicht übernommen werden: ${err.message}`);
    }
  }

  // showSidecarNotice() selbst wechselt (wie showError) auf den Tab
  // "Verbinden", weil der Banner nur dort sichtbar ist - das ist beim
  // Ändern von Einstellungen (der Nutzer steht ja gerade auf "Einstellungen")
  // aber unerwünscht, deshalb hier ein Alias ohne Tab-Wechsel für den
  // reinen "wird übernommen…"-Fortschrittstext.
  function showSidecarNoticeQuiet(message) {
    if (!message) {
      el.sidecarBanner.classList.add('hidden');
      el.sidecarBanner.textContent = '';
      return;
    }
    el.sidecarBanner.textContent = message;
    el.sidecarBanner.classList.remove('hidden');
  }

  /** Persistiert reine UI-Einstellungen (activeTab, lastDevice) still, ohne den "Sidecar wird neu gestartet"-Banner zu zeigen. */
  async function saveUiSetting(partial) {
    if (!currentSettings) return;
    const merged = { ...currentSettings, ...partial };
    try {
      const resp = await api.settings.set(merged);
      if (resp && resp.ok !== false) currentSettings = resp.settings;
    } catch (err) {
      // nicht kritisch - reine UI-Persistenz, der nächste erfolgreiche
      // settings:set-Aufruf holt den Zustand wieder ein
    }
  }

  [
    ...el.fpsField.querySelectorAll('input[name="fps"]'),
    el.maxHeight,
    el.monitorSelect,
    el.showCursor,
    el.rememberLastDevice,
    el.autoConnect,
    el.startMinimized,
    el.trayIcon,
    el.debugLogging,
  ].forEach((input) => {
    input.addEventListener('change', onSettingsChanged);
  });

  // Preset-Auswahl: setzt Bitrate/GOP/Ziellatenz/Audio-Puffer gemeinsam per
  // Direktzuweisung (siehe applyPresetToForm - löst bewusst KEINE eigenen
  // change-Events aus) und persistiert danach in genau einem settings:set-
  // Aufruf, statt vier separate Neustart-Zyklen anzustoßen.
  el.presetSelect.addEventListener('change', async () => {
    const preset = el.presetSelect.value;
    if (preset !== 'custom' && QUALITY_PRESETS[preset]) {
      applyPresetToForm(QUALITY_PRESETS[preset]);
    }
    await onSettingsChanged();
  });

  // Expertenmodus: schaltet die Latenz&Encoder-Felder bedienbar/gesperrt.
  el.expertMode.addEventListener('change', async () => {
    setLatencyGroupDisabled(!el.expertMode.checked);
    await onSettingsChanged();
  });

  // Verstellt der Nutzer im Expertenmodus einen der preset-gesteuerten
  // Einzelwerte manuell, springt das Preset auf "Benutzerdefiniert" - die
  // Preset-Auswahl selbst wird dabei nur im DOM aktualisiert (kein eigenes
  // change-Event), damit weiterhin nur ein settings:set pro Nutzeraktion
  // ausgelöst wird.
  LATENCY_ENCODER_INPUTS.forEach((input) => {
    input.addEventListener('change', async () => {
      if (el.presetSelect.value !== 'custom') {
        el.presetSelect.value = 'custom';
      }
      await onSettingsChanged();
    });
  });

  el.btnOpenLog.addEventListener('click', async () => {
    try {
      const resp = await api.logs.open();
      if (!resp || resp.ok === false) {
        showError((resp && resp.error) || 'Log-Verzeichnis konnte nicht geöffnet werden.');
      }
    } catch (err) {
      showError(`Log-Verzeichnis konnte nicht geöffnet werden: ${err.message}`);
    }
  });

  // Eigener Handler statt Teil der generischen Liste oben: getDisplayMedia
  // braucht eine aktive Nutzergeste, die durch den vorgeschalteten await
  // von api.settings.set() verloren ginge. Schlägt die Aufnahme fehl,
  // wird der Schalter zurückgesetzt und die Einstellung NICHT übernommen
  // (der Sidecar bleibt unangetastet).
  el.audioToggle.addEventListener('change', async () => {
    const enabling = el.audioToggle.checked;
    if (enabling) {
      const ok = await startAudioCapture();
      if (!ok) {
        el.audioToggle.checked = false;
        return;
      }
    } else {
      stopAudioCapture();
    }
    await onSettingsChanged();
  });

  // -------------------------------------------------------------------
  // Monitore
  // -------------------------------------------------------------------

  function renderDisplays() {
    const previousValue = el.monitorSelect.value;
    el.monitorSelect.innerHTML = '';
    displays.forEach((d) => {
      const opt = document.createElement('option');
      opt.value = String(d.index);
      opt.textContent = `${d.label} — ${d.width}×${d.height}${d.isPrimary ? ' (Hauptmonitor)' : ''}`;
      el.monitorSelect.appendChild(opt);
    });
    if (currentSettings) {
      el.monitorSelect.value = String(currentSettings.outputIndex);
    } else if (previousValue) {
      el.monitorSelect.value = previousValue;
    }
  }

  // -------------------------------------------------------------------
  // Manuelle Verbindung
  // -------------------------------------------------------------------

  el.manualForm.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const ip = el.manualIp.value.trim();
    const port = Number(el.manualPort.value) || 7000;
    if (!ip) {
      showError('Bitte eine IP-Adresse eingeben.');
      return;
    }
    selectTarget({ name: ip, model: 'Manuell', ip, port });
    renderDevices();
  });

  // -------------------------------------------------------------------
  // Buttons
  // -------------------------------------------------------------------

  el.btnDiscover.addEventListener('click', discoverDevices);
  el.btnStart.addEventListener('click', () => startMirroring(''));
  el.btnStop.addEventListener('click', stopMirroring);

  // -------------------------------------------------------------------
  // Events vom Main-Prozess
  // -------------------------------------------------------------------

  function subscribeEvents() {
    // Passiver Poll (1s während gestreamt wird, sonst 2s) - aktualisiert
    // bewusst NUR die Verbindungsanzeige und den Statistik-Cache, siehe
    // applyConnectionState.
    unsubscribers.push(api.on('mirror:status', (resp) => applyConnectionState(resp)));
    unsubscribers.push(
      api.on('display:list', (list) => {
        displays = list || [];
        renderDisplays();
      })
    );
    unsubscribers.push(
      api.on('sidecar:error', (payload) => {
        showSidecarNotice(payload && payload.message ? payload.message : 'Unbekannter Sidecar-Fehler.');
      })
    );
    unsubscribers.push(api.on('audio:status', (payload) => updateAudioPill(payload)));
    unsubscribers.push(
      api.on('device:updated', (resp) => {
        // Billiger Hintergrund-Refresh (nutzt den Geräte-Cache, löst KEINEN
        // neuen Scan aus). Läuft gerade eine manuelle Suche, überschreiben
        // wir deren Ladezustand nicht; schlägt der Hintergrund-Refresh
        // fehl, bleibt die Liste einfach unverändert (kein Fehlerbanner
        // für einen stillen Hintergrundvorgang).
        if (discovering) return;
        if (!resp || resp.ok === false) return;
        devices = resp.devices || [];
        renderDevices();
      })
    );
  }

  window.addEventListener('beforeunload', () => {
    unsubscribers.forEach((unsub) => unsub());
    unsubscribers = [];
  });

  // -------------------------------------------------------------------
  // Initialisierung
  // -------------------------------------------------------------------

  async function init() {
    subscribeEvents();

    try {
      const settings = await api.settings.get();
      fillSettingsForm(settings);
    } catch (err) {
      showError(`Einstellungen konnten nicht geladen werden: ${err.message}`);
    }

    try {
      // Pull-Fallback für den Audio-Bridge-Zustand: der 'audio:status'-Push
      // vom Main-Prozess kann verloren gehen, falls die Verbindung zu 7655
      // (ausgelöst durch einen bereits beim Start persistierten
      // audioEnabled:true) schon steht, bevor subscribeEvents() oben
      // fertig registriert ist. Ohne diesen aktiven Abruf würde die Pille
      // dauerhaft "aus" anzeigen, obwohl im Hintergrund längst Ton fließt.
      const audioStatus = await api.audio.status();
      updateAudioPill(audioStatus);
    } catch (err) {
      /* nicht kritisch - der nächste Push aktualisiert die Anzeige */
    }

    if (currentSettings && currentSettings.audioEnabled) {
      // Persistierter Zustand "Ton übertragen" war beim letzten Beenden
      // aktiv. getDisplayMedia verlangt laut Spezifikation eine
      // Nutzergeste; in Tests startete der automatische Wiederanlauf über
      // den Electron-eigenen setDisplayMediaRequestHandler aber auch ohne
      // vorherige Geste zuverlässig (kein Berechtigungsdialog, den Chromium
      // sonst an eine Geste koppelt). Der Fallback bleibt trotzdem als
      // Sicherheitsnetz bestehen, falls sich das in einer anderen
      // Electron/Chromium-Version anders verhält.
      const ok = await startAudioCapture();
      if (!ok) {
        el.audioToggle.checked = false;
        try {
          await onSettingsChanged();
        } catch (err) {
          /* onSettingsChanged zeigt Fehler bereits selbst an */
        }
        showSidecarNotice(
          'Ton übertragen konnte beim Start nicht automatisch fortgesetzt werden (keine Nutzergeste) — bitte den Schalter erneut betätigen.'
        );
      }
    }

    try {
      displays = await api.displays.list();
      renderDisplays();
    } catch (err) {
      showError(`Bildschirme konnten nicht ermittelt werden: ${err.message}`);
    }

    try {
      const pendingError = await api.sidecar.lastError();
      if (pendingError) {
        showSidecarNotice(pendingError);
      }
    } catch (err) {
      /* nicht kritisch */
    }

    await discoverDevices();

    // Automatisch mit dem zuletzt verbundenen Gerät verbinden, sofern
    // sowohl "letztes Gerät merken" als auch "Autostart" aktiv sind und ein
    // Gerät gemerkt wurde. Läuft NACH discoverDevices(), damit ein evtl.
    // frischer discover-Fehler nicht durch den Connect-Versuch überschrieben
    // wird, aber unabhängig davon, ob das Gerät in der Liste auftaucht (die
    // manuelle IP-Eingabe funktioniert schließlich auch ohne Listeneintrag).
    if (
      currentSettings &&
      currentSettings.rememberLastDevice &&
      currentSettings.autoConnect &&
      currentSettings.lastDevice &&
      !selectedTarget
    ) {
      const d = currentSettings.lastDevice;
      selectTarget({ name: d.name || d.ip, model: '', ip: d.ip, port: d.port || 7000 });
      renderDevices();
      startMirroring('');
    }

    try {
      const status = await api.mirror.status();
      applyConnectionState(status);
    } catch (err) {
      /* Statuspolling übernimmt spätestens im nächsten Tick */
    }
  }

  document.addEventListener('DOMContentLoaded', init);
})();
