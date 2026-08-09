package airplay

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
	"time"
)

// This file is the first test coverage capture_broadcast.go has ever had.
//
// -race is unavailable on windows/arm64, so nothing here relies on timing for
// correctness: synchronisation runs through io.Pipe semantics and channels,
// and timeouts appear only as failure detectors (never as a success
// condition). Run with -count=100 to shake out ordering flakes.

// broadcastFixture wires a BroadcastCapture to an in-memory source.
func broadcastFixture() (*BroadcastCapture, *io.PipeWriter) {
	src, pw := pipeCapture()
	return NewBroadcastCapture(src), pw
}

// readFullWithin reads exactly len(buf) bytes or fails the test.
func readFullWithin(t *testing.T, r io.Reader, buf []byte, limit time.Duration, what string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(r, buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(limit):
		t.Fatalf("%s: timed out after %v", what, limit)
	}
}

// TestBroadcastSinkChunksAreIndependent pins the aliasing fix. Run reuses one
// read buffer across iterations; without an explicit per-read copy, a sink
// that has not drained yet sees the *second* chunk's bytes where the first
// chunk should be.
//
// The reader deliberately starts only after both chunks have been written, so
// the failure is deterministic rather than timing-dependent.
func TestBroadcastSinkChunksAreIndependent(t *testing.T) {
	bc, pw := broadcastFixture()
	sink := bc.AddSink()
	go bc.Run()

	first := bytes.Repeat([]byte{0xAA}, 4096)
	second := bytes.Repeat([]byte{0xBB}, 4096)

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		pw.Write(first)
		pw.Write(second)
	}()
	<-writeDone

	got := make([]byte, len(first)+len(second))
	readFullWithin(t, sink, got, 5*time.Second, "reading both chunks")

	if !bytes.Equal(got[:len(first)], first) {
		t.Errorf("first chunk was clobbered by the next read: got %#x…, want %#x…", got[:8], first[:8])
	}
	if !bytes.Equal(got[len(first):], second) {
		t.Errorf("second chunk corrupted: got %#x…, want %#x…", got[len(first):len(first)+8], second[:8])
	}

	pw.Close()
}

// TestBroadcastSlowSinkDoesNotBlockFastSink is the head-of-line-blocking
// regression: one sink whose reader never drains must not stall the pump (and
// with it ffmpeg's stdout) for everyone else. Without the fix this test hangs.
func TestBroadcastSlowSinkDoesNotBlockFastSink(t *testing.T) {
	bc, pw := broadcastFixture()
	slow := bc.AddSink() // deliberately never read
	fast := bc.AddSink()
	go bc.Run()
	_ = slow

	const chunkSize = 64 * 1024
	const chunks = 128 // 8 MiB, comfortably past sinkQueueMaxBytes
	chunk := bytes.Repeat([]byte{0x5A}, chunkSize)

	go func() {
		for i := 0; i < chunks; i++ {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
			// Pace the producer a little. An unthrottled 8 MiB burst can
			// outrun *any* reader and push even the healthy sink past the
			// queue limit, which would make this test flaky for a reason
			// that has nothing to do with head-of-line blocking. A real
			// capture produces frames at the frame rate, not all at once.
			if i%8 == 7 {
				time.Sleep(2 * time.Millisecond)
			}
		}
		pw.Close()
	}()

	got := make([]byte, chunkSize*chunks)
	readFullWithin(t, fast, got, 20*time.Second, "fast sink receiving all bytes while a slow sink is stalled")

	for i := 0; i < len(got); i += chunkSize {
		if !bytes.Equal(got[i:i+chunkSize], chunk) {
			t.Fatalf("fast sink received corrupted data at offset %d", i)
		}
	}
}

// TestBroadcastSlowSinkClosedWithOverflowError: the stalled sink is dropped
// with a diagnosable error rather than silently losing bytes.
func TestBroadcastSlowSinkClosedWithOverflowError(t *testing.T) {
	bc, pw := broadcastFixture()
	slow := bc.AddSink()
	fast := bc.AddSink()
	go bc.Run()

	const chunkSize = 64 * 1024
	chunk := bytes.Repeat([]byte{0x27}, chunkSize)
	go func() {
		// Enough to blow past sinkQueueMaxBytes on the un-drained sink.
		for i := 0; i < 128; i++ {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
	}()

	// Drain the fast sink so the pump keeps moving.
	go io.Copy(io.Discard, fast)

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, chunkSize)
		for {
			if _, err := slow.Read(buf); err != nil {
				errCh <- err
				return
			}
			// Read just enough to stay alive but never keep up: stop reading
			// after the first chunk so the queue backs up.
			time.Sleep(50 * time.Millisecond)
		}
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrSinkOverflow) {
			t.Fatalf("slow sink closed with %v, want ErrSinkOverflow", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("slow sink was never dropped despite exceeding the queue limit")
	}

	bc.mu.Lock()
	for _, s := range bc.sinks {
		if s == slow {
			bc.mu.Unlock()
			t.Fatal("overflowed sink is still registered in the fan-out list")
		}
	}
	bc.mu.Unlock()
	pw.Close()
}

// TestBroadcastSinkNeverDeliversPartialChunk: an overflowed sink must be cut
// at a chunk boundary, never mid-chunk. A truncated NAL is undetectable
// downstream and decodes to garbage, which is why sinks are dropped wholesale
// instead of having bytes discarded (see ErrSinkOverflow).
func TestBroadcastSinkNeverDeliversPartialChunk(t *testing.T) {
	bc, pw := broadcastFixture()
	slow := bc.AddSink()
	go bc.Run()

	const chunkSize = 32 * 1024
	chunk := bytes.Repeat([]byte{0x11}, chunkSize)
	go func() {
		for i := 0; i < 512; i++ {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
	}()

	received := 0
	buf := make([]byte, chunkSize)
	deadline := time.After(20 * time.Second)
	for {
		type res struct {
			n   int
			err error
		}
		ch := make(chan res, 1)
		go func() {
			n, err := slow.Read(buf)
			ch <- res{n, err}
		}()
		select {
		case r := <-ch:
			received += r.n
			if r.err != nil {
				if received%chunkSize != 0 {
					t.Fatalf("sink delivered %d bytes, not a whole multiple of the %d-byte chunk size — a chunk was cut mid-way", received, chunkSize)
				}
				return
			}
		case <-deadline:
			t.Fatal("slow sink neither overflowed nor finished in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBroadcastSinkAsCaptureStopReturns is the regression for the waitCh that
// was created but never closed: Stop() waits 2s and then, with no cmd to
// kill, blocks forever on <-sc.waitCh.
func TestBroadcastSinkAsCaptureStopReturns(t *testing.T) {
	bc, pw := broadcastFixture()
	sink := bc.AddSink()
	go bc.Run()
	defer pw.Close()

	capture := sink.AsCapture()

	done := make(chan struct{})
	go func() {
		capture.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// t.Fatal rather than letting the whole suite hit its timeout, so
		// the failure names the actual problem.
		t.Fatal("ScreenCapture.Stop() on a broadcast sink never returned — waitCh is not being closed")
	}
}

// TestBroadcastSinkAsCaptureIsStable: the close path has to find the very
// capture the caller is holding, so AsCapture must memoize.
func TestBroadcastSinkAsCaptureIsStable(t *testing.T) {
	bc, pw := broadcastFixture()
	defer pw.Close()
	sink := bc.AddSink()

	if a, b := sink.AsCapture(), sink.AsCapture(); a != b {
		t.Fatalf("AsCapture returned two different captures (%p, %p); the sink could only ever signal one of their waitChs", a, b)
	}
}

// TestBroadcastFanoutDeliversIdenticalBytes: every sink sees the full stream,
// byte for byte.
func TestBroadcastFanoutDeliversIdenticalBytes(t *testing.T) {
	bc, pw := broadcastFixture()
	sinks := []*BroadcastSink{bc.AddSink(), bc.AddSink(), bc.AddSink()}
	go bc.Run()

	payload := make([]byte, 1<<20)
	rng := rand.New(rand.NewSource(1))
	rng.Read(payload)

	go func() {
		pw.Write(payload)
		pw.Close()
	}()

	for i, s := range sinks {
		got := make([]byte, len(payload))
		readFullWithin(t, s, got, 20*time.Second, "sink receiving full payload")
		if !bytes.Equal(got, payload) {
			t.Fatalf("sink %d received different bytes than were written", i)
		}
	}
}

// TestBroadcastRunKeepsDrainingWithoutSinks: with no sinks, Run must keep
// reading. Stopping would backpressure ffmpeg's stdout.
func TestBroadcastRunKeepsDrainingWithoutSinks(t *testing.T) {
	bc, pw := broadcastFixture()
	sink := bc.AddSink()
	go bc.Run()

	bc.RemoveSink(sink)

	written := make(chan error, 1)
	go func() {
		_, err := pw.Write(bytes.Repeat([]byte{0x33}, 256*1024))
		written <- err
	}()

	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("write to source failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("source write blocked with no sinks registered — Run stopped draining and would backpressure ffmpeg")
	}
	pw.Close()
}
