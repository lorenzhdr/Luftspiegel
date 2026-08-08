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
  };

  // -------------------------------------------------------------------
  // Hilfsfunktionen: Anzeige
  // -------------------------------------------------------------------

  /**
   * Verbindungsfehler bleiben stehen, bis der Nutzer erneut etwas auslöst
   * (Start/Suchen ruft showError(null) selbst zu Beginn auf) oder sie
   * über den ×-Button wegklickt. Der passive 2-Sekunden-Statuspoll darf
   * diese Anzeige NICHT eigenständig setzen oder löschen (siehe
   * applyConnectionState, das den Banner absichtlich nicht anfasst).
   */
  function showError(message) {
    if (!message) {
      el.errorBanner.classList.add('hidden');
      el.errorBannerText.textContent = '';
      return;
    }
    el.errorBannerText.textContent = message;
    el.errorBanner.classList.remove('hidden');
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
   * passive 2-Sekunden-Statuspoll (mirror:status) keine überraschenden
   * Seiteneffekte auslöst (z.B. einen gerade angezeigten Verbindungsfehler
   * nach 2s wieder verschwinden lassen, oder unaufgefordert den PIN-Dialog
   * öffnen).
   */
  function applyConnectionState(resp) {
    const { bucket, label } = classifyStatus(resp);
    el.connPill.dataset.state = bucket;
    el.connLabel.textContent = label;

    lastKnownConnected = bucket === 'connected' || bucket === 'connecting';
    el.btnStop.disabled = !lastKnownConnected;
    el.btnStart.disabled = lastKnownConnected || !selectedTarget;
  }

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
  // Einstellungen
  // -------------------------------------------------------------------

  function fillSettingsForm(settings) {
    currentSettings = settings;
    const radios = el.fpsField.querySelectorAll('input[name="fps"]');
    radios.forEach((r) => {
      r.checked = Number(r.value) === Number(settings.fps);
    });
    el.maxHeight.value = String(settings.maxHeight);
    el.bitrate.value = String(settings.bitrate);
    if (el.monitorSelect.options.length) {
      el.monitorSelect.value = String(settings.outputIndex);
    }
  }

  function readSettingsFromForm() {
    const checked = el.fpsField.querySelector('input[name="fps"]:checked');
    return {
      fps: checked ? Number(checked.value) : 30,
      maxHeight: Number(el.maxHeight.value),
      bitrate: Number(el.bitrate.value) || 0,
      outputIndex: Number(el.monitorSelect.value) || 0,
    };
  }

  async function onSettingsChanged() {
    const newSettings = readSettingsFromForm();
    showSidecarNotice('Einstellungen werden übernommen — Sidecar wird ggf. neu gestartet…');
    try {
      const resp = await api.settings.set(newSettings);
      if (!resp || resp.ok === false) {
        showSidecarNotice(null);
        showError((resp && resp.error) || 'Einstellungen konnten nicht übernommen werden.');
        return;
      }
      currentSettings = resp.settings;
      if (resp.restarted) {
        showSidecarNotice('Einstellungen übernommen, Sidecar wurde neu gestartet.');
        setTimeout(() => showSidecarNotice(null), 4000);
      } else if (resp.note) {
        showSidecarNotice(resp.note);
      } else {
        showSidecarNotice(null);
      }
    } catch (err) {
      showSidecarNotice(null);
      showError(`Einstellungen konnten nicht übernommen werden: ${err.message}`);
    }
  }

  [
    ...el.fpsField.querySelectorAll('input[name="fps"]'),
    el.maxHeight,
    el.bitrate,
    el.monitorSelect,
  ].forEach((input) => {
    input.addEventListener('change', onSettingsChanged);
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
    // Passiver Poll (alle ~2s) - aktualisiert bewusst NUR die
    // Verbindungsanzeige, siehe applyConnectionState.
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

    try {
      const status = await api.mirror.status();
      applyConnectionState(status);
    } catch (err) {
      /* Statuspolling übernimmt spätestens in 2s */
    }
  }

  document.addEventListener('DOMContentLoaded', init);
})();
