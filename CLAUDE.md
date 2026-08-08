# CLAUDE.md — Luftspiegel

## 1. Was das ist

Luftspiegel spiegelt den Bildschirm eines Windows-Laptops per **AirPlay** auf ein Apple TV — Cast **vom
Laptop aus**, ohne dass auf dem Apple TV etwas installiert werden muss. Eine Alternative zu AirParrot.

Das Projekt ist ein **Fork von [`omarroth/doubletake`](https://github.com/omarroth/doubletake)** (Go,
LGPL-3.0-or-later), Modulname im Code weiterhin `doubletake` (siehe `go.mod`, alle Import-Pfade
`doubletake/internal/...`). Remotes:

- `origin` → `https://github.com/lorenzhdr/Luftspiegel.git`
- `upstream` → `https://github.com/omarroth/doubletake.git`

Aktiver Branch: `windows-port`. Upstream ist **Linux-only** (GStreamer, Wayland/X11, PulseAudio). Dieser
Fork fügt hinzu:

- ein Windows-Capture-Backend auf Basis von **ffmpeg** (ddagrab + h264_mf) statt GStreamer,
- einen **TCP-Control-Channel** für Windows (statt Unix-Domain-Socket),
- **Windows-Systemaudio** über einen TCP-PCM-Kanal zwischen Go-Daemon und Electron-GUI,
- eine **Electron-GUI** (`gui/`) als Bedienoberfläche für den Go-Daemon.

Die eigentliche AirPlay-Protokollschicht (FairPlay, Pairing, RTSP, `internal/airplay/mirror.go`,
`client.go`, `fairplay*.go`) stammt unverändert von upstream und wurde für den Windows-Port **nicht**
angefasst — nur die plattformabhängigen Ränder (Capture, Audio-Quelle, Control-Transport) wurden ergänzt.
`README.md` ist weiterhin die unveränderte Upstream-Doku (Englisch, Linux-fokussiert) — sie hier nicht
ersetzen oder überschreiben.

**Tech-Stack:** Go 1.25 (`go.mod`) für Daemon/CLI, Electron 42 + Vanilla-JS (kein Build-Step) für die GUI,
ffmpeg (native arm64-Build) als externer Capture-/Encode-Prozess auf Windows.

## 2. Befehle

### Go-Daemon/CLI bauen

```powershell
$env:PATH = "C:\Program Files\Go\bin;$env:PATH"   # Go liegt NICHT in der Standard-PATH
$env:CGO_ENABLED = "0"
go build -o bin/doubletake.exe ./cmd/doubletake
go build -o bin/doubletake-ctl.exe ./cmd/doubletake-ctl
```

`make` (aus dem Upstream-Makefile) funktioniert nur unter einer POSIX-Shell und baut nach `bin/doubletake`
bzw. `bin/doubletake-ctl` (ohne `.exe`-Endung im Zielnamen; Go hängt sie unter Windows selbst an). Für den
Windows-Port relevante Targets:

| Target | Zweck |
|---|---|
| `make doubletake` / `make doubletake-ctl` | Debug-Build nach `bin/` |
| `make test` | `go test ./...` |
| `make clean` | `bin/` löschen, Testcache leeren |

**Wichtig:** Die GUI erwartet die Sidecar-Binaries unter den Namen `bin/luftspiegel.exe` und
`bin/ffmpeg.exe`/`bin/ffprobe.exe` (siehe `gui/main.js:resolveSidecarPath` und
`gui/package.json:extraResources`), nicht `doubletake.exe`. Das Makefile baut aber `doubletake.exe`/
`doubletake-ctl.exe`. Es gibt **keinen automatisierten Rename-Schritt** — im lokalen `bin/`-Ordner liegen
`luftspiegel.exe` und `luftspiegel-ctl.exe` bereits umbenannt/kopiert, aber wie dieser Schritt reproduzierbar
ausgeführt wird ist nicht im Repo dokumentiert (offener Punkt, siehe Abschnitt 5).

**Eine laufende GUI sperrt `bin\luftspiegel.exe`** (Windows hält die Datei des laufenden Prozesses fest) —
vor jedem `go build`, der diese Datei überschreiben soll, erst die GUI/den Sidecar-Prozess beenden, sonst
schlägt der Build mit einem Zugriffsfehler fehl.

### CLI direkt starten (ohne Daemon/GUI)

```powershell
bin\doubletake.exe -target 192.168.178.125 -fps 30 -max-height 1080
```

Relevante Flags (aus `cmd/doubletake/main.go`, nicht abschließend — bei Änderungen dort nachsehen):

| Flag | Bedeutung |
|---|---|
| `-target` | Apple-TV-IP/Hostname, überspringt Discovery |
| `-port` | AirPlay-Port (Default 7000) |
| `-code` / `$DOUBLETAKE_CODE` | Pairing-PIN bzw. Passwort; Env-Var bevorzugt (nicht in Shell-History/`ps`) |
| `-pair` | Erzwingt Neu-Pairing |
| `-fps` | 30 (Default) |
| `-bitrate` | kbps, 0 = automatisch (Windows: 1800–12000, Default 4500, siehe `windowsBitrateKbps`) |
| `-hwaccel` | `auto`\|`nvenc`\|`vaapi`\|`openh264`\|`none`; unter Windows sind nur `auto` (h264_mf) und `none` (libx264) tatsächlich verfügbar, die Linux-Encoder-Namen werden mit Fehlermeldung abgelehnt |
| `-no-audio` | Audio deaktivieren |
| `-audio-tcp` | Windows: lokaler TCP-Port für PCM-Zulieferung durch die GUI (Default `7655`) |
| `-daemonize` | Als Hintergrunddienst mit Control-Channel starten |
| `-socket` | Control-Channel-Adresse (Windows: `host:port` oder nackter Port, Default `127.0.0.1:7654`) |
| `-max-height` | Windows: Downscale-Obergrenze in Pixeln (Höhe), 0 = native Auflösung |
| `-output-index` | Windows: 0-basierter `ddagrab`-Monitorindex |
| `-stub-file` | Annex-B-`.h264`-Datei statt echtem Capture abspielen (Pipeline-Test ohne Display) |
| `-test` | Synthetisches Testbild/-ton statt echter Capture |
| `-debug` | Verbose Logging |

### Daemon + `doubletake-ctl`

```powershell
bin\doubletake.exe -daemonize -audio-tcp 7655
bin\doubletake-ctl.exe status
bin\doubletake-ctl.exe discover
bin\doubletake-ctl.exe connect 192.168.178.125
bin\doubletake-ctl.exe disconnect
```

`doubletake-ctl`-Kommandos (aus `cmd/doubletake-ctl/main.go`): `status`, `discover`, `devices`, `connect
[target] [PIN]`, `pin <PIN>`, `disconnect [target]`, `mute [target]`, `unmute [target]`.

### Electron-GUI

```powershell
cd gui
npm install
npm start                              # Dev-Start (electron .)
npm run build                          # electron-builder --win --arm64 (NSIS-Installer)
```

Die GUI startet den Daemon-Sidecar-Prozess selbst (`spawn`, Args u. a. `-daemonize -fps -output-index
-max-height`, siehe `gui/main.js:buildSidecarArgs`) oder **adoptiert** einen bereits laufenden Daemon auf
Port 7654 (z. B. manuell gestartet, oder `gui/tools/fake-daemon.js` für UI-Tests ohne echten Go-Prozess) —
in dem Fall verwaltet sie dessen Lebenszyklus nicht (kein Kill, kein automatischer Neustart bei
Einstellungsänderung).

### Tests

```powershell
go test ./...          # Go-Unit-/Integrationstests (internal/airplay, internal/daemon)
```

Kein JS-Test-Runner für die GUI im Repo (`gui/package.json` hat keine `test`-Skripte).

### Testwerkzeuge (ohne echtes Apple TV / echte GUI)

- `go run ./tools/pcmtest [-addr 127.0.0.1:7655] [-freq 440] [-duration 30s]` — sendet einen Sinuston als
  PCM (s16le, 44100 Hz, stereo) an den Audio-TCP-Port, um `audio_windows.go` ohne Electron zu testen.
- `node gui/tools/fake-daemon.js` — simuliert den Go-Daemon-Control-Channel für GUI-Entwicklung ohne
  echten Sidecar.
- `node gui/tools/pcm-sink.js` — Gegenstück zu `pcmtest`, nimmt PCM vom Audio-Port entgegen.
- `-stub-file` (CLI-Flag) — spielt eine vorab aufgenommene Annex-B-`.h264`-Datei ab, um die
  Mirror-/Streaming-Pipeline ohne Capture-Backend zu testen.

## 3. Geräte-Kontext (dieser Rechner)

- Windows 11 ARM64, Snapdragon X Elite X1E78100, Adreno X1-85 GPU.
- Go 1.26.5 (native arm64) — **nicht** in der Standard-PATH, vor jedem Build:
  `$env:PATH = "C:\Program Files\Go\bin;$env:PATH"`, dazu `CGO_ENABLED=0` setzen (der Fork ist bewusst
  cgo-frei gehalten, siehe Kommentar zum linearen Resampler in `audio_windows.go`).
- Getestetes Apple TV im lokalen Testnetz: `192.168.178.125`, Name `h264zyyx`, Modell **AppleTV11,1**
  (Apple TV 4K, 2021). Pairing-Credentials liegen unter `C:\Users\loren\.config\doubletake\credentials.json`
  (`airplay.DefaultCredentialsPath()`, folgt `$XDG_CONFIG_HOME` bzw. `$HOME/.config/doubletake/`). Pairing
  ist einmalig; danach genügt Pair-Verify ohne erneute PIN-Eingabe.
- **Native ARM64-ffmpeg ist Pflicht**, nicht die x64-Emulation. Quelle:
  `https://github.com/tordona/ffmpeg-win-arm64` — dieser Build enthält `ddagrab` (DXGI Desktop
  Duplication) und `h264_mf` (Media-Foundation-Hardwareencoding), Standard-ffmpeg-Builds i. d. R. nicht.
  Liegt lokal unter `bin/ffmpeg.exe` / `bin/ffprobe.exe` (per `.gitignore` von Git ausgeschlossen, muss
  manuell besorgt werden). Gemessen: **~3,3 % CPU von 12 Kernen** bei 1080p30 mit der nativen
  ARM64-Variante gegenüber ~21 % mit einer emulierten x64-ffmpeg.
- **Ein dauerhaft aktives NordVPN zerstört mDNS-Discovery**: `dns-sd` findet nichts, obwohl Bonjour läuft,
  weil UDP 5353 bereits von `mDNSResponder` und Spotify belegt ist. Deshalb existiert der Subnetz-Fallback
  in `internal/airplay/discovery_fallback.go` (`DiscoverAirPlayDevicesFallback`): scannt alle lokalen
  privaten `/24`-Subnetze (nie größer) auf TCP 7000, überspringt virtuelle/VPN-Adapter anhand des
  Namensmusters (u. a. `nordlynx`, `openvpn`, `tailscale`, `wsl`, `docker`) und ist auf `fallbackBudget = 5s`
  gedeckelt. Der GUI-Kommentar zu `DISCOVER_TIMEOUT_MS` beziffert den realen Scan (mDNS-Timeout plus
  Fallback) mit „bis zu ~5 s“, praktisch gemessen rund 4,5 s.
- **Eine laufende GUI sperrt `bin\luftspiegel.exe`** unter Windows (Datei des laufenden Prozesses ist
  exklusiv geöffnet) — vor `go build`, das diese Datei überschreiben würde, erst den Sidecar/die GUI
  beenden.

### Die drei gemessenen ffmpeg-Fallen (Design-Begründung für `capture_windows.go`)

Diese drei Punkte sind laut Code-Kommentar **durch Messung**, nicht durch Dokumentation gefunden worden und
sind essenziell für die Filterkette in `StartCapture`:

1. **`format=nv12` muss der letzte Filterschritt sein.** `h264_mf` bewirbt `yuv420p` als akzeptiertes
   Format, stirbt beim tatsächlichen Encode damit aber mit „Generic error in an external library“.
2. **`fps=<fps>` im Filtergraph plus `-fps_mode cfr`** sind beide nötig — ohne beides flutet `ddagrab` den
   Muxer mit „non monotonically increasing dts“.
3. **`-rate_control ld_vbr`/`gld_vbr` lehnt der Qualcomm-MFT mit `E_INVALIDARG` ab.** Stattdessen wird
   `-scenario display_remoting` verwendet (Alternative laut Kommentar: `-rate_control cbr -scenario
   live_streaming`).

Zusätzlich: `gdigrab` ist bei **23 fps hart gedeckelt**, nur `ddagrab` (DXGI Desktop Duplication) liefert
volle Framerate. Der Desktop meldet sich logisch mit einer DPI-skalierten Auflösung, `ddagrab` liefert die
physische Pixelauflösung des Panels — deutlich größer. `-max-height` (Default in der GUI: `1080`) wird
empfohlen, um Bitrate und Latenz in einem vernünftigen Rahmen zu halten; die Skalierung passiert über
`windowsScaleFilter` (`scale=-2:trunc(min(ih,maxHeight)/2)*2`, das Komma in `min(...)` muss im
ffmpeg-Filtergraph escaped werden, siehe Kommentar dort).

### Zwei plattformspezifische Stolpersteine

- `errors.Is(err, syscall.EADDRINUSE)` greift auf Windows **nie**. Winsock meldet den Adressenkonflikt als
  Errno `10048` (`WSAEADDRINUSE`), das ist ein anderer Wertebereich als Gos POSIX-Errno-Emulation. Die
  Prüfung erfolgt in `internal/daemon/control_windows.go` über `isAddrInUse` mit dem expliziten
  `wsaEADDRINUSE = 10048`-Vergleich.
- **Die Audio-Samplerate ist 44100 Hz, nicht 48000.** `mirror.go` meldet `"sr": 44100` im SDP, alle
  Latenzberechnungen setzen diese Rate voraus. Bei 48 kHz liefe der Ton ca. 8,8 % zu schnell. Erwartete
  Bandbreite am PCM-TCP-Socket: **176400 Byte/s** (44100 × 2 Kanäle × 2 Byte) — der günstigste Weg, eine
  falsche Rate, Kanalzahl oder Bittiefe zu erkennen. Sowohl `pcmInputRate` als auch `pcmOutputRate` sind in
  `audio_windows.go` fest auf `44100` gesetzt (die Electron-GUI fordert diese Rate direkt von ihrer
  `AudioContext` an); der eingebaute `linearResampler` ist bei gleichen Raten ein reines Durchreichen und
  bleibt nur für einen künftigen Sender vorbereitet, der nur 48 kHz liefern könnte. **Achtung:** Die
  Flag-Hilfe von `-audio-tcp` in `cmd/doubletake/main.go` beschreibt das Wire-Format noch fälschlich als
  „48000 Hz“ — das ist ein veralteter Kommentar, der tatsächliche Vertrag (Code + Wire) ist 44100 Hz.

## 4. Architektur

```
Electron-GUI (gui/)                    Go-Daemon/CLI (cmd/, internal/)
┌─────────────────────────┐            ┌───────────────────────────────────┐
│ renderer/ (UI, kein     │  TCP JSON  │ internal/daemon: Control-Channel   │
│ Build-Step, Vanilla JS) │◄──────────►│  (Windows: TCP 127.0.0.1:7654,     │
│                         │  :7654     │   Unix: Domain-Socket)             │
│ main.js (Main-Prozess): │            │  verwaltet activeStream(s),        │
│  - spawnt/adoptiert     │  spawn     │  Discovery-Cache, Pairing-Status   │
│    Sidecar-Prozess      │───────────►│                                    │
│  - Audio-Bridge (TCP    │  raw PCM   │ internal/airplay:                  │
│    Client) → schickt    │───────────►│  StartAudioCapture (Windows: TCP-  │
│    WASAPI-Loopback-PCM  │  :7655     │  Listener) / GStreamer (Linux)     │
│    vom AudioWorklet     │            │  StartCapture (Windows: ffmpeg     │
│                         │            │  ddagrab→h264_mf) / GStreamer      │
└─────────────────────────┘            │  mirror.go, client.go, fairplay*.go│
                                        │  (AirPlay-Protokoll, unverändert)  │
                                        └──────────────┬──────────────────┘
                                                        │ RTSP/HTTP + verschlüsselter
                                                        │ H.264/ALAC-Stream
                                                        ▼
                                                  Apple TV (AirPlay-Empfänger)
```

- **`internal/airplay/`** — Protokoll- und Capture-Schicht.
  - `mirror.go`, `client.go`, `pairing.go`, `fairplay*.go`, `digest.go`, `latency.go` — AirPlay-Protokoll
    (RTSP/HTTP, FairPlay SAP, SRP-6a-Pairing, ChaCha20-Poly1305), von upstream übernommen und im Fork nicht
    verändert.
  - `capture.go` — plattformunabhängiger Teil (`CaptureConfig`, `ScreenCapture`, Bitrate-Grenzen,
    `ValidateHWAccel`). Per Go-Build-Tags aufgeteilt in:
    - `capture_linux.go` (`//go:build linux`) — GStreamer-Pipeline (Wayland-Portal/X11), unverändert.
    - `capture_windows.go` (`//go:build windows`) — ffmpeg-basiertes Backend, neu im Fork.
    - `capture_stub.go` — `-stub-file`-Replay-Backend, plattformunabhängig.
  - `audio.go` — gemeinsamer Teil (`AudioCapture`, `DrainStale`, `DefaultAudioTCPPort = 7655`), aufgeteilt
    analog zu `capture.go`:
    - `audio_linux.go` (`//go:build linux`) — GStreamer/PulseAudio, unverändert.
    - `audio_windows.go` (`//go:build windows`) — TCP-PCM-Listener (`tcpPCMSource`) + Sinuston-Testquelle
      (`sineWaveSource`) + `linearResampler`, neu im Fork.
  - `discovery.go` — mDNS-Discovery (unverändert), `discovery_fallback.go` — Subnetz-Fallback-Scan (neu im
    Fork, s. Abschnitt 3).
  - `credentials.go`, `credentials_keyring.go` — Pairing-Credential-Persistenz (Datei oder System-Keyring).
- **`internal/daemon/`** — Daemon-Zustandsmaschine (`daemon.go`, plattformunabhängig) und
  Control-Channel-Transport, per Build-Tag aufgeteilt:
  - `control_unix.go` (`//go:build !windows`) — Unix-Domain-Socket, unverändert.
  - `control_windows.go` (`//go:build windows`) — TCP `127.0.0.1:7654`, neu im Fork; enthält den
    `WSAEADDRINUSE`-Workaround.
  - `daemonclient/client.go` — Client-Bibliothek für `doubletake-ctl` und die GUI (Protokoll: eine
    JSON-Zeile pro Request/Response über den Control-Channel, s. `gui/main.js:sendControlCommand`).
- **`cmd/doubletake/`** — CLI-Entry-Point (direkter Modus oder `-daemonize`).
- **`cmd/doubletake-ctl/`** — schlanker Control-Channel-Client für Skripting/Automatisierung.
- **`gui/`** — Electron-App, spricht den Go-Daemon ausschließlich über den Control-Channel (TCP-JSON,
  Port 7654) und den Audio-TCP-Kanal (Port 7655) an; kein direkter Import von Go-Code.
  - `main.js` — Electron-Main-Prozess: Sidecar-Prozessverwaltung (spawnen/adoptieren/sauber stoppen),
    Control-Channel-Client, Audio-Bridge (TCP-Client zu Port 7655, gespeist von einem AudioWorklet im
    Renderer), Settings-Persistenz (`userData/settings.json`), IPC-Handler.
  - `preload.js` — Context-Bridge zwischen Main und Renderer (contextIsolation aktiv, kein
    `nodeIntegration`).
  - `renderer/` — UI (HTML/CSS/JS ohne Build-Step), `pcm-worklet.js` — AudioWorklet, das
    WASAPI-Loopback-Samples (über `getDisplayMedia({ audio: 'loopback' })`) in PCM-Chunks an den Main-
    Prozess weiterreicht.
  - `tools/fake-daemon.js`, `tools/pcm-sink.js` — Testwerkzeuge, s. Abschnitt 2.
- **`tools/pcmtest/`** — Go-Testwerkzeug für den Audio-TCP-Kanal, s. Abschnitt 2.
- **`man/`, `plasmoid/`** — Upstream-Linux-Artefakte (Manpages, KDE-Plasma-Widget), im Fork unverändert und
  für den Windows-Pfad nicht relevant.

### Datenfluss beim Mirroring (Windows)

1. GUI startet/adoptiert `luftspiegel.exe -daemonize` (Control-Channel TCP 7654).
2. GUI ruft `discover`/`connect <IP>` über den Control-Channel auf; der Daemon übernimmt Pairing (gespeicherte
   Credentials oder PIN-Flow) und FairPlay-Setup über `internal/airplay`.
3. Daemon startet `ffmpeg` (ddagrab→h264_mf) als Kindprozess, liest H.264-Annex-B von dessen stdout und
   speist es in `mirror.go`s Streaming-Pfad zum Apple TV.
4. Falls Audio aktiv: Daemon öffnet einen TCP-Listener auf Port 7655; die GUI verbindet sich als Client und
   schickt kontinuierlich rohes PCM (44100 Hz, s16le, stereo) aus dem Renderer-AudioWorklet. Der Daemon
   puffert bis max. 300 ms, füllt bei Unterlauf mit Stille auf (`readWaitTimeout = 12 ms`) und kodiert nach
   ALAC für den AirPlay-Audiostream.
5. Beenden läuft **immer** über `disconnect` auf dem Control-Channel (s. u.), nie über Kill des
   Daemon-Prozesses während eine Session aktiv ist.

### Die wichtigste Betriebsregel

**Mirroring wird immer über `disconnect` auf dem Control-Channel beendet, niemals durch Abschießen des
Prozesses.** AirPlay braucht ein sauberes RTSP-`TEARDOWN`; ein Kill lässt den Apple TV die Session halten und
einen erneuten Verbindungsversuch **verweigern**. `gui/main.js:stopSidecarClean` setzt das um: erst
`disconnect` über den Control-Channel senden (mit Timeout, Fehler dabei wird nur geloggt, nicht als fatal
behandelt), *danach* erst den Kindprozess beenden (`SIGTERM`-Äquivalent, nach 3 s Timeout `SIGKILL`) — und
das auch nur, wenn die GUI den Prozess selbst gestartet hat (nicht bei einem adoptierten Fremdprozess).
Gemessen dauert ein TEARDOWN rund 0,75 s.

### Verifizierter Stand

Gegen das echte Apple TV (`AppleTV11,1`) getestet: Pairing, FairPlay, Live-Mirroring, Reconnect,
Fenster-Schließen bei aktivem Stream, Audio.

## 5. Offene Punkte

- **Rename-Schritt Daemon-Binaries nicht automatisiert.** Die GUI erwartet `bin/luftspiegel.exe` (und
  `bin/luftspiegel-ctl.exe` für `doubletake-ctl`), der Go-Build erzeugt aber `doubletake.exe`/
  `doubletake-ctl.exe`. Lokal liegen bereits umbenannte Kopien in `bin/`, aber weder Makefile noch ein
  Skript im Repo bilden diesen Schritt nach — sollte geklärt/automatisiert werden (Makefile-Target oder
  Build-Skript).
- **`restrictOwnAudio` ungefixt.** Die App soll ihre eigenen Sounds nicht mitübertragen; die entsprechende
  Electron-API ist laut Code-Kommentar erst in Electron **v44** verfügbar, gepinnt ist aber **v42**
  (`gui/package.json`). Bewusst in Kauf genommen, bis auf eine neuere Electron-Version aktualisiert wird.
- **Installer-Build nicht abschließend abgenommen.** `npm run build` (`electron-builder --win --arm64`,
  NSIS) wurde noch nicht vollständig verifiziert.
- **Praktische Bedienabnahme der GUI durch den Nutzer steht aus** (über die Low-Level-Verifikation von
  Pairing/Mirroring/Audio hinaus).
- **Upstream hat offene Bugs**, u. a. schlechtere Latenz als bei echten Apple-Sendegeräten. Ein
  Windows-Backend wäre ein natürlicher Beitrag zurück an `omarroth/doubletake`; die
  Capture-/Audio-Aufteilung per Build-Tag ist genau als Byte-Grenze dafür gebaut (`ScreenCapture`/
  `AudioCapture` sind plattformunabhängige Contracts, siehe `capture.go`/`audio.go`).
- **Lizenz: LGPL-3.0-or-later ist zwingend** (abgeleitetes Werk von `doubletake`), eine Relizenzierung ist
  nicht erlaubt; die Herkunft (Fork von `omarroth/doubletake`) muss erkennbar bleiben. `gui/package.json`
  gab zum Zeitpunkt dieser Dokumentation im committeten Stand fälschlich `"license": "GPL-3.0"` an — im
  Arbeitsverzeichnis liegt (uncommitted) bereits eine Korrektur auf `"LGPL-3.0-or-later"` vor; sollte
  committet werden.
- **Zusätzliche Remote `origin/gpl-only`** existiert (`git branch -a`) — Zweck/Inhalt nicht aus dem Code
  ersichtlich, ggf. beim nächsten Aufräumen klären.
- **Kein automatisierter Windows-CI-Lauf.** `.github/workflows/ci.yml` baut/testet ausschließlich auf
  `ubuntu-latest` (Upstream-Workflow, unverändert) — der Windows-Port wird dadurch nicht laufend geprüft.
