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

	ac, err := StartAudioCapture(ctx, false, 17655)
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

	ac, err := StartAudioCapture(ctx, false, 17656)
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

	ac, err := StartAudioCapture(ctx, false, 17657)
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

	ac, err := StartAudioCapture(ctx, false, 17658)
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
	if bufLen > maxBufferedPCM {
		t.Fatalf("buffered PCM = %d bytes, want <= maxBufferedPCM = %d bytes", bufLen, maxBufferedPCM)
	}
	t.Logf("after flooding 5x1s of 48kHz PCM with no consumer, buffer settled at %d bytes (cap %d)", bufLen, maxBufferedPCM)
}

var _ io.Reader = (*tcpPCMSource)(nil)
