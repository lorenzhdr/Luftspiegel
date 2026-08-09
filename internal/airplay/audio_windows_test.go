//go:build windows

package airplay

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// sendPCM dials addr and writes n stereo s16le frames of a nonzero sine
// tone, split into chunks to resemble a real streaming client rather than
// one giant write.
func sendPCM(t *testing.T, addr string, frames int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	buf := make([]byte, 4)
	for i := 0; i < frames; i++ {
		v := int16(10000) // constant nonzero sample, trivially distinguishable from silence
		binary.LittleEndian.PutUint16(buf[0:], uint16(v))
		binary.LittleEndian.PutUint16(buf[2:], uint16(v))
		if _, err := conn.Write(buf); err != nil {
			t.Fatalf("write pcm: %v", err)
		}
	}
	return conn
}

// TestWindowsAudioSilenceWithoutClient verifies the core non-blocking
// guarantee: with no client ever connected, ReadFrame must still return
// promptly (bounded by readWaitTimeout, not indefinitely) and the underlying
// PCM must be silence.
func TestWindowsAudioSilenceWithoutClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ac, err := StartAudioCapture(ctx, false, 17655, 0)
	if err != nil {
		t.Fatalf("StartAudioCapture: %v", err)
	}
	defer ac.Stop()

	src, ok := ac.pcmPipe.(*tcpPCMSource)
	if !ok {
		t.Fatalf("pcmPipe is %T, want *tcpPCMSource", ac.pcmPipe)
	}

	pcm := make([]byte, 1408) // one ALAC frame's worth: 352 samples * 2ch * 2bytes
	start := time.Now()
	n, err := src.Read(pcm)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Read with no client: %v", err)
	}
	if n != len(pcm) {
		t.Fatalf("Read returned %d bytes, want %d (a short read would stall ReadFrame's io.ReadFull)", n, len(pcm))
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("Read took %v with no client connected; want it bounded (~readWaitTimeout=%v), not a long/indefinite block", elapsed, readWaitTimeout)
	}
	for i, b := range pcm {
		if b != 0 {
			t.Fatalf("pcm[%d] = %d, want silence (all zero) with no client connected", i, b)
		}
	}
	t.Logf("no-client Read took %v (bound: readWaitTimeout=%v), returned %d bytes of silence", elapsed, readWaitTimeout, n)

	// The AudioCapture-level entry point must produce a plausible ALAC frame
	// too, not just the raw PCM source.
	frameBuf := make([]byte, 8192)
	fn, err := ac.ReadFrame(frameBuf)
	if err != nil {
		t.Fatalf("ReadFrame with no client: %v", err)
	}
	if fn <= 0 {
		t.Fatalf("ReadFrame returned %d bytes, want > 0", fn)
	}
	t.Logf("ReadFrame with no client produced a %d-byte ALAC frame", fn)
}

// TestWindowsAudioConnectProducesFrames verifies that once a client streams
// PCM, ReadFrame yields stably-sized, non-empty ALAC frames.
func TestWindowsAudioConnectProducesFrames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ac, err := StartAudioCapture(ctx, false, 17656, 0)
	if err != nil {
		t.Fatalf("StartAudioCapture: %v", err)
	}
	defer ac.Stop()

	conn := sendPCM(t, "127.0.0.1:17656", 48000*2) // ~2 seconds of 48kHz audio, streamed in the background
	go func() {
		defer conn.Close()
		buf := make([]byte, 4)
		for {
			v := int16(10000)
			binary.LittleEndian.PutUint16(buf[0:], uint16(v))
			binary.LittleEndian.PutUint16(buf[2:], uint16(v))
			if _, err := conn.Write(buf); err != nil {
				return
			}
			time.Sleep(time.Microsecond * 20) // ~48kHz pacing
		}
	}()

	// Give the connection a moment to be accepted and start filling the buffer.
	time.Sleep(100 * time.Millisecond)

	frameBuf := make([]byte, 8192)
	var sizes []int
	for i := 0; i < 10; i++ {
		n, err := ac.ReadFrame(frameBuf)
		if err != nil {
			t.Fatalf("ReadFrame #%d: %v", i, err)
		}
		if n <= 0 {
			t.Fatalf("ReadFrame #%d returned %d bytes, want > 0", i, n)
		}
		sizes = append(sizes, n)
	}
	for i, n := range sizes {
		if n != sizes[0] {
			t.Fatalf("frame size unstable: frame 0 = %d bytes, frame %d = %d bytes", sizes[0], i, n)
		}
	}
	t.Logf("10 ALAC frames read, stable size = %d bytes", sizes[0])
}

// TestWindowsAudioReconnect verifies that a client disconnecting and a new
// one connecting does not break the capture: ReadFrame must keep working
// (falling back to silence during the gap) across the transition.
func TestWindowsAudioReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ac, err := StartAudioCapture(ctx, false, 17657, 0)
	if err != nil {
		t.Fatalf("StartAudioCapture: %v", err)
	}
	defer ac.Stop()

	conn1 := sendPCM(t, "127.0.0.1:17657", 4800) // 100ms
	time.Sleep(50 * time.Millisecond)
	conn1.Close()
	time.Sleep(50 * time.Millisecond)

	frameBuf := make([]byte, 8192)
	// During the gap, ReadFrame must still succeed (silence), not error.
	if _, err := ac.ReadFrame(frameBuf); err != nil {
		t.Fatalf("ReadFrame during reconnect gap: %v", err)
	}

	conn2 := sendPCM(t, "127.0.0.1:17657", 48000)
	defer conn2.Close()
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 5; i++ {
		if _, err := ac.ReadFrame(frameBuf); err != nil {
			t.Fatalf("ReadFrame after reconnect #%d: %v", i, err)
		}
	}
	t.Log("capture survived client disconnect + reconnect")
}

// TestWindowsAudioBufferBounded verifies the drift-control requirement: a
// client that produces PCM much faster than ReadFrame consumes it must not
// make the internal buffer grow without bound.
func TestWindowsAudioBufferBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ac, err := StartAudioCapture(ctx, false, 17658, 0)
	if err != nil {
		t.Fatalf("StartAudioCapture: %v", err)
	}
	defer ac.Stop()

	src := ac.pcmPipe.(*tcpPCMSource)

	conn, err := net.Dial("tcp", "127.0.0.1:17658")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Flood far more PCM than ReadFrame will ever drain during this test,
	// with no consumer running concurrently, to exercise the drop-oldest path.
	chunk := make([]byte, 48000*4) // 1 second of 48kHz stereo s16le
	for i := 0; i < 5; i++ {
		if _, err := conn.Write(chunk); err != nil {
			t.Fatalf("write chunk %d: %v", i, err)
		}
	}

	// Let the read loop drain the socket into the internal buffer.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		src.mu.Lock()
		n := len(src.buf)
		src.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	src.mu.Lock()
	bufLen := len(src.buf)
	src.mu.Unlock()
	// hardCapBytes (not maxBufferedPCM, which no longer exists — see
	// appendPCM) is the actual configured ceiling now: bufferMs=0 here
	// resolves to defaultPCMBufferMs, so this is hardCapPct% of that.
	if bufLen > src.hardCapBytes {
		t.Fatalf("buffered PCM = %d bytes, want <= hardCapBytes = %d bytes", bufLen, src.hardCapBytes)
	}
	t.Logf("after flooding 5x1s of 48kHz PCM with no consumer, buffer settled at %d bytes (hard cap %d)", bufLen, src.hardCapBytes)
}

// TestWindowsAudioBufferSettlesNearTarget is the core regression test for the
// bug this file exists to fix: a backlog built up by a burst must ease back
// down toward the configured target over subsequent appends, not stay parked
// at hardCapBytes forever. See appendPCM's doc comment for the two-step
// drain/backstop design this exercises.
func TestWindowsAudioBufferSettlesNearTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// An explicit, readable target rather than the default, so the expected
	// byte thresholds below are easy to sanity-check by hand.
	ac, err := StartAudioCapture(ctx, false, 17663, 100)
	if err != nil {
		t.Fatalf("StartAudioCapture: %v", err)
	}
	defer ac.Stop()

	src := ac.pcmPipe.(*tcpPCMSource)

	// Simulate a startup burst far beyond every threshold: appendPCM's
	// emergency backstop must clip it straight to hardCapBytes in this one
	// call (this is also the "drain counts a hard-cap overflow" case that
	// TestWindowsAudioDrainCounted checks against the stats side).
	src.appendPCM(make([]byte, src.hardCapBytes*3))
	src.mu.Lock()
	afterBurst := len(src.buf)
	src.mu.Unlock()
	if afterBurst != src.hardCapBytes {
		t.Fatalf("buffer after burst = %d bytes, want exactly hardCapBytes = %d", afterBurst, src.hardCapBytes)
	}

	// Feed small chunks with nothing else draining the buffer (no Read
	// running concurrently). Each chunk is far smaller than drainStepBytes,
	// so as long as the level stays above drainThresholdBytes the
	// incremental drain removes more per append than production adds,
	// pulling the level down instead of leaving it pinned near the cap.
	chunk := make([]byte, 200) // 50 stereo frames; well under drainStepBytes
	var levels []int
	for i := 0; i < 500; i++ {
		src.appendPCM(chunk)
		src.mu.Lock()
		levels = append(levels, len(src.buf))
		src.mu.Unlock()
	}

	final := levels[len(levels)-1]
	if final >= afterBurst {
		t.Fatalf("buffer did not shrink from the post-burst level: after burst=%d, after settling=%d", afterBurst, final)
	}
	if final > src.drainThresholdBytes {
		t.Fatalf("buffer settled at %d bytes, want <= drainThresholdBytes=%d (still above the drain trigger)", final, src.drainThresholdBytes)
	}
	if final >= src.hardCapBytes {
		t.Fatalf("buffer settled at %d bytes, still pinned at hardCapBytes=%d — the bug this test guards against", final, src.hardCapBytes)
	}
	t.Logf("buffer settled at %d bytes (target=%d threshold=%d hardCap=%d) after %d appends",
		final, src.targetBytes, src.drainThresholdBytes, src.hardCapBytes, len(levels))
}

// TestWindowsAudioBufferNoOscillation checks that the settling exercised by
// TestWindowsAudioBufferSettlesNearTarget is smooth rather than a sawtooth
// between drainThresholdBytes and hardCapBytes: once the level first reaches
// drainThresholdBytes or below, it must not later swing back up toward the
// cap — it should stay within roughly one drain step of the threshold.
func TestWindowsAudioBufferNoOscillation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ac, err := StartAudioCapture(ctx, false, 17664, 100)
	if err != nil {
		t.Fatalf("StartAudioCapture: %v", err)
	}
	defer ac.Stop()

	src := ac.pcmPipe.(*tcpPCMSource)
	src.appendPCM(make([]byte, src.hardCapBytes*3))

	chunk := make([]byte, 200)
	var levels []int
	settledAt := -1
	for i := 0; i < 500; i++ {
		src.appendPCM(chunk)
		src.mu.Lock()
		n := len(src.buf)
		src.mu.Unlock()
		levels = append(levels, n)
		if settledAt < 0 && n <= src.drainThresholdBytes {
			settledAt = i
		}
	}
	if settledAt < 0 {
		t.Fatalf("buffer never reached drainThresholdBytes=%d within %d appends", src.drainThresholdBytes, len(levels))
	}

	// One drain step of margin absorbs the normal threshold-crossing
	// overshoot (chunk added, then drained back down) that happens on every
	// cycle near the threshold; anything past that means the level bounced
	// back up toward hardCapBytes instead of staying settled.
	margin := src.drainThresholdBytes + drainStepBytes
	for i := settledAt; i < len(levels); i++ {
		if levels[i] > margin {
			t.Fatalf("buffer at append %d = %d bytes, want <= %d (threshold+one drain step) after first settling at append %d — looks like oscillation back toward hardCapBytes=%d",
				i, levels[i], margin, settledAt, src.hardCapBytes)
		}
	}
	t.Logf("buffer first settled at append %d and stayed within one drain step of drainThresholdBytes=%d for the remaining %d appends",
		settledAt, src.drainThresholdBytes, len(levels)-settledAt)
}

// TestWindowsAudioUnderrunCounted verifies RecordAudioUnderrun fires (with a
// nonzero silence-byte count) when Read has to pad its result because no
// client is connected — the same condition TestWindowsAudioSilenceWithoutClient
// exercises, but checked against the stats side this time.
func TestWindowsAudioUnderrunCounted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ac, err := StartAudioCapture(ctx, false, 17665, 0)
	if err != nil {
		t.Fatalf("StartAudioCapture: %v", err)
	}
	defer ac.Stop()

	stats := newSessionStats()
	ac.SetStats(stats)

	pcm := make([]byte, 1408) // one ALAC frame's worth
	if _, err := ac.pcmPipe.Read(pcm); err != nil {
		t.Fatalf("Read with no client: %v", err)
	}

	snap := stats.Snapshot()
	if snap.AudioUnderruns != 1 {
		t.Fatalf("AudioUnderruns = %d, want 1", snap.AudioUnderruns)
	}
	if snap.AudioSilenceMs <= 0 {
		t.Fatalf("AudioSilenceMs = %v, want > 0 after a fully-silent read", snap.AudioSilenceMs)
	}
	t.Logf("one silent read recorded AudioUnderruns=%d AudioSilenceMs=%.2f", snap.AudioUnderruns, snap.AudioSilenceMs)
}

// TestWindowsAudioDrainCounted verifies RecordAudioDrain fires when an
// oversized append triggers the hard-cap backstop — the case
// appendPCM's doc comment calls out explicitly: a hard-cap overflow must
// count as drain too, not vanish into a separate/invisible path.
func TestWindowsAudioDrainCounted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ac, err := StartAudioCapture(ctx, false, 17666, 40) // small, min-clamped target for a small/fast overflow
	if err != nil {
		t.Fatalf("StartAudioCapture: %v", err)
	}
	defer ac.Stop()

	src := ac.pcmPipe.(*tcpPCMSource)
	stats := newSessionStats()
	ac.SetStats(stats)

	src.appendPCM(make([]byte, src.hardCapBytes*2))

	snap := stats.Snapshot()
	if snap.AudioDrainMs <= 0 {
		t.Fatalf("AudioDrainMs = %v, want > 0 after an oversized append", snap.AudioDrainMs)
	}

	src.mu.Lock()
	bufLen := len(src.buf)
	src.mu.Unlock()
	if bufLen != src.hardCapBytes {
		t.Fatalf("buffer after oversized append = %d bytes, want exactly hardCapBytes = %d", bufLen, src.hardCapBytes)
	}
	t.Logf("oversized append recorded AudioDrainMs=%.2f and clipped buffer to hardCapBytes=%d", snap.AudioDrainMs, src.hardCapBytes)
}

// TestWindowsAudioBufferMsClamping verifies StartAudioCapture's bufferMs
// clamping: 0 substitutes the default, and out-of-range values clamp to
// min/max rather than being used verbatim.
func TestWindowsAudioBufferMsClamping(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero selects default", 0, defaultPCMBufferMs},
		{"below min clamps up", 10, minPCMBufferMs},
		{"far above max clamps down", 9999, maxPCMBufferMs},
		{"min passes through", minPCMBufferMs, minPCMBufferMs},
		{"max passes through", maxPCMBufferMs, maxPCMBufferMs},
		{"mid-range passes through", 150, 150},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolvePCMBufferMs(c.in); got != c.want {
				t.Errorf("resolvePCMBufferMs(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

var _ io.Reader = (*tcpPCMSource)(nil)
