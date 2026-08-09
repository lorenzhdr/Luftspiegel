//go:build windows

package airplay

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// pcmInputRate is the sample rate the TCP audio source is contractually
	// required to send (see tcpPCMSource doc comment). The sender asks its
	// AudioContext for 44.1kHz directly so no conversion is needed here:
	// Chromium resamples from the device rate with better filters than the
	// linear interpolation below. The resampler stays in the path because it
	// passes samples through unchanged when the rates match, which keeps a
	// future sender that can only produce 48kHz working.
	pcmInputRate = 44100
	// pcmOutputRate matches the 44.1kHz AirPlay declares for mirrored audio
	// (see AudioCodec.Info and the "sr": 44100 SDP field in mirror.go) — the
	// same rate GStreamer's audioresample element converts to on Linux.
	pcmOutputRate     = 44100
	pcmBytesPerFrame  = 4 // stereo, S16LE: 2 channels * 2 bytes
	pcmOutBytesPerSec = pcmOutputRate * pcmBytesPerFrame

	// defaultPCMBufferMs is the backlog target when the caller does not
	// specify one.
	defaultPCMBufferMs = 120
	// minPCMBufferMs / maxPCMBufferMs bound the configurable target. Below the
	// minimum an ordinary scheduling hiccup on the sender turns into audible
	// dropouts, because an underrun is padded with silence and never reported
	// as an error.
	minPCMBufferMs = 40
	maxPCMBufferMs = 500

	// drainThresholdPct is how far above the configured target (as a percent
	// of it) the buffer must climb before appendPCM's incremental drain does
	// anything. 150% gives ordinary jitter — a slightly late TCP read, a
	// scheduler hiccup on either end of the connection — enough headroom to
	// resolve on its own through normal consumption, so draining stays the
	// exception rather than a constant tug-of-war against a source that is
	// merely a bit bursty.
	drainThresholdPct = 150
	// hardCapPct is the emergency backstop, kept from the original
	// drop-oldest implementation: a single append that jumps the buffer past
	// this (e.g. one oversized TCP read delivered after a stall) is clipped
	// straight down in that same call instead of waiting for the
	// incremental drain — bounded to drainStepBytes per call — to catch up
	// over however many further appends that would take. Set above
	// drainThresholdPct with real headroom so it only fires for a genuine
	// burst; the incremental drain already handles the steady-state
	// overshoot this bug was actually about.
	hardCapPct = 200

	// alacFrameBytes is one ALAC frame's worth of PCM at the output rate —
	// 352 samples * pcmBytesPerFrame — used to size the drain increment in
	// units that line up with what ReadFrame consumes per call.
	alacFrameBytes = 352 * pcmBytesPerFrame
	// drainStepFrames/drainStepBytes bound how much a single incremental
	// drain step removes. The bug this file fixes was dropping the entire
	// overshoot (historically up to 300ms, i.e. ~180ms of audio) in one
	// shot, which is an audible click; 1-2 ALAC frames (~8-16ms) is short
	// enough to be inaudible, so a sustained overshoot eases back toward the
	// target over many appends/reads instead of cutting once.
	drainStepFrames = 2
	drainStepBytes  = drainStepFrames * alacFrameBytes

	// readWaitTimeout bounds how long tcpPCMSource.Read will wait for a full
	// frame's worth of real PCM before padding the shortfall with silence and
	// returning anyway. It is set just above one ALAC frame's duration at
	// 44.1kHz (352/44100 ≈ 8ms) so a healthy stream is never held up waiting,
	// while a missing/lagging client still gets a bounded, non-blocking read.
	readWaitTimeout  = 12 * time.Millisecond
	readPollInterval = 1 * time.Millisecond
)

// resolvePCMBufferMs clamps a caller-requested PCM backlog target to
// [minPCMBufferMs, maxPCMBufferMs], substituting defaultPCMBufferMs when the
// caller expressed no preference (0). Broken out as a plain function, rather
// than inlined into StartAudioCapture, so the clamping rules are unit
// testable without spinning up a listener.
func resolvePCMBufferMs(ms int) int {
	switch {
	case ms == 0:
		return defaultPCMBufferMs
	case ms < minPCMBufferMs:
		return minPCMBufferMs
	case ms > maxPCMBufferMs:
		return maxPCMBufferMs
	default:
		return ms
	}
}

// StartAudioCapture starts the Windows audio source: either a synthetic sine
// test tone (testTone=true, used by -test / daemon TestMode, no network
// involved) or a local TCP listener that accepts raw PCM audio pushed by an
// external process doing WASAPI loopback capture (in practice, doubletake's
// companion Electron GUI). Either way the result feeds AudioCapture.ReadFrame
// exactly like the Linux GStreamer pipeline does — see audio.go.
//
// TCP wire contract (fixed, shared with the Electron GUI):
//   - doubletake is the TCP server (net.Listen), the GUI is the client and
//     connects to it.
//   - 127.0.0.1 only, port tcpPort (0 or negative means DefaultAudioTCPPort).
//   - Payload: raw PCM, s16le, 44100 Hz, 2 channels (stereo), interleaved,
//     little endian, no header — a continuous byte stream. That works out to
//     176400 bytes/sec, which is the cheapest way to spot a sender that got
//     the rate, channel count or bit depth wrong.
//
// Audio must never block video: if no client is connected, or the connected
// client stalls, ReadFrame (via tcpPCMSource.Read) returns silence within
// readWaitTimeout instead of blocking or erroring. See tcpPCMSource for the
// buffering/backpressure and reconnect handling.
// bufferMs bounds the PCM backlog held for the receiver; 0 selects
// defaultPCMBufferMs. It is the dominant contributor to audio latency, so it
// is configurable rather than fixed — see tcpPCMSource for the drain policy
// that keeps the steady state near the target instead of parked at the cap.
func StartAudioCapture(ctx context.Context, testTone bool, tcpPort, bufferMs int) (*AudioCapture, error) {
	captureCtx, cancel := context.WithCancel(ctx)

	ac := &AudioCapture{
		cancel: cancel,
		waitCh: make(chan struct{}),
	}

	if testTone {
		dbg("[AUDIO] using test tone (440 Hz sine wave, %dHz stereo)", pcmOutputRate)
		ac.pcmPipe = newSineWaveSource(440)
		go func() {
			<-captureCtx.Done()
			close(ac.waitCh)
		}()
		return ac, nil
	}

	if tcpPort <= 0 {
		tcpPort = DefaultAudioTCPPort
	}
	addr := fmt.Sprintf("127.0.0.1:%d", tcpPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("listen for PCM audio source on %s: %w", addr, err)
	}
	dbg("[AUDIO] waiting for PCM audio source to connect on %s (s16le %dHz stereo)", addr, pcmInputRate)

	bufferMs = resolvePCMBufferMs(bufferMs)
	src := newTCPPCMSource(listener, bufferMs)
	dbg("[AUDIO] PCM backlog target: %dms (drain above %dms, hard cap %dms)",
		bufferMs, bufferMs*drainThresholdPct/100, bufferMs*hardCapPct/100)
	go src.acceptLoop(captureCtx)
	go func() {
		<-captureCtx.Done()
		src.Close()
		close(ac.waitCh)
	}()

	ac.pcmPipe = src
	ac.extraStop = func() { src.Close() }
	return ac, nil
}

// tcpPCMSource is an io.ReadCloser that feeds AudioCapture.ReadFrame from a
// TCP listener. It owns three responsibilities beyond plain byte relaying:
//
//  1. Never block the caller: Read always returns a full buffer within
//     readWaitTimeout, padding with silence if not enough real PCM has
//     arrived (no client connected yet, client disconnected, or client
//     momentarily behind). It never returns an error except after Close.
//  2. Bound latency/drift: incoming PCM (after resampling to pcmOutputRate)
//     is kept in a byte queue steered toward a configurable targetBytes by
//     appendPCM's incremental drain, with hardCapBytes as a hard emergency
//     ceiling (oldest bytes dropped) for a single burst the incremental
//     drain cannot absorb in one call. This is the Windows equivalent of the
//     Linux backend's one-shot AudioCapture.DrainStale — see the note on
//     DrainStale below for why it becomes a no-op here.
//  3. Reconnect: exactly one connection is treated as "current" at a time.
//     A newly accepted connection replaces the previous one — the previous
//     is closed and its read loop exits. This favors the common real-world
//     case (the GUI being restarted, e.g. after a crash or config change)
//     over a hypothetical second, unwanted sender, and avoids two writers
//     racing into the same buffer if a stale connection lingers rather than
//     being cleanly closed by the old client.
type tcpPCMSource struct {
	listener net.Listener

	mu  sync.Mutex
	buf []byte // resampled pcmOutputRate S16LE PCM, oldest-first

	// targetBytes/drainThresholdBytes/hardCapBytes are derived once (in
	// newTCPPCMSource) from the caller's bufferMs and never change, so they
	// need no synchronization of their own — only s.buf, guarded by mu, is
	// mutated at runtime.
	targetBytes         int
	drainThresholdBytes int
	hardCapBytes        int

	connMu sync.Mutex
	conn   net.Conn

	closeOnce sync.Once
	doneCh    chan struct{}

	// stats is set after StartAudioCapture returns, once the caller has a
	// SessionStats to hand over (see AudioCapture.SetStats in audio.go) —
	// i.e. concurrently with acceptLoop/readLoop/Read already running.
	// atomic.Pointer gives a lock-free Store/Load pair for that handoff
	// without adding a mutex to the append/read hot path, which already
	// takes mu on every call; a *SessionStats swap doesn't need mu's
	// exclusion, just word-atomicity. A nil pointer (before SetStats is
	// called) is a valid, safe value: every SessionStats method tolerates a
	// nil receiver (see stats.go), so call sites here never need to check.
	stats atomic.Pointer[SessionStats]
}

func newTCPPCMSource(listener net.Listener, bufferMs int) *tcpPCMSource {
	target := bufferMs * pcmOutBytesPerSec / 1000
	target -= target % pcmBytesPerFrame
	threshold := target * drainThresholdPct / 100
	threshold -= threshold % pcmBytesPerFrame
	hardCap := target * hardCapPct / 100
	hardCap -= hardCap % pcmBytesPerFrame

	return &tcpPCMSource{
		listener:            listener,
		doneCh:              make(chan struct{}),
		targetBytes:         target,
		drainThresholdBytes: threshold,
		hardCapBytes:        hardCap,
	}
}

// SetStats attaches the session's statistics collector. See the stats field
// doc above for why this is safe to call while acceptLoop/readLoop/Read are
// already running.
func (s *tcpPCMSource) SetStats(stats *SessionStats) {
	s.stats.Store(stats)
}

// acceptLoop accepts connections until ctx is done or the listener is
// closed. Deliberately does not select on ctx directly — net.Listener.Accept
// is a blocking OS call; Close (called when ctx is done, see
// StartAudioCapture) closes the listener out from under it, which is the
// standard way to unblock an in-flight Accept.
func (s *tcpPCMSource) acceptLoop(ctx context.Context) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.doneCh:
			default:
				dbg("[AUDIO] TCP accept error: %v", err)
			}
			return
		}
		dbg("[AUDIO] audio source connected: %s", conn.RemoteAddr())

		s.connMu.Lock()
		old := s.conn
		s.conn = conn
		s.connMu.Unlock()
		if old != nil {
			dbg("[AUDIO] new audio connection replaces previous one")
			old.Close()
		}

		go s.readLoop(conn)
	}
}

// readLoop relays one connection's PCM into the bounded buffer, resampling
// from the contractual 48kHz input to the 44.1kHz ALAC expects, until the
// connection errors (including being closed by acceptLoop on replacement, or
// by Close on shutdown) or the peer disconnects.
func (s *tcpPCMSource) readLoop(conn net.Conn) {
	resampler := newLinearResampler(pcmInputRate, pcmOutputRate)
	readBuf := make([]byte, 4096)
	var pending []byte // carries a trailing partial stereo frame across reads

	for {
		n, err := conn.Read(readBuf)
		if n > 0 {
			data := readBuf[:n]
			if len(pending) > 0 {
				data = append(pending, data...)
				pending = nil
			}
			usable := len(data) - (len(data) % pcmBytesPerFrame)
			if usable > 0 {
				s.appendPCM(resampler.resample(data[:usable]))
			}
			if usable < len(data) {
				pending = append([]byte(nil), data[usable:]...)
			}
		}
		if err != nil {
			s.connMu.Lock()
			if s.conn == conn {
				s.conn = nil
			}
			s.connMu.Unlock()
			if err != io.EOF {
				dbg("[AUDIO] audio source connection error: %v", err)
			} else {
				dbg("[AUDIO] audio source disconnected")
			}
			return
		}
	}
}

// appendPCM adds resampled PCM to the buffer and then, in up to two steps,
// keeps the backlog near targetBytes instead of parked at hardCapBytes:
//
//  1. Incremental drain: if the buffer is above drainThresholdBytes, drop at
//     most drainStepBytes (~1-2 ALAC frames) from the front. One call only
//     ever removes one step, so a sustained overshoot eases back down over
//     many appends/reads rather than being corrected in one audible cut.
//  2. Emergency backstop: if the buffer is still above hardCapBytes after
//     step 1 — meaning this single append was too large for one drain step
//     to absorb (e.g. one oversized TCP read after a stall) — drop straight
//     down to hardCapBytes. This is the original drop-oldest behavior,
//     preserved as a rare fallback rather than the everyday mechanism it
//     used to be (see the maxBufferedPCM removal in the fix this is part
//     of).
//
// Both steps report their drop through RecordAudioDrain: the hard-cap
// overflow counts as a drain too, deliberately, since a target that is
// undersized for the incoming stream should show up as sustained drain
// activity in the stats, not disappear into a separate, harder-to-notice
// counter. See RecordAudioDrain's doc for how that reads against
// RecordAudioUnderrun in the GUI.
func (s *tcpPCMSource) appendPCM(data []byte) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	s.buf = append(s.buf, data...)
	drained := 0

	if len(s.buf) > s.drainThresholdBytes {
		step := drainStepBytes
		if avail := len(s.buf) - s.targetBytes; step > avail {
			step = avail
		}
		step -= step % pcmBytesPerFrame
		if step > 0 {
			s.buf = s.buf[step:]
			drained += step
		}
	}

	if excess := len(s.buf) - s.hardCapBytes; excess > 0 {
		excess -= excess % pcmBytesPerFrame
		if excess > 0 {
			s.buf = s.buf[excess:]
			drained += excess
		}
	}

	bufLen := len(s.buf)
	s.mu.Unlock()

	stats := s.stats.Load()
	stats.RecordAudioBuffered(bufLen)
	if drained > 0 {
		stats.RecordAudioDrain(drained)
	}
}

// Read implements io.Reader for AudioCapture.ReadFrame. It always fills p
// completely and returns len(p), nil — except after Close, when it returns
// io.EOF like a closed pipe would. It waits up to readWaitTimeout for enough
// real PCM to satisfy the request in full; on timeout it returns whatever
// real PCM is available immediately, padded with silence, rather than
// blocking further or erroring. This is what guarantees audio capture never
// stalls video: with no client connected, every call simply costs
// readWaitTimeout and yields silence.
func (s *tcpPCMSource) Read(p []byte) (int, error) {
	need := len(p)
	deadline := time.Now().Add(readWaitTimeout)
	for {
		s.mu.Lock()
		if len(s.buf) >= need {
			n := copy(p, s.buf)
			s.buf = s.buf[n:]
			bufLen := len(s.buf)
			s.mu.Unlock()
			s.stats.Load().RecordAudioBuffered(bufLen)
			return n, nil
		}
		s.mu.Unlock()

		select {
		case <-s.doneCh:
			return 0, io.EOF
		default:
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(readPollInterval)
	}

	s.mu.Lock()
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	bufLen := len(s.buf)
	s.mu.Unlock()
	for i := n; i < need; i++ {
		p[i] = 0
	}

	stats := s.stats.Load()
	stats.RecordAudioBuffered(bufLen)
	if silence := need - n; silence > 0 {
		stats.RecordAudioUnderrun(silence)
	}
	return need, nil
}

// Close stops accepting connections, closes the current connection if any,
// and unblocks any pending Read (which returns io.EOF afterwards). Safe to
// call more than once and never blocks.
func (s *tcpPCMSource) Close() error {
	s.closeOnce.Do(func() {
		close(s.doneCh)
		s.listener.Close()
		s.connMu.Lock()
		if s.conn != nil {
			s.conn.Close()
			s.conn = nil
		}
		s.connMu.Unlock()
	})
	return nil
}

// DrainStale is inherited unmodified from audio.go's AudioCapture.DrainStale,
// which only acts when the PCM source implements SetReadDeadline.
// tcpPCMSource deliberately does not implement it, so DrainStale is a no-op
// on Windows — intentionally, not by omission. The Linux backend needs a
// one-shot drain because its OS pipe is an unbounded FIFO that keeps
// whatever backlog accumulated while audio waited for the first video frame.
// tcpPCMSource's buffer is continuously bounded (appendPCM, above) as data
// arrives, so by the time streaming begins, whatever is buffered is already
// within that same freshness bound; there is no unbounded startup backlog
// left to remove.

// sineWaveSource generates a continuous 44.1kHz stereo S16LE sine tone,
// standing in for the Linux backend's "audiotestsrc wave=sine" test source
// (see -test / daemon TestMode). It never blocks and never errors, so unlike
// tcpPCMSource it needs no silence fallback — every Read directly produces
// live samples.
type sineWaveSource struct {
	freq  float64
	phase float64
}

func newSineWaveSource(freq float64) *sineWaveSource {
	return &sineWaveSource{freq: freq}
}

func (s *sineWaveSource) Read(p []byte) (int, error) {
	frames := len(p) / pcmBytesPerFrame
	const twoPi = 2 * math.Pi
	step := twoPi * s.freq / pcmOutputRate
	for i := 0; i < frames; i++ {
		v := int16(math.Sin(s.phase) * 16000)
		off := i * pcmBytesPerFrame
		binary.LittleEndian.PutUint16(p[off:], uint16(v))
		binary.LittleEndian.PutUint16(p[off+2:], uint16(v))
		s.phase += step
		if s.phase > twoPi {
			s.phase -= twoPi
		}
	}
	return frames * pcmBytesPerFrame, nil
}

func (s *sineWaveSource) Close() error { return nil }

// linearResampler performs linear-interpolation sample-rate conversion on
// interleaved S16LE stereo PCM, carrying its fractional phase and last input
// sample across calls so a continuous input stream (delivered one TCP read
// at a time) resamples without a discontinuity at each call boundary. This
// is not a high-fidelity resampler (no anti-aliasing filter) — doubletake is
// a screen-mirroring sender, not an audio tool, and pulling in a proper
// resampling library would be the first cgo dependency in a cgo-free build.
// It is adequate for the ~9% rate change between the GUI's 48kHz WASAPI
// capture and AirPlay's fixed 44.1kHz mirrored-audio format.
type linearResampler struct {
	inRate, outRate int
	pos             float64 // fractional position; integer part already consumed
	prevL, prevR    int16
	havePrev        bool
}

func newLinearResampler(inRate, outRate int) *linearResampler {
	return &linearResampler{inRate: inRate, outRate: outRate}
}

// resample converts a whole number of interleaved S16LE stereo frames (in
// must be a multiple of 4 bytes) from inRate to outRate.
func (r *linearResampler) resample(in []byte) []byte {
	inFrames := len(in) / pcmBytesPerFrame
	if inFrames == 0 {
		return nil
	}
	ratio := float64(r.inRate) / float64(r.outRate)

	sample := func(i int) (int16, int16) {
		if i < 0 {
			if r.havePrev {
				return r.prevL, r.prevR
			}
			// First call ever: no history yet. Repeating sample 0 avoids a
			// fade-in click at the very start of the stream.
			return int16(binary.LittleEndian.Uint16(in[0:])), int16(binary.LittleEndian.Uint16(in[2:]))
		}
		off := i * pcmBytesPerFrame
		return int16(binary.LittleEndian.Uint16(in[off:])), int16(binary.LittleEndian.Uint16(in[off+2:]))
	}

	out := make([]byte, 0, int(float64(inFrames)/ratio)+pcmBytesPerFrame)
	for {
		idx := int(math.Floor(r.pos))
		if idx+1 >= inFrames {
			break
		}
		frac := r.pos - float64(idx)
		l0, rr0 := sample(idx)
		l1, rr1 := sample(idx + 1)
		outL := int16(float64(l0) + (float64(l1)-float64(l0))*frac)
		outR := int16(float64(rr0) + (float64(rr1)-float64(rr0))*frac)
		var frame [pcmBytesPerFrame]byte
		binary.LittleEndian.PutUint16(frame[0:2], uint16(outL))
		binary.LittleEndian.PutUint16(frame[2:4], uint16(outR))
		out = append(out, frame[:]...)
		r.pos += ratio
	}

	r.prevL, r.prevR = sample(inFrames - 1)
	r.havePrev = true
	r.pos -= float64(inFrames)
	return out
}
