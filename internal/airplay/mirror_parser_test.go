package airplay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// annexB builds a start-code-prefixed NAL with the given type and payload
// length. Payload bytes are non-zero so they can never be mistaken for a start
// code by the parser.
func annexB(nalType byte, payloadLen int) []byte {
	out := []byte{0x00, 0x00, 0x00, 0x01, nalType & 0x1f}
	for i := 0; i < payloadLen; i++ {
		out = append(out, byte(0x40+i%0x30))
	}
	return out
}

// The parser must still hand back every complete NAL exactly as before; the
// idle-flush work is only about the trailing one.
func TestParserStillDelimitsCompleteNALs(t *testing.T) {
	p := newH264Parser()
	sps, pps, idr := annexB(7, 10), annexB(8, 4), annexB(5, 40)

	got := p.Push(concat(sps, pps, idr))

	// The trailing NAL is deliberately withheld — that is the behaviour the
	// idle flush exists to work around, not a bug to fix here.
	if len(got) != 2 {
		t.Fatalf("got %d NALs, want 2 (trailing one withheld)", len(got))
	}
	if !bytes.Equal(got[0], sps) {
		t.Errorf("NAL 0 mismatch")
	}
	if !bytes.Equal(got[1], pps) {
		t.Errorf("NAL 1 mismatch")
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// One quiet observation is not enough: at that point the buffer could equally
// be a write still in progress.
func TestFlushTailRequiresTwoStableObservations(t *testing.T) {
	p := newH264Parser()
	idr := annexB(5, 32)
	p.Push(concat(annexB(7, 8), idr))

	if got := p.FlushTail(); got != nil {
		t.Fatalf("first FlushTail returned %d bytes, want nil", len(got))
	}
	got := p.FlushTail()
	if got == nil {
		t.Fatal("second FlushTail returned nil, want the trailing NAL")
	}
	if !bytes.Equal(got, idr) {
		t.Errorf("flushed tail = % x, want % x", got, idr)
	}
	// Buffer is now drained.
	if got := p.FlushTail(); got != nil {
		t.Errorf("third FlushTail returned %d bytes, want nil", len(got))
	}
}

// The core safety property: a NAL that is still arriving must never be
// released, because a truncated NAL is not detectably bad — it decodes into
// garbage on the receiver.
func TestFlushTailNeverReleasesGrowingBuffer(t *testing.T) {
	p := newH264Parser()
	p.Push(concat(annexB(7, 8), []byte{0x00, 0x00, 0x00, 0x01, 0x65}))

	// Simulate a slow writer: a quiet tick, then more bytes, repeatedly.
	for i := 0; i < 5; i++ {
		if got := p.FlushTail(); got != nil {
			t.Fatalf("iteration %d: released %d bytes while the buffer was still growing", i, len(got))
		}
		p.Push([]byte{0x41, 0x42, 0x43, 0x44})
	}

	// Only once the writes stop do two quiet observations release it.
	p.FlushTail()
	if got := p.FlushTail(); got == nil {
		t.Fatal("tail never released after the writer went quiet")
	}
}

// Push must invalidate the stability evidence, otherwise a quiet tick followed
// by a write followed by another tick would look stable.
func TestPushResetsStabilityEvidence(t *testing.T) {
	p := newH264Parser()
	p.Push(concat(annexB(7, 8), annexB(5, 20)))

	p.FlushTail()        // first quiet observation
	p.Push([]byte{0x41}) // data arrives
	if got := p.FlushTail(); got != nil {
		t.Fatalf("released %d bytes after an intervening Push", len(got))
	}
}

// Length-prefixed AVCC input already knows where each NAL ends and never holds
// one back, so the tail flush must keep its hands off it.
func TestFlushTailIgnoresNonAnnexBBuffer(t *testing.T) {
	p := newH264Parser()
	// Length-prefixed, no start codes anywhere.
	p.Push([]byte{0x00, 0x00, 0x00, 0x08, 0x65, 0x41, 0x42, 0x43})

	for i := 0; i < 3; i++ {
		if got := p.FlushTail(); got != nil {
			t.Fatalf("released %d bytes of AVCC data", len(got))
		}
	}
}

// A buffer holding more than one NAL is not a tail — pushAnnexB would have
// delimited it. Releasing it wholesale would merge two NALs into one.
func TestFlushTailRejectsMultipleNALs(t *testing.T) {
	p := &h264Parser{idleLen: -1}
	p.buf = concat(annexB(1, 8), annexB(1, 8))

	if got := p.FlushTail(); got != nil {
		t.Fatalf("released %d bytes spanning two NALs", len(got))
	}
	if got := p.FlushTail(); got != nil {
		t.Fatalf("released %d bytes spanning two NALs on retry", len(got))
	}
}

// At end of stream nothing more can arrive, so the tail is releasable without
// the two-observation wait.
func TestFlushTailFinalReleasesImmediately(t *testing.T) {
	p := newH264Parser()
	idr := annexB(5, 24)
	p.Push(concat(annexB(7, 8), idr))

	got := p.FlushTailFinal()
	if got == nil {
		t.Fatal("FlushTailFinal returned nil")
	}
	if !bytes.Equal(got, idr) {
		t.Errorf("flushed tail = % x, want % x", got, idr)
	}
}

// FlushTailFinal is still bound by the same structural checks — it skips the
// wait, not the "is this actually one whole NAL" test.
func TestFlushTailFinalStillRejectsNonTail(t *testing.T) {
	p := &h264Parser{idleLen: -1}
	p.buf = []byte{0x41, 0x42, 0x43} // no start code at all

	if got := p.FlushTailFinal(); got != nil {
		t.Fatalf("released %d bytes with no start code", len(got))
	}
}

// --- StreamFrames integration ---

// pipeCapture builds a ScreenCapture fed by an in-process pipe, so a test can
// drive StreamFrames byte by byte and control when the input goes quiet.
func pipeCapture() (*ScreenCapture, *io.PipeWriter) {
	pr, pw := io.Pipe()
	return &ScreenCapture{stdout: pr, waitCh: make(chan struct{})}, pw
}

// The whole point of the idle flush: a frame reaches the socket without the
// next frame's bytes having to arrive first.
func TestStreamFramesSendsFrameWithoutNextFrameArriving(t *testing.T) {
	sender, receiver := net.Pipe()
	defer sender.Close()
	defer receiver.Close()

	session := &MirrorSession{
		dataConn:       sender,
		firstFrameSent: make(chan struct{}),
		stats:          newSessionStats(),
	}
	capture, pw := pipeCapture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- session.StreamFrames(ctx, capture, 0) }()

	// Drain whatever the session writes so it never blocks on the pipe.
	received := make(chan int, 8)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := receiver.Read(buf)
			if n > 0 {
				received <- n
			}
			if err != nil {
				return
			}
		}
	}()

	// One complete access unit: SPS, PPS, IDR. Nothing follows it.
	if _, err := pw.Write(concat(annexB(7, 12), annexB(8, 6), annexB(5, 64))); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Without the idle flush this would block until a second frame arrived.
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("no frame reached the socket; the trailing NAL was never released")
	}

	// The first thing on the wire is the unencrypted codec frame, which is sent
	// before the VCL frame and before RecordFrame runs — so wait for the frame
	// counter rather than for bytes, or this reads the stats too early.
	var snap StatsSnapshot
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if snap = session.Stats(); snap.FramesSent > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap.FramesSent == 0 {
		t.Fatal("no access unit was ever recorded as sent")
	}

	// AUHold has to span the idle wait, otherwise it cannot be used to compare
	// this code path against the old one: the whole delay being removed happens
	// between the bytes arriving and the tail being released, so a metric that
	// starts after the release would read ~0ms both before and after the fix
	// and make the change look like a no-op.
	if snap.AUHoldMsP50 < float64(idleFlushInterval/time.Millisecond) {
		t.Errorf("AUHoldMsP50 = %.1fms, want >= %v — the metric is not spanning the idle wait",
			snap.AUHoldMsP50, idleFlushInterval)
	}
	if snap.AUHoldMsP50 > 100 {
		t.Errorf("AUHoldMsP50 = %.1fms, implausibly high for a single idle flush", snap.AUHoldMsP50)
	}

	cancel()
	pw.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StreamFrames did not return after cancel")
	}
}

// Teardown is this project's highest-risk path: a reader goroutine left
// blocked on the capture would interfere with capture shutdown, and AirPlay
// requires a clean RTSP TEARDOWN or the receiver refuses to reconnect.
func TestStreamFramesReleasesReaderGoroutineOnCancel(t *testing.T) {
	// Assert on the reader goroutine itself rather than runtime.NumGoroutine():
	// the count is process-global, so stragglers from neighbouring tests (and
	// this test's own pipe-drain goroutine) would sit inside any tolerance
	// wide enough to be non-flaky — a genuinely leaked reader could hide there.
	exitedCh := make(chan (<-chan struct{}), 1)
	streamReaderExited = func(c <-chan struct{}) { exitedCh <- c }
	defer func() { streamReaderExited = nil }()

	sender, receiver := net.Pipe()
	defer sender.Close()
	defer receiver.Close()
	go io.Copy(io.Discard, receiver)

	session := &MirrorSession{
		dataConn:       sender,
		firstFrameSent: make(chan struct{}),
		stats:          newSessionStats(),
	}
	capture, pw := pipeCapture()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- session.StreamFrames(ctx, capture, 0) }()

	pw.Write(annexB(7, 8))
	time.Sleep(50 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StreamFrames returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamFrames did not return after cancel")
	}

	var readerExited <-chan struct{}
	select {
	case readerExited = <-exitedCh:
	default:
		t.Fatal("StreamFrames never published its reader-exit channel")
	}

	// The reader is still parked in Read at this point — cancelling the context
	// cannot interrupt a blocking read. It exits once the capture is closed,
	// which is exactly what session teardown does (the daemon closes the
	// broadcast sink, the direct path closes the ffmpeg pipe).
	select {
	case <-readerExited:
		t.Fatal("reader exited before the capture was closed; the test proves nothing")
	case <-time.After(100 * time.Millisecond):
	}

	pw.Close()

	select {
	case <-readerExited:
	case <-time.After(2 * time.Second):
		t.Fatal("reader goroutine leaked: still running after the capture was closed")
	}
}

// EOF must not swallow the access unit that was still buffered.
func TestStreamFramesFlushesTailOnEOF(t *testing.T) {
	sender, receiver := net.Pipe()
	defer sender.Close()
	defer receiver.Close()

	session := &MirrorSession{
		dataConn:       sender,
		firstFrameSent: make(chan struct{}),
		stats:          newSessionStats(),
	}
	capture, pw := pipeCapture()

	received := make(chan int, 8)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := receiver.Read(buf)
			if n > 0 {
				received <- n
			}
			if err != nil {
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- session.StreamFrames(context.Background(), capture, 0) }()

	pw.Write(concat(annexB(7, 12), annexB(8, 6), annexB(5, 48)))
	pw.Close()

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("buffered access unit was lost at EOF")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StreamFrames did not return after EOF")
	}
}
