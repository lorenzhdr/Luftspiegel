package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"doubletake/internal/airplay"
)

// State represents the daemon's current lifecycle state.
type State string

const (
	StateIdle        State = "idle"
	StateDiscovering State = "discovering"
	StateConnecting  State = "connecting"
	StateStreaming   State = "streaming"
	StatePINRequired State = "pin_required"
)

// Request is a command sent to the daemon over the control socket.
type Request struct {
	Cmd    string `json:"cmd"`
	Target string `json:"target,omitempty"`
	Port   int    `json:"port,omitempty"`
	Pin    string `json:"pin,omitempty"`
}

// StreamInfo describes one active (or connecting) mirror stream.
type StreamInfo struct {
	Device     string `json:"device"`
	DeviceIP   string `json:"device_ip"`
	State      State  `json:"state"`
	HasAudio   bool   `json:"has_audio"`
	AudioMuted bool   `json:"audio_muted"`
}

// Response is returned to the caller for every request.
type Response struct {
	OK         bool         `json:"ok"`
	State      State        `json:"state"`
	Device     string       `json:"device,omitempty"`
	DeviceIP   string       `json:"device_ip,omitempty"`
	HasAudio   bool         `json:"has_audio"`
	AudioMuted bool         `json:"audio_muted"`
	NeedsPIN   bool         `json:"needs_pin,omitempty"`
	Error      string       `json:"error,omitempty"`
	Devices    []DeviceInfo `json:"devices,omitempty"`
	Streams    []StreamInfo `json:"streams,omitempty"`
}

// DeviceInfo is a simplified view of a discovered AirPlay device.
type DeviceInfo struct {
	Name     string `json:"name"`
	Model    string `json:"model"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	DeviceID string `json:"device_id"`
}

// Config holds daemon configuration.
type Config struct {
	// SocketPath is the control channel address: a Unix domain socket path
	// on Unix-likes, or a TCP "host:port"/bare-port address on Windows (see
	// DefaultControlAddr and control_windows.go).
	SocketPath  string
	CredFile    string
	CredBackend string
	FPS         int
	Bitrate     int
	HWAccel     string
	Debug       bool
	TestMode    bool
	NoEncrypt   bool
	DirectKey   bool
	NoAudio     bool
	ShowCursor  bool

	// MaxHeight and OutputIndex are forwarded to airplay.CaptureConfig; see
	// its docs. Windows-only, ignored on Linux.
	MaxHeight   int
	OutputIndex int

	// AudioTCPPort is the local TCP port airplay.StartAudioCapture listens on
	// for raw PCM audio on Windows (see audio_windows.go); ignored on Linux.
	// Zero (the Config zero value) means airplay.DefaultAudioTCPPort.
	AudioTCPPort int
}

// DefaultControlAddr returns the default control channel address: a Unix
// domain socket path derived from XDG_RUNTIME_DIR on Unix-likes, or a
// loopback-only TCP address ("127.0.0.1:7654") on Windows.
func DefaultControlAddr() string {
	return defaultControlAddr()
}

// DialControl connects to a running daemon's control channel at addr, using
// the platform-appropriate transport (Unix domain socket on Unix-likes, TCP
// on Windows). It is the single dial path shared by daemonclient.
func DialControl(addr string, timeout time.Duration) (net.Conn, error) {
	return dialControl(addr, timeout)
}

// activeStream tracks the state of a single mirroring session to one receiver.
type activeStream struct {
	device     string // friendly name
	deviceIP   string
	deviceID   string
	state      State
	audioMuted bool
	session    *airplay.MirrorSession
	client     *airplay.AirPlayClient
	sink       *airplay.BroadcastSink // fan-out video sink (nil when no broadcast)
	cancelFn   context.CancelFunc
	pinCh      chan string
}

// Daemon manages a long-running doubletake service.
type Daemon struct {
	cfg            Config
	mu             sync.Mutex
	devices        []airplay.AirPlayDevice
	deviceLastSeen map[string]time.Time // keyed by IP
	credStore      *airplay.CredentialStore

	// loggedDeviceSignature is the deviceSetSignature of the last device set
	// that was actually logged (see logDiscoveryOutcomeLocked). Used so a
	// scan that simply reconfirms an already-known set of devices doesn't
	// log anything without -debug.
	loggedDeviceSignature string

	// Multi-stream state
	streams       map[string]*activeStream  // keyed by target IP
	broadcast     *airplay.BroadcastCapture // shared video fan-out; nil when no streams active
	capture       *airplay.ScreenCapture    // underlying screen capture
	captureCancel context.CancelFunc        // cancellation for shared capture context

	// captureStopExpected is set to true, under d.mu, immediately before we
	// intentionally tear down the current capture (disconnect/shutdown), and
	// back to false when a fresh capture is published. The capture's Run()
	// goroutine consults it (also under d.mu) after Run() returns to decide
	// whether the resulting "closed pipe"-style error is an expected side
	// effect of our own teardown or a genuine, unexpected capture failure.
	//
	// This is deliberately a flag guarded by d.mu rather than a check against
	// the capture's context: setting the flag happens inside the very same
	// mutex-protected call that goes on to cancel the context and stop the
	// capture, and the goroutine can only read the flag by acquiring that
	// same mutex — so whichever of "flag set" vs. "Run() observes the stop"
	// happens first in wall-clock time, the reader is guaranteed (by mutual
	// exclusion, not by timing) to see the flag already set once it gets the
	// lock. A context.Err() check does not have that guarantee: nothing
	// forces the goroutine's read of ctx.Err() to happen after the cancel
	// has become visible relative to the unrelated pipe-close that actually
	// wakes Run() up.
	captureStopExpected bool

	// PIN-waiting state (at most one device waits for a PIN at a time)
	pendingTarget string

	discoverCancel context.CancelFunc
	listener       net.Listener

	// runCtx is the context passed to Run, used to bound and cancel
	// on-demand discovery scans triggered by the "discover" command. Nil
	// until Run has been called (e.g. in unit tests that drive handlers
	// directly), in which case on-demand scans fall back to
	// context.Background().
	runCtx context.Context

	// fallbackScan is non-nil while a TCP subnet fallback scan (the
	// expensive /24 sweep) is in flight. This is the one piece of discovery
	// work that genuinely must not run twice concurrently, so it alone gets
	// a single-flight gate — see runFallbackScan. mDNS browsing is cheap
	// enough that concurrent callers each just run their own; there's
	// nothing to coalesce there.
	fallbackScan *fallbackScanState
}

// fallbackScanState is the shared result of one in-flight (or just
// completed) TCP subnet fallback scan; see Daemon.fallbackScan and
// Daemon.runFallbackScan.
type fallbackScanState struct {
	wg    sync.WaitGroup
	found []airplay.AirPlayDevice
	err   error
}

// New creates a new Daemon with the given configuration.
func New(cfg Config) (*Daemon, error) {
	if err := airplay.ValidateHWAccel(cfg.HWAccel); err != nil {
		return nil, fmt.Errorf("hwaccel: %w", err)
	}
	if cfg.HWAccel == "" {
		cfg.HWAccel = "auto"
	}
	var cs *airplay.CredentialStore
	switch cfg.CredBackend {
	case "keyring":
		kb, err := airplay.NewKeyringBackend()
		if err != nil {
			return nil, fmt.Errorf("keyring backend: %w", err)
		}
		cs = airplay.NewCredentialStoreWithBackend(kb)
	default:
		credPath := cfg.CredFile
		if credPath == "" {
			credPath = airplay.DefaultCredentialsPath()
		}
		var err error
		cs, err = airplay.NewCredentialStore(credPath)
		if err != nil {
			return nil, fmt.Errorf("load credentials: %w", err)
		}
	}

	return &Daemon{
		cfg:            cfg,
		deviceLastSeen: make(map[string]time.Time),
		streams:        make(map[string]*activeStream),
		credStore:      cs,
	}, nil
}

// Run starts the daemon control socket and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	airplay.DebugMode = d.cfg.Debug

	d.mu.Lock()
	d.runCtx = ctx
	d.mu.Unlock()

	ln, err := listenControl(d.cfg.SocketPath)
	if err != nil {
		return err
	}
	d.listener = ln

	log.Printf("[daemon] listening on %s", ln.Addr().String())

	// Start continuous mDNS discovery in the background
	discoverCtx, discoverCancel := context.WithCancel(ctx)
	d.discoverCancel = discoverCancel
	go d.backgroundDiscover(discoverCtx)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("[daemon] accept error: %v", err)
			continue
		}
		go d.handleConn(conn)
	}
}

// Shutdown stops any active sessions and cleans up the socket.
func (d *Daemon) Shutdown() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.discoverCancel != nil {
		d.discoverCancel()
		d.discoverCancel = nil
	}
	d.stopAllLocked()
	if d.listener != nil {
		d.listener.Close()
	}
	cleanupControlAddr(d.cfg.SocketPath)
}

const (
	// mdnsBrowseTimeout bounds a single mDNS browse attempt, for both the
	// background loop and an on-demand scan. A real receiver answers within
	// milliseconds; this timeout only ever matters when nothing is going to
	// answer at all (e.g. multicast blocked by a VPN, another responder
	// squatting on UDP 5353), so keeping it short costs nothing on a
	// working network and saves seconds on this one.
	mdnsBrowseTimeout = 1500 * time.Millisecond

	// deviceTTL is how long a discovered device is kept in the cache after
	// its last sighting before it is dropped.
	deviceTTL = 30 * time.Second

	// backgroundScanInterval is the minimum time between the start of one
	// background discovery scan (mDNS, plus a subnet fallback scan when
	// allowed — see backgroundFallbackInterval) and the next, independent of
	// how quickly an individual scan completes.
	backgroundScanInterval = 10 * time.Second

	// backgroundFallbackInterval is the minimum time between the background
	// loop's own /24 TCP fallback scans once at least one device is already
	// cached. On a network where mDNS never resolves (VPN, a competing
	// responder), the loop would otherwise run a full subnet sweep every
	// backgroundScanInterval, indefinitely, for as long as the daemon is up
	// — real network/battery cost for zero new information once the
	// receiver everyone cares about is already known. 90s keeps the cache
	// self-healing at a sane pace (a receiver that appears after being off
	// gets picked up within a minute and a half without the user having to
	// press "search") while cutting scan frequency 9x. It does not affect
	// the on-demand "discover" command, which always scans immediately
	// regardless of this interval (see handleDiscover/discoverParallel).
	backgroundFallbackInterval = 90 * time.Second
)

// backgroundDiscover continuously browses mDNS for AirPlay devices, at most
// once every backgroundScanInterval. The TCP subnet fallback scan only rides
// along under one of these conditions, checked fresh each cycle:
//   - no device is cached yet (fast first-find matters, so scan every cycle
//     until something is found), or
//   - at least backgroundFallbackInterval has passed since the loop's own
//     last fallback scan (keeps the cache eventually-consistent without
//     scanning the /24 every ~10s forever), and
//   - no stream is currently active (a live mirror session shouldn't share
//     bandwidth/CPU with a subnet sweep nobody asked for).
//
// The "discover" command does not wait for this loop's next cycle and is
// not subject to any of the above — it always runs an immediate scan (see
// handleDiscover), sharing this loop's in-flight fallback scan when the
// timing overlaps (via runFallbackScan) rather than running a redundant /24
// sweep.
func (d *Daemon) backgroundDiscover(ctx context.Context) {
	log.Printf("[daemon] starting continuous mDNS discovery")
	var lastFallback time.Time
	for {
		cycleStart := time.Now()

		d.mu.Lock()
		haveDevice := len(d.devices) > 0
		streaming := len(d.streams) > 0
		d.mu.Unlock()

		allowFallback := !streaming && (!haveDevice || time.Since(lastFallback) >= backgroundFallbackInterval)

		if ctx.Err() != nil {
			return
		}
		found, ok, usedFallback := d.discoverSequential(ctx, allowFallback)
		if usedFallback {
			lastFallback = time.Now()
		}
		if ctx.Err() != nil {
			return
		}
		if ok {
			now := time.Now()
			d.mu.Lock()
			d.mergeDiscoveredLocked(found, now)
			d.logDiscoveryOutcomeLocked()
			d.mu.Unlock()
		}

		if remaining := backgroundScanInterval - time.Since(cycleStart); remaining > 0 {
			select {
			case <-time.After(remaining):
			case <-ctx.Done():
				return
			}
		}
	}
}

// performDiscoverScan runs one discovery pass using the given strategy and
// merges whatever it finds into the device cache. Used by the on-demand
// "discover" command (with discoverParallel, which is never throttled —
// see backgroundDiscover for where the throttling lives). Must NOT be
// called with d.mu held.
func (d *Daemon) performDiscoverScan(ctx context.Context, scan func(context.Context) ([]airplay.AirPlayDevice, bool)) {
	if ctx.Err() != nil {
		return
	}

	found, ok := scan(ctx)

	if ctx.Err() != nil || !ok {
		// Hard failure (or shutdown mid-scan) — leave the existing cache
		// alone rather than wiping it out. ok is still true for a clean
		// "scanned, found nothing" result, which does update the cache (and
		// lets deviceTTL expire stale entries).
		return
	}

	now := time.Now()
	d.mu.Lock()
	d.mergeDiscoveredLocked(found, now)
	d.logDiscoveryOutcomeLocked()
	d.mu.Unlock()
}

// logDiscoveryOutcomeLocked logs the current set of known devices, but only
// when it differs from the last set that was logged — repeating "found the
// same device(s) again" on every scan is exactly the log spam the
// background loop used to produce every ~10s indefinitely. An actual change
// (a device appears, or one drops out of the cache after deviceTTL) always
// gets a line; an unchanged result only appears with -debug. Must be called
// with d.mu held, after mergeDiscoveredLocked.
func (d *Daemon) logDiscoveryOutcomeLocked() {
	sig := deviceSetSignature(d.devices)
	if sig == d.loggedDeviceSignature {
		if d.cfg.Debug {
			log.Printf("[daemon] discovery scan: %d known device(s), unchanged", len(d.devices))
		}
		return
	}
	d.loggedDeviceSignature = sig

	if len(d.devices) == 0 {
		log.Printf("[daemon] no AirPlay devices known (none found, or all expired)")
		return
	}
	names := make([]string, len(d.devices))
	for i, dev := range d.devices {
		names[i] = fmt.Sprintf("%s (%s)", dev.Name, dev.IP)
	}
	log.Printf("[daemon] known AirPlay devices: %s", strings.Join(names, ", "))
}

// deviceSetSignature builds a comparable summary of a device list for
// logDiscoveryOutcomeLocked's change detection. devices is already sorted by
// IP (mergeDiscoveredLocked does this), so equal sets always produce equal
// signatures regardless of scan order.
func deviceSetSignature(devices []airplay.AirPlayDevice) string {
	parts := make([]string, len(devices))
	for i, dev := range devices {
		parts[i] = dev.IP + "|" + dev.DeviceID + "|" + dev.Name
	}
	return strings.Join(parts, ";")
}

// runFallbackScan runs the TCP subnet fallback scan (DiscoverAirPlayDevicesFallback),
// coalescing concurrent callers into a single in-flight /24 sweep. This is
// the one part of discovery expensive enough to need real single-flight
// protection — mDNS is cheap enough that each caller just runs its own (see
// discoverParallel) — so it is the only thing gated here, and every call
// site that needs the fallback scan (both discoverSequential and
// discoverParallel, whether triggered by the background loop or an
// on-demand "discover") goes through this one method. That is also what
// lets an on-demand parallel scan get a fast answer even while the
// background loop's own (sequential, slower) scan is mid-flight: it doesn't
// wait for that whole scan, it just runs (or joins) the fallback here
// directly, without also having to sit through that other scan's mDNS
// phase.
func (d *Daemon) runFallbackScan(ctx context.Context) ([]airplay.AirPlayDevice, error) {
	d.mu.Lock()
	if d.fallbackScan != nil {
		state := d.fallbackScan
		d.mu.Unlock()
		state.wg.Wait()
		return state.found, state.err
	}
	state := &fallbackScanState{}
	state.wg.Add(1)
	d.fallbackScan = state
	d.mu.Unlock()

	found, err := airplay.DiscoverAirPlayDevicesFallback(ctx)
	// Log errors here, exactly once per actual scan, not in each caller:
	// several callers (the background loop plus one or more on-demand
	// "discover" commands) can all be waiting on this same in-flight scan,
	// and logging in discoverSequential/discoverParallel instead would
	// print one line per *caller* even though only one /24 sweep actually
	// ran. A successful scan's outcome is logged by the caller's merge into
	// the device cache (logDiscoveryOutcomeLocked), which — unlike this
	// method — knows whether the result actually changed anything and can
	// stay quiet when it didn't.
	if err != nil {
		log.Printf("[daemon] discovery fallback scan error: %v", err)
	}

	d.mu.Lock()
	state.found, state.err = found, err
	d.fallbackScan = nil
	d.mu.Unlock()
	state.wg.Done()

	return found, err
}

// discoverSequential runs mDNS to completion first, only falling back to the
// directed TCP subnet scan (via runFallbackScan) if mDNS returned nothing
// AND allowFallback is true. Used by the continuous background loop, which
// decides allowFallback each cycle (see its doc) to avoid scanning the
// local /24 indefinitely once a device is already known. usedFallback
// reports whether the fallback scan actually ran (as opposed to being
// skipped by allowFallback=false, or skipped because mDNS itself
// succeeded), so the caller can pace its own throttling correctly. ok is
// false only on a hard failure, or a deliberate skip, that should leave the
// device cache untouched — a clean scan that simply found nothing still
// reports ok=true.
func (d *Daemon) discoverSequential(ctx context.Context, allowFallback bool) (found []airplay.AirPlayDevice, ok bool, usedFallback bool) {
	browseCtx, cancel := context.WithTimeout(ctx, mdnsBrowseTimeout)
	mdnsFound, mdnsErr := airplay.DiscoverAirPlayDevices(browseCtx)
	cancel()

	if ctx.Err() != nil {
		return nil, false, false
	}
	if mdnsErr != nil {
		log.Printf("[daemon] mDNS browse error: %v", mdnsErr)
	}
	// mDNS can fail entirely (e.g. UDP 5353 already owned by another
	// responder, or a VPN interfering with multicast) even though receivers
	// are reachable. Fall back to a directed TCP scan of the local subnet
	// only when mDNS turned up nothing — never in addition to a successful
	// mDNS result.
	if mdnsErr == nil && len(mdnsFound) > 0 {
		return mdnsFound, true, false
	}

	if !allowFallback {
		// Nothing fresh to report this cycle: mDNS found nothing (as
		// usual on this network) and the background loop has decided not
		// to re-scan the subnet right now. Report "no result" rather than
		// "scanned, found nothing" so the cache (and its TTL-based pruning,
		// which only runs on an actual merge) is left exactly as it was —
		// silence here must not look like a fresh "confirmed empty" scan.
		return nil, false, false
	}

	// runFallbackScan logs the outcome itself (see its doc for why that log
	// must live there and not here).
	fbFound, fbErr := d.runFallbackScan(ctx)
	if ctx.Err() != nil {
		return nil, false, true
	}
	if fbErr != nil {
		if mdnsErr != nil {
			return nil, false, true // both mDNS and the fallback failed hard
		}
		return mdnsFound, true, true // mDNS itself succeeded, just with zero devices
	}
	return fbFound, true, true
}

// discoverParallel runs the mDNS browse and the TCP subnet fallback scan
// (via runFallbackScan) concurrently and merges their results (deduplicated
// by IP, with mDNS entries winning on collision since mDNS's TXT records
// give a cleaner device name than the fallback's bare /info parse). Used by
// the on-demand "discover" command so its response time is roughly the
// slower of the two paths instead of their sum, and — because it goes
// through runFallbackScan rather than calling the fallback scan directly —
// so it never has to wait out an unrelated in-flight background scan's own
// mDNS phase just to get a fast answer. See discoverSequential for the
// ok=false contract.
func (d *Daemon) discoverParallel(ctx context.Context) (found []airplay.AirPlayDevice, ok bool) {
	var (
		wg                 sync.WaitGroup
		mdnsFound, fbFound []airplay.AirPlayDevice
		mdnsErr, fbErr     error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		browseCtx, cancel := context.WithTimeout(ctx, mdnsBrowseTimeout)
		mdnsFound, mdnsErr = airplay.DiscoverAirPlayDevices(browseCtx)
		cancel()
	}()
	go func() {
		defer wg.Done()
		fbFound, fbErr = d.runFallbackScan(ctx)
	}()
	wg.Wait()

	if ctx.Err() != nil {
		return nil, false
	}
	if mdnsErr != nil {
		log.Printf("[daemon] mDNS browse error: %v", mdnsErr)
	}
	// runFallbackScan already logged the fallback outcome itself.
	if mdnsErr != nil && fbErr != nil {
		return nil, false // both paths failed hard
	}

	return mergeDeviceLists(mdnsFound, fbFound), true
}

// mergeDeviceLists merges two device lists, deduplicating by IP; on a
// collision the entry from primary is kept.
func mergeDeviceLists(primary, secondary []airplay.AirPlayDevice) []airplay.AirPlayDevice {
	byIP := make(map[string]airplay.AirPlayDevice, len(primary)+len(secondary))
	for _, dev := range secondary {
		byIP[dev.IP] = dev
	}
	for _, dev := range primary {
		byIP[dev.IP] = dev // primary overwrites secondary on collision
	}
	merged := make([]airplay.AirPlayDevice, 0, len(byIP))
	for _, dev := range byIP {
		merged = append(merged, dev)
	}
	return merged
}

// mergeDiscoveredLocked merges freshly discovered devices into the device
// cache, refreshing their last-seen timestamps, and drops any cached device
// not seen within deviceTTL. Must be called with d.mu held.
func (d *Daemon) mergeDiscoveredLocked(found []airplay.AirPlayDevice, now time.Time) {
	// Build a map of currently known devices by IP for quick lookup
	known := make(map[string]airplay.AirPlayDevice, len(d.devices))
	for _, dev := range d.devices {
		known[dev.IP] = dev
	}

	// Update last-seen timestamps and merge new devices
	for _, dev := range found {
		d.deviceLastSeen[dev.IP] = now
		known[dev.IP] = dev // add or update
	}

	// Rebuild device list, dropping anything older than TTL
	devices := make([]airplay.AirPlayDevice, 0, len(known))
	for ip, dev := range known {
		if now.Sub(d.deviceLastSeen[ip]) <= deviceTTL {
			devices = append(devices, dev)
		} else {
			delete(d.deviceLastSeen, ip)
		}
	}
	d.devices = devices
	sort.Slice(d.devices, func(i, j int) bool {
		return d.devices[i].IP < d.devices[j].IP
	})
}

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req Request
	if err := dec.Decode(&req); err != nil {
		enc.Encode(Response{OK: false, Error: "invalid request: " + err.Error()})
		return
	}

	resp := d.handleRequest(req)
	enc.Encode(resp)
}

func (d *Daemon) handleRequest(req Request) Response {
	switch req.Cmd {
	case "status":
		return d.handleStatus()
	case "discover":
		return d.handleDiscover()
	case "devices":
		return d.handleDevices()
	case "connect":
		return d.handleConnect(req)
	case "disconnect":
		return d.handleDisconnect(req)
	case "mute":
		return d.handleSetMute(req, true)
	case "unmute":
		return d.handleSetMute(req, false)
	default:
		return Response{OK: false, Error: "unknown command: " + req.Cmd}
	}
}

// overallState returns the aggregate daemon state based on active streams.
// Must be called with d.mu held.
func (d *Daemon) overallStateLocked() State {
	if d.pendingTarget != "" {
		return StatePINRequired
	}
	hasStreaming := false
	hasConnecting := false
	for _, s := range d.streams {
		switch s.state {
		case StateStreaming:
			hasStreaming = true
		case StateConnecting:
			hasConnecting = true
		}
	}
	if hasStreaming {
		return StateStreaming
	}
	if hasConnecting {
		return StateConnecting
	}
	return StateIdle
}

// streamHasAudio reports whether a stream's audio should be advertised to
// callers. The receiver may negotiate audio ports during SETUP whenever it
// offers them, independent of Config.NoAudio — that flag only stops the
// daemon from starting the local audio capture pipeline. So with -no-audio
// set, no audio is ever actually flowing, and has_audio must report false
// even though session.HasAudio() (a negotiation-level fact) is true.
func (d *Daemon) streamHasAudio(s *activeStream) bool {
	return !d.cfg.NoAudio && s.session != nil && s.session.HasAudio()
}

func (d *Daemon) handleStatus() Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statusResponseLocked(true, "")
}

func (d *Daemon) statusResponseLocked(ok bool, errMsg string) Response {
	streams := make([]StreamInfo, 0, len(d.streams))
	for _, s := range d.streams {
		streams = append(streams, StreamInfo{
			Device:     s.device,
			DeviceIP:   s.deviceIP,
			State:      s.state,
			HasAudio:   d.streamHasAudio(s),
			AudioMuted: s.audioMuted,
		})
	}
	// Sort for deterministic output
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].DeviceIP < streams[j].DeviceIP
	})

	overall := d.overallStateLocked()

	// Populate legacy single-stream fields using the first streaming entry for
	// backwards-compatibility with existing clients.
	var device, deviceIP string
	var hasAudio, audioMuted bool
	for _, s := range streams {
		if s.State == StateStreaming {
			device = s.Device
			deviceIP = s.DeviceIP
			hasAudio = s.HasAudio
			audioMuted = s.AudioMuted
			break
		}
	}

	return Response{
		OK:         ok,
		State:      overall,
		Device:     device,
		DeviceIP:   deviceIP,
		HasAudio:   hasAudio,
		AudioMuted: audioMuted,
		NeedsPIN:   overall == StatePINRequired,
		Error:      errMsg,
		Streams:    streams,
	}
}

// handleDiscover runs a fresh discovery scan and returns whatever it finds
// in the same response. This deliberately does not rely on the background
// discovery loop's cache: on machines where mDNS never works (VPN, a
// competing responder, ...) the fallback is the normal path, and a caller
// that queries "discover" right after starting the daemon must not see an
// empty list just because the background loop hasn't gotten there yet. The
// scan runs mDNS and the TCP subnet fallback in parallel (discoverParallel)
// so a GUI's "Suchen" button isn't stuck waiting for a mDNS timeout and then
// a full fallback scan back to back — and, because the fallback scan alone
// is single-flighted (runFallbackScan) rather than the whole scan, this does
// not have to wait out the background loop's own scan if one happens to be
// running at that moment either.
func (d *Daemon) handleDiscover() Response {
	d.mu.Lock()
	ctx := d.runCtx
	d.mu.Unlock()
	if ctx == nil {
		// Run() was never called (e.g. a unit test driving handlers
		// directly); there is no daemon lifetime to bound the scan by.
		ctx = context.Background()
	}
	d.performDiscoverScan(ctx, d.discoverParallel)

	d.mu.Lock()
	defer d.mu.Unlock()
	return Response{
		OK:      true,
		State:   d.overallStateLocked(),
		Devices: toDeviceInfos(d.devices),
	}
}

func (d *Daemon) handleDevices() Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Response{
		OK:      true,
		State:   d.overallStateLocked(),
		Devices: toDeviceInfos(d.devices),
	}
}

func (d *Daemon) handleConnect(req Request) Response {
	d.mu.Lock()

	// If we're waiting for a PIN, resume the existing connection rather than
	// creating a new client and losing the receiver's pending pairing session.
	if d.pendingTarget != "" && req.Pin != "" {
		target := d.pendingTarget
		if req.Target != "" && req.Target != target {
			d.mu.Unlock()
			return Response{OK: false, State: StatePINRequired, Error: "a different device is waiting for a PIN"}
		}
		entry, ok := d.streams[target]
		if !ok || entry.state != StatePINRequired || entry.pinCh == nil {
			d.pendingTarget = ""
			state := d.overallStateLocked()
			d.mu.Unlock()
			return Response{OK: false, State: state, Error: "pending PIN session is no longer available"}
		}
		d.pendingTarget = ""
		entry.state = StateConnecting
		// pinCh is buffered and each pending session can be claimed only once.
		entry.pinCh <- req.Pin
		d.mu.Unlock()
		return Response{OK: true, State: StateConnecting, Device: target}
	}
	if req.Pin != "" && req.Target == "" {
		state := d.overallStateLocked()
		d.mu.Unlock()
		return Response{OK: false, State: state, Error: "no device is waiting for a PIN"}
	}

	// Reject a duplicate connection to the same target.
	target := req.Target
	if target != "" {
		if existing, ok := d.streams[target]; ok {
			st := existing.state
			d.mu.Unlock()
			return Response{OK: false, State: st, Error: "already connected or connecting to " + target}
		}
	}

	// If no target specified, use first cached device not already streaming.
	port := req.Port
	if target == "" {
		target, port = d.pickFreeDeviceLocked(port)
		if target == "" {
			state := d.overallStateLocked()
			d.mu.Unlock()
			return Response{OK: false, State: state, Error: "no available devices found"}
		}
	}

	// Look up the discovered port for this target if not explicitly provided.
	if port == 0 {
		for _, dev := range d.devices {
			if dev.IP == target {
				port = dev.Port
				break
			}
		}
	}
	if port == 0 {
		port = 7000
	}

	// Create the context before publishing the entry so a concurrent disconnect
	// can always cancel the connection goroutine.
	connCtx, cancel := context.WithCancel(context.Background())
	entry := &activeStream{
		deviceIP: target,
		state:    StateConnecting,
		cancelFn: cancel,
		pinCh:    make(chan string, 1),
	}
	d.streams[target] = entry
	d.mu.Unlock()

	go d.connectAndStream(connCtx, entry, target, port, req.Pin)

	d.mu.Lock()
	defer d.mu.Unlock()
	return Response{OK: true, State: d.overallStateLocked(), Device: target}
}

// pickFreeDeviceLocked returns the first discovered device not already in d.streams.
// Must be called with d.mu held.
func (d *Daemon) pickFreeDeviceLocked(preferredPort int) (string, int) {
	for _, dev := range d.devices {
		if _, inUse := d.streams[dev.IP]; !inUse {
			p := dev.Port
			if preferredPort != 0 {
				p = preferredPort
			}
			return dev.IP, p
		}
	}
	return "", 0
}

func (d *Daemon) connectAndStream(ctx context.Context, entry *activeStream, target string, port int, pin string) {
	// removeStream cleans up this stream's entry and tears down the shared broadcast
	// if no other streams remain.
	removeStream := func(msg string) {
		if msg != "" {
			log.Printf("[daemon] %s", msg)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.streams[target] == entry {
			d.removeStreamLocked(target)
		}
	}

	connectClient := func() (*airplay.AirPlayClient, *airplay.ReceiverInfo, error) {
		next := airplay.NewAirPlayClient(target, port)
		if err := next.Connect(ctx); err != nil {
			return nil, nil, err
		}

		// Publish the connected client immediately so disconnect/shutdown can
		// interrupt GetInfo, pairing, or a pending PIN wait.
		d.mu.Lock()
		if d.streams[target] != entry {
			d.mu.Unlock()
			_ = next.Close()
			return nil, nil, context.Canceled
		}
		entry.client = next
		d.mu.Unlock()

		nextInfo, err := next.GetInfo()
		if err != nil {
			_ = next.Close()
			return nil, nil, err
		}

		d.mu.Lock()
		if d.streams[target] != entry {
			d.mu.Unlock()
			_ = next.Close()
			return nil, nil, context.Canceled
		}
		entry.device = nextInfo.Name
		entry.deviceID = nextInfo.DeviceID
		d.mu.Unlock()
		return next, nextInfo, nil
	}

	client, info, err := connectClient()
	if err != nil {
		removeStream(fmt.Sprintf("connect to %s:%d failed: %v", target, port, err))
		return
	}

	deviceID := info.DeviceID
	savedCreds := d.credStore.Lookup(deviceID)
	screenCastRestoreToken := ""
	if savedCreds != nil {
		screenCastRestoreToken = savedCreds.RestoreToken
	}

	reconnect := func() error {
		_ = client.Close()
		next, nextInfo, err := connectClient()
		if err != nil {
			return err
		}
		client = next
		info = nextInfo
		deviceID = info.DeviceID
		return nil
	}

	log.Printf("[daemon] connected to %s (model: %s, deviceID: %s)", info.Name, info.Model, deviceID)

	pairWithPIN := func(pinValue string) error {
		if err := client.Pair(ctx, pinValue); err != nil {
			return err
		}
		if client.PairKeys != nil {
			if err := d.credStore.Save(deviceID, client.PairingID,
				client.PairKeys.Ed25519Public, client.PairKeys.Ed25519Private); err != nil {
				log.Printf("[daemon] warning: failed to save credentials: %v", err)
			} else {
				log.Printf("[daemon] credentials saved for %s (deviceID: %s)", info.Name, deviceID)
			}
		}
		return nil
	}

	waitForPIN := func() (string, error) {
		d.mu.Lock()
		if d.streams[target] != entry {
			d.mu.Unlock()
			return "", context.Canceled
		}
		if d.pendingTarget != "" && d.pendingTarget != target {
			d.mu.Unlock()
			return "", fmt.Errorf("another device is already waiting for a PIN")
		}
		entry.state = StatePINRequired
		d.pendingTarget = target
		d.mu.Unlock()

		if err := client.StartPINDisplay(); err != nil {
			// Fixed-password receivers may reject this request but still accept the
			// password through pair-setup.
			log.Printf("[daemon] start PIN display failed: %v", err)
		}

		log.Printf("[daemon] PIN required for %s — waiting for user input", info.Name)
		select {
		case pinValue := <-entry.pinCh:
			return pinValue, nil
		case <-ctx.Done():
			d.mu.Lock()
			if d.pendingTarget == target {
				d.pendingTarget = ""
			}
			d.mu.Unlock()
			return "", ctx.Err()
		}
	}

	// Pairing
	paired := false
	if pin != "" {
		if err := pairWithPIN(pin); err != nil {
			_ = client.Close()
			removeStream(fmt.Sprintf("pairing failed: %v", err))
			return
		}
		paired = true
	}

	if !paired && savedCreds != nil && savedCreds.HasPairingCredentials() {
		pub, priv := savedCreds.Ed25519Keys()
		client.PairingID = savedCreds.PairingID
		client.PairKeys = &airplay.PairKeys{
			Ed25519Public:  pub,
			Ed25519Private: priv,
		}
		if err := client.PairVerify(ctx); err != nil {
			log.Printf("[daemon] pair-verify with saved creds failed: %v", err)
			if err := reconnect(); err != nil {
				removeStream(fmt.Sprintf("reconnect failed: %v", err))
				return
			}
		} else {
			paired = true
			log.Printf("[daemon] pair-verify succeeded for %s", info.Name)
		}
	} else if !paired && savedCreds != nil {
		log.Printf("[daemon] saved credentials have no usable pair-verify keys, skipping")
	}

	if !paired {
		pairInteractively := func() error {
			pinValue, err := waitForPIN()
			if err != nil {
				return fmt.Errorf("wait for PIN: %w", err)
			}
			if err := pairWithPIN(pinValue); err != nil {
				return fmt.Errorf("PIN pairing: %w", err)
			}
			return nil
		}

		if info.RequiresPINPairing() {
			if err := pairInteractively(); err != nil {
				_ = client.Close()
				removeStream(err.Error())
				return
			}
		} else if err := client.Pair(ctx, ""); err != nil {
			log.Printf("[daemon] transient pairing failed: %v", err)
			// A failed setup may leave pairing state attached to this socket. Start
			// and finish the PIN exchange together on a fresh connection.
			if err := reconnect(); err != nil {
				removeStream(fmt.Sprintf("reconnect for PIN pairing failed: %v", err))
				return
			}
			if err := pairInteractively(); err != nil {
				_ = client.Close()
				removeStream(err.Error())
				return
			}
		} else {
			log.Printf("[daemon] transient pairing succeeded for %s", info.Name)
		}
	}

	// FairPlay setup
	if err := client.FairPlaySetup(ctx); err != nil {
		if !errors.Is(err, airplay.ErrFairPlayUnsupported) {
			client.Close()
			removeStream(fmt.Sprintf("FairPlay setup failed: %v", err))
			return
		}
		log.Printf("[daemon] FairPlay SAP unsupported (%v); continuing with pair-verify DataStream setup", err)
	}

	streamCfg := airplay.StreamConfig{
		FPS:       d.cfg.FPS,
		Bitrate:   d.cfg.Bitrate,
		NoEncrypt: d.cfg.NoEncrypt,
		DirectKey: d.cfg.DirectKey,
		NoAudio:   d.cfg.NoAudio,
	}

	// Start capture before SetupMirror. Modern receivers start a deadline for
	// the first video data during setup, while the Wayland screencast portal may
	// wait indefinitely for user selection.
	sink, err := d.getOrStartBroadcastLocked(screenCastRestoreToken, deviceID)
	if err != nil {
		client.Close()
		removeStream(fmt.Sprintf("capture failed: %v", err))
		return
	}

	session, err := client.SetupMirror(ctx, streamCfg)
	if err != nil {
		sink.Close()
		client.Close()
		removeStream(fmt.Sprintf("mirror setup failed: %v", err))
		return
	}

	d.mu.Lock()
	current, ok := d.streams[target]
	if !ok || current != entry {
		// Stream was cancelled while we were setting up
		d.mu.Unlock()
		sink.Close()
		session.Close()
		client.Close()
		d.mu.Lock()
		d.maybeStopBroadcastLocked()
		d.mu.Unlock()
		return
	}
	current.state = StateStreaming
	current.session = session
	current.client = client
	current.sink = sink
	current.audioMuted = false
	d.mu.Unlock()

	log.Printf("[daemon] streaming to %s (%s)", info.Name, target)

	// Start audio for this stream independently.
	if !d.cfg.NoAudio && session.HasAudio() {
		audioCapture, audioErr := airplay.StartAudioCapture(ctx, d.cfg.TestMode, d.cfg.AudioTCPPort)
		if audioErr != nil {
			log.Printf("[daemon] audio capture failed: %v (continuing without audio)", audioErr)
		} else {
			defer audioCapture.Stop()
			go func() {
				if aerr := session.StreamAudio(ctx, audioCapture, session.AudioStream()); aerr != nil && ctx.Err() == nil {
					log.Printf("[daemon] audio streaming error: %v", aerr)
				}
			}()
			log.Printf("[daemon] audio capture started for %s", target)
		}
	}

	streamErr := session.StreamFrames(ctx, sink.AsCapture(), 0)
	if streamErr != nil {
		if ctx.Err() == nil {
			// The stream ended on its own (no disconnect/shutdown requested
			// this), so this is a genuine, unexpected failure.
			log.Printf("[daemon] stream error for %s: %v", target, streamErr)
		} else if d.cfg.Debug {
			// ctx was cancelled by an intentional disconnect/shutdown,
			// which closes the sink's pipe out from under StreamFrames and
			// surfaces as a "closed pipe" style error here. That is
			// expected, not a failure — only note it at debug level.
			log.Printf("[daemon] stream ended for %s (disconnect): %v", target, streamErr)
		}
	}

	// Cleanup this stream.
	sink.Close()
	session.Close()
	client.Close()

	d.mu.Lock()
	if d.streams[target] == entry {
		d.removeStreamLocked(target)
	}
	d.mu.Unlock()

	log.Printf("[daemon] stream ended for %s", target)
}

// getOrStartBroadcastLocked ensures a shared BroadcastCapture is running and
// returns a new sink registered with it. If no capture is running, it starts one.
// Must NOT be called with d.mu held.
func (d *Daemon) getOrStartBroadcastLocked(restoreToken, deviceID string) (*airplay.BroadcastSink, error) {
	d.mu.Lock()
	bc := d.broadcast
	d.mu.Unlock()

	if bc != nil {
		// Capture already running — add a new sink.
		sink := bc.AddSink()
		return sink, nil
	}

	// Start a fresh screen capture.
	capCfg := airplay.CaptureConfig{
		FPS:          d.cfg.FPS,
		Bitrate:      d.cfg.Bitrate,
		HWAccel:      d.cfg.HWAccel,
		ShowCursor:   d.cfg.ShowCursor,
		RestoreToken: restoreToken,
		OutputIndex:  d.cfg.OutputIndex,
		MaxHeight:    d.cfg.MaxHeight,
	}
	if deviceID != "" {
		capCfg.SaveRestoreToken = func(token string) error {
			return d.credStore.SaveRestoreToken(deviceID, token)
		}
	}

	var (
		capture *airplay.ScreenCapture
		err     error
	)
	captureCtx, captureCancel := context.WithCancel(context.Background())
	if d.cfg.TestMode {
		capture, err = airplay.StartTestCapture(captureCtx, capCfg)
	} else {
		capture, err = airplay.StartCapture(captureCtx, capCfg)
	}
	if err != nil {
		captureCancel()
		return nil, err
	}

	newBC := airplay.NewBroadcastCapture(capture)
	sink := newBC.AddSink()

	d.mu.Lock()
	// Double-check: another goroutine might have started capture concurrently.
	if d.broadcast != nil {
		d.mu.Unlock()
		// Discard the one we just started and use the existing one.
		captureCancel()
		capture.Stop()
		return d.broadcast.AddSink(), nil
	}
	d.broadcast = newBC
	d.capture = capture
	d.captureCancel = captureCancel
	d.captureStopExpected = false
	d.mu.Unlock()

	go func() {
		runErr := newBC.Run()

		d.mu.Lock()
		stopExpected := d.captureStopExpected
		d.mu.Unlock()

		if runErr != nil && runErr.Error() != "EOF" {
			if !stopExpected {
				// The capture ended on its own — a real, unexpected failure.
				log.Printf("[daemon] broadcast capture error: %v", runErr)
			} else if d.cfg.Debug {
				// We intentionally tore this capture down (disconnect/
				// shutdown); the "closed pipe"-style error Run() reports
				// here is just that teardown's side effect, not a failure.
				log.Printf("[daemon] broadcast capture ended (stopped): %v", runErr)
			}
		}
		// When the capture ends, stop all active streams.
		d.mu.Lock()
		d.stopAllLocked()
		d.mu.Unlock()
	}()

	return sink, nil
}

// removeStreamLocked removes a single stream entry and tears down the shared
// capture if no other streams are left. Must be called with d.mu held.
func (d *Daemon) removeStreamLocked(target string) {
	entry, ok := d.streams[target]
	if !ok {
		return
	}
	if d.pendingTarget == target {
		d.pendingTarget = ""
	}
	if entry.cancelFn != nil {
		entry.cancelFn()
	}
	delete(d.streams, target)
	d.maybeStopBroadcastLocked()
}

// maybeStopBroadcastLocked stops the shared capture if no active streams remain.
// Must be called with d.mu held.
func (d *Daemon) maybeStopBroadcastLocked() {
	if len(d.streams) > 0 {
		return
	}
	// Mark this teardown as expected, and cancel the capture's context,
	// before actually stopping it: see the captureStopExpected field doc for
	// why both must happen in this order and while still holding d.mu.
	d.captureStopExpected = true
	if d.captureCancel != nil {
		d.captureCancel()
		d.captureCancel = nil
	}
	if d.capture != nil {
		d.capture.Stop()
		d.capture = nil
	}
	d.broadcast = nil
}

func (d *Daemon) handleDisconnect(req Request) Response {
	d.mu.Lock()
	defer d.mu.Unlock()

	// If a target is specified, disconnect only that stream.
	if req.Target != "" {
		entry, ok := d.streams[req.Target]
		if !ok {
			return Response{OK: false, State: d.overallStateLocked(), Error: "no active stream to " + req.Target}
		}
		// Cancel the streaming goroutine's context before tearing down its
		// sink/session/client (mirrors stopAllLocked's order below). That
		// goroutine's own ctx.Err() check then correctly recognizes the
		// "read on closed pipe"/connection-closed errors this causes as
		// the expected effect of an intentional disconnect rather than a
		// real streaming failure worth logging.
		d.removeStreamLocked(req.Target)
		if entry.sink != nil {
			entry.sink.Close()
		}
		if entry.session != nil {
			entry.session.Close()
		}
		if entry.client != nil {
			entry.client.Close()
		}
		return Response{OK: true, State: d.overallStateLocked()}
	}

	// Disconnect all.
	d.stopAllLocked()
	return Response{OK: true, State: StateIdle}
}

func (d *Daemon) handleSetMute(req Request, muted bool) Response {
	d.mu.Lock()

	var targets []*activeStream
	if req.Target != "" {
		entry, ok := d.streams[req.Target]
		if !ok {
			state := d.overallStateLocked()
			d.mu.Unlock()
			return Response{OK: false, State: state, Error: "no active stream to " + req.Target}
		}
		targets = []*activeStream{entry}
	} else {
		for _, s := range d.streams {
			if s.state == StateStreaming {
				targets = append(targets, s)
			}
		}
	}

	if len(targets) == 0 {
		resp := d.statusResponseLocked(false, "not currently streaming")
		d.mu.Unlock()
		return resp
	}

	sessions := make([]*airplay.MirrorSession, 0, len(targets))
	for _, t := range targets {
		if t.session != nil && (d.cfg.NoAudio || t.session.HasAudio()) {
			sessions = append(sessions, t.session)
		}
	}
	d.mu.Unlock()

	var lastErr error
	for _, s := range sessions {
		if err := s.SetAudioMuted(muted); err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.statusResponseLocked(false, "failed to update audio mute state: "+lastErr.Error())
	}

	d.mu.Lock()
	for _, t := range targets {
		t.audioMuted = muted
	}
	defer d.mu.Unlock()
	return d.statusResponseLocked(true, "")
}

// stopAllLocked stops all active streams and tears down the capture.
// Must be called with d.mu held.
func (d *Daemon) stopAllLocked() {
	d.pendingTarget = ""
	for target, entry := range d.streams {
		if entry.cancelFn != nil {
			entry.cancelFn()
		}
		if entry.sink != nil {
			entry.sink.Close()
		}
		if entry.session != nil {
			entry.session.Close()
		}
		if entry.client != nil {
			entry.client.Close()
		}
		delete(d.streams, target)
	}
	// Mark this teardown as expected, and cancel the capture's context,
	// before actually stopping it — see the captureStopExpected field doc.
	// This order was previously reversed here (capture.Stop() ran before
	// captureCancel()), which is what let the "broadcast capture error:
	// read |0: file already closed" log line slip through on an intentional
	// disconnect: the shared capture's Run() goroutine could wake up from
	// capture.Stop() closing the pipe before its context was visibly
	// cancelled, so it mistook the resulting error for an unexpected
	// failure. maybeStopBroadcastLocked already had the correct order; this
	// path (used by a target-less "disconnect" and by Shutdown) did not.
	d.captureStopExpected = true
	if d.captureCancel != nil {
		d.captureCancel()
		d.captureCancel = nil
	}
	if d.capture != nil {
		d.capture.Stop()
		d.capture = nil
	}
	d.broadcast = nil
}

func toDeviceInfos(devices []airplay.AirPlayDevice) []DeviceInfo {
	infos := make([]DeviceInfo, len(devices))
	for i, d := range devices {
		infos[i] = DeviceInfo{
			Name:     d.Name,
			Model:    d.Model,
			IP:       d.IP,
			Port:     d.Port,
			DeviceID: d.DeviceID,
		}
	}
	return infos
}
