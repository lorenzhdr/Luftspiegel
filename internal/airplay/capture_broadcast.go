package airplay

import (
	"errors"
	"io"
	"log"
	"sync"
)

// BroadcastCapture reads from a single ScreenCapture and fans the raw byte
// stream out to multiple registered sinks. Each sink is a pipe-based reader
// that can be passed to MirrorSession.StreamFrames just like a ScreenCapture.
//
// Usage:
//
//	bc := NewBroadcastCapture(underlying)
//	sink1 := bc.AddSink()
//	sink2 := bc.AddSink()
//	go bc.Run()          // pumps bytes from the underlying capture
//	go session1.StreamFrames(ctx, sink1.AsCapture(), 0)
//	go session2.StreamFrames(ctx, sink2.AsCapture(), 0)
type BroadcastCapture struct {
	src  *ScreenCapture
	mu   sync.Mutex
	done chan struct{}
	err  error // set once Run() exits

	sinks []*BroadcastSink
}

// sinkQueueMaxBytes and sinkQueueMaxChunks bound how far a single fan-out
// sink's outgoing queue (see BroadcastSink.enqueue) may grow before the sink
// is dropped outright. 4 MiB is roughly 7s of buffering at the default
// 4500 kbps video bitrate, or ~2.7s at the 12000 kbps ceiling (see
// maxVideoBitrateKbps in capture.go). sinkQueueMaxChunks is a second, chunk-
// count bound so many small reads can't inflate the queue without tripping
// the byte limit.
const (
	sinkQueueMaxBytes  = 4 << 20
	sinkQueueMaxChunks = 512
)

// ErrSinkOverflow is the error a BroadcastSink is closed with (and that its
// Read() then returns) once its outgoing queue exceeds sinkQueueMaxBytes or
// sinkQueueMaxChunks because its reader isn't keeping up.
//
// Sinks are dropped outright rather than having individual bytes or chunks
// discarded, deliberately:
//   - A sink only ever sees ffmpeg's own read-sized chunks, not NAL-unit
//     boundaries — that parsing lives in mirror.go's h264Parser, which
//     itself withholds the tail NAL until a later read proves it complete.
//     Any byte-level drop here is therefore guaranteed to hand the receiver
//     a truncated NAL, and a truncated NAL is not detectable as broken; it
//     just decodes to garbage.
//   - Even with NAL-aware parsing, dropping would still be unsafe:
//     mirror.go's streamPrimed latches true after the first IDR with no way
//     for a sink to un-latch it, so after a gap the decoder would keep
//     running with a missing reference — up to a full GOP (4s default) of
//     corrupted video.
//   - A sink that is multiple seconds behind is effectively a dead
//     receiver already. Closing it drives its reader out of Read with an
//     error, which triggers the normal MirrorSession teardown (including
//     RTSP TEARDOWN) — exactly what should happen to a stalled session.
var ErrSinkOverflow = errors.New("broadcast sink exceeded queue limit and was closed")

// BroadcastSink is a reader end of a BroadcastCapture. It satisfies the same
// Read interface as ScreenCapture and can be wrapped into a ScreenCapture-like
// value via AsCapture().
type BroadcastSink struct {
	pr *io.PipeReader
	pw *io.PipeWriter

	mu   sync.Mutex
	cond *sync.Cond

	// queue holds chunks accepted by enqueue but not yet handed to pw by
	// writeLoop. Decoupling accept-from-source (enqueue, called by Run) from
	// deliver-to-reader (writeLoop, its own goroutine) is what stops one
	// slow reader from blocking the whole fan-out pump; see the comments on
	// enqueue and writeLoop.
	queue      [][]byte
	queueBytes int

	closed      bool
	closeReason error // nil means a plain EOF close

	// flushOnClose distinguishes the two ways a sink ends. On a graceful end
	// of stream (the source hit EOF, see Run's defer) whatever is still
	// queued is genuinely the tail of the video and must still be delivered.
	// On an abandoning close (overflow, or the daemon tearing the session
	// down) the queue is dropped immediately — nobody is going to read it.
	flushOnClose bool

	// capture memoizes the *ScreenCapture returned by AsCapture, so that (a)
	// repeated AsCapture calls return the same value, and (b) this sink's
	// close path can find that exact capture's waitCh to close it. See
	// AsCapture and signalCaptureDone.
	capture           *ScreenCapture
	captureWaitClosed bool
}

// NewBroadcastCapture wraps src. Call AddSink before calling Run.
func NewBroadcastCapture(src *ScreenCapture) *BroadcastCapture {
	return &BroadcastCapture{
		src:  src,
		done: make(chan struct{}),
	}
}

// AddSink registers a new fan-out reader. Must be called before Run.
func (bc *BroadcastCapture) AddSink() *BroadcastSink {
	s := newBroadcastSink()
	bc.mu.Lock()
	bc.sinks = append(bc.sinks, s)
	bc.mu.Unlock()
	return s
}

// RemoveSink closes and removes a sink so it no longer receives data.
// Safe to call concurrently with Run.
func (bc *BroadcastCapture) RemoveSink(s *BroadcastSink) {
	s.close()
	bc.mu.Lock()
	defer bc.mu.Unlock()
	for i, ss := range bc.sinks {
		if ss == s {
			bc.sinks = append(bc.sinks[:i], bc.sinks[i+1:]...)
			return
		}
	}
}

// Run pumps data from the underlying ScreenCapture to all registered sinks.
// It returns only when the underlying capture itself ends (EOF or error) —
// having zero registered sinks does NOT stop the pump. Run keeps draining
// the source in that case so ffmpeg is never backpressured, even while no
// session is currently receiving frames (see the empty-sinks branch below).
// The caller should run this in a dedicated goroutine.
func (bc *BroadcastCapture) Run() error {
	buf := make([]byte, 256*1024)
	defer func() {
		bc.mu.Lock()
		sinks := make([]*BroadcastSink, len(bc.sinks))
		copy(sinks, bc.sinks)
		bc.mu.Unlock()

		// Propagate the real reason Run stopped (e.g. a genuine capture
		// failure, not just EOF) to any sink whose AsCapture() a caller is
		// blocked in Read/Stop on. A plain io.EOF is passed through as nil
		// so ScreenCapture.Read reports a plain io.EOF rather than wrapping
		// it as "capture exited: EOF".
		reason := bc.err
		if reason == io.EOF {
			reason = nil
		}
		for _, s := range sinks {
			// Graceful: the source is done, but chunks already queued are
			// the tail of the stream and still have to reach the receiver.
			s.closeGraceful(reason)
		}
		close(bc.done)
	}()

	for {
		n, err := bc.src.Read(buf)
		if n > 0 {
			// Each sink gets its own independent copy of the bytes read
			// this iteration. buf is reused across iterations (the next
			// Read overwrites it), and that used to be safe only because
			// sinks were written to synchronously. Now that sinks queue
			// chunks instead (see enqueue/writeLoop), a slow sink can hold
			// a reference to this chunk well past the point where the next
			// Read would otherwise clobber it through the shared backing
			// array — so the copy is required, not an optimization.
			//
			// Deliberately no sync.Pool here: recycling the copy would need
			// refcounting across every sink that still holds a reference to
			// it (each sink drains at its own pace), and nothing here
			// tracks that. A pool without refcounting would let one sink's
			// writeLoop hand a still-referenced buffer back to the pool
			// while another sink is still reading it.
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			bc.mu.Lock()
			sinks := make([]*BroadcastSink, len(bc.sinks))
			copy(sinks, bc.sinks)
			bc.mu.Unlock()

			if len(sinks) == 0 {
				// No active sinks; keep draining to avoid backpressuring
				// the capture (and thus ffmpeg).
				if err != nil {
					bc.err = err
					return err
				}
				continue
			}

			for _, s := range sinks {
				if writeErr := s.enqueue(chunk); writeErr != nil {
					if errors.Is(writeErr, ErrSinkOverflow) {
						log.Printf("airplay: broadcast sink fell more than %d bytes/%d chunks behind, dropping it", sinkQueueMaxBytes, sinkQueueMaxChunks)
					}
					bc.RemoveSink(s)
				}
			}
		}
		if err != nil {
			bc.err = err
			return err
		}
	}
}

// Done returns a channel that is closed when Run has finished.
func (bc *BroadcastCapture) Done() <-chan struct{} {
	return bc.done
}

// Err returns the error that caused Run to exit (nil if still running).
func (bc *BroadcastCapture) Err() error {
	select {
	case <-bc.done:
		return bc.err
	default:
		return nil
	}
}

// Source returns the underlying ScreenCapture.
func (bc *BroadcastCapture) Source() *ScreenCapture {
	return bc.src
}

// --- BroadcastSink ---

func newBroadcastSink() *BroadcastSink {
	pr, pw := io.Pipe()
	s := &BroadcastSink{pr: pr, pw: pw}
	s.cond = sync.NewCond(&s.mu)
	go s.writeLoop()
	return s
}

// enqueue appends chunk to this sink's outgoing queue without blocking on
// the underlying pipe — Run calls this once per sink per source read, and
// must never block on a single slow sink (that would stall the whole
// fan-out pump and, transitively, ffmpeg's stdout). If accepting chunk would
// push the queue past sinkQueueMaxBytes or sinkQueueMaxChunks, the sink is
// instead closed with ErrSinkOverflow and that error is returned so Run can
// drop it from the fan-out list. See ErrSinkOverflow for why the sink is
// dropped wholesale instead of individual chunks being discarded.
func (s *BroadcastSink) enqueue(chunk []byte) error {
	s.mu.Lock()
	if s.closed {
		reason := s.closeReason
		s.mu.Unlock()
		if reason == nil {
			reason = io.ErrClosedPipe
		}
		return reason
	}

	newBytes := s.queueBytes + len(chunk)
	newChunks := len(s.queue) + 1
	if newBytes > sinkQueueMaxBytes || newChunks > sinkQueueMaxChunks {
		s.mu.Unlock()
		s.closeErr(ErrSinkOverflow)
		return ErrSinkOverflow
	}

	s.queue = append(s.queue, chunk)
	s.queueBytes = newBytes
	s.cond.Signal()
	s.mu.Unlock()
	return nil
}

// writeLoop drains this sink's queue into the underlying pipe on its own
// goroutine, one sink-lifetime per goroutine. Blocking here — pw.Write waits
// for a reader — is expected and harmless: it stalls only this goroutine,
// never BroadcastCapture.Run's shared pump. That decoupling is the entire
// point of the queue; see enqueue and ErrSinkOverflow for what happens when
// a reader falls behind faster than this loop can drain into it.
func (s *BroadcastSink) writeLoop() {
	reason := s.drainLoop()

	// Closing the WRITE half is what makes the reader observe reason: a
	// PipeReader.CloseWithError would instead make every Read return
	// io.ErrClosedPipe and mask it entirely (verified against io.Pipe).
	s.pw.CloseWithError(errOrEOF(reason))

	// Only now may the synthetic capture be marked done: ScreenCapture.Read
	// short-circuits to EOF as soon as waitCh is closed, so signalling it
	// earlier would truncate the flush above.
	s.signalCaptureDone(reason)
}

// drainLoop delivers queued chunks until the sink is closed (and, on a
// graceful close, until the queue is empty). Returns the close reason.
func (s *BroadcastSink) drainLoop() error {
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closed {
			s.cond.Wait()
		}
		if len(s.queue) == 0 || (s.closed && !s.flushOnClose) {
			reason := s.closeReason
			s.queue = nil
			s.queueBytes = 0
			s.mu.Unlock()
			return reason
		}
		// Take the whole current backlog in one critical section instead of
		// re-locking per chunk.
		batch := s.queue
		s.queue = nil
		s.queueBytes = 0
		s.mu.Unlock()

		for _, chunk := range batch {
			if _, err := s.pw.Write(chunk); err != nil {
				// Reader gone / pipe closed out from under this write.
				s.mu.Lock()
				reason := s.closeReason
				s.mu.Unlock()
				return reason
			}
		}
	}
}

func errOrEOF(err error) error {
	if err == nil {
		return io.EOF
	}
	return err
}

// closeErr closes the sink with the given reason (nil means a plain EOF
// close, e.g. normal teardown). Safe to call multiple times and
// concurrently with itself and with enqueue/writeLoop/AsCapture; only the
// first call has any effect. Also propagates the reason to the
// *ScreenCapture returned by AsCapture, if one was ever created — see
// signalCaptureDone.
func (s *BroadcastSink) closeErr(err error) {
	s.closeSink(err, false)
}

// closeSink marks the sink closed. flush=true lets writeLoop deliver what is
// still queued before the reader sees the close (used for a graceful
// end-of-stream); flush=false drops the queue at once.
//
// Neither variant touches s.pr: closing the READ half would make every
// subsequent Read return io.ErrClosedPipe and hide the actual reason
// (ErrSinkOverflow, or a genuine capture error). The reason is delivered by
// closing the write half in writeLoop.
func (s *BroadcastSink) closeSink(err error, flush bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.closeReason = err
	s.flushOnClose = flush
	s.cond.Broadcast() // wake writeLoop so it can observe closed
	s.mu.Unlock()

	if !flush {
		// Unblock a writeLoop that is parked in pw.Write because nobody is
		// reading; it then exits and closes the pipe with err.
		s.pw.CloseWithError(errOrEOF(err))
		s.signalCaptureDone(err)
	}
}

// closeGraceful ends the sink at a normal end of stream, delivering whatever
// is still queued first. reason is the error that ended the source (nil for a
// plain EOF).
func (s *BroadcastSink) closeGraceful(reason error) {
	s.closeSink(reason, true)
}

func (s *BroadcastSink) close() {
	s.closeErr(nil)
}

// signalCaptureDone closes the waitCh of the ScreenCapture returned by
// AsCapture (if any), exactly once. Without this, ScreenCapture.Stop()
// would block on <-sc.waitCh for its full 2s timeout and then hang forever
// in the kill fallback, because a BroadcastSink-backed capture has no cmd to
// kill (see AsCapture).
//
// waitErr is set before the channel closes; the close of the channel is
// what establishes happens-before for any goroutine that later reads
// waitErr via <-waitCh (mirrors the cmd.Wait()/close(waitCh) ordering in
// capture_linux.go).
func (s *BroadcastSink) signalCaptureDone(reason error) {
	s.mu.Lock()
	if s.capture == nil || s.captureWaitClosed {
		s.mu.Unlock()
		return
	}
	s.captureWaitClosed = true
	c := s.capture
	s.mu.Unlock()

	c.waitErr = reason
	close(c.waitCh)
}

// Read implements io.Reader — reads broadcast data. Blocks until data arrives
// or the broadcast ends.
func (s *BroadcastSink) Read(p []byte) (int, error) {
	return s.pr.Read(p)
}

// AsCapture wraps this sink in a synthetic ScreenCapture so it can be passed
// directly to MirrorSession.StreamFrames. The result is memoized: repeated
// calls return the same *ScreenCapture. That matters because Stop() on the
// returned capture (via extraClose) closes this sink, and closing this sink
// must be able to find and close that same capture's waitCh (see
// signalCaptureDone) — two independently-created ScreenCaptures would each
// have their own waitCh and only one could ever be signalled.
//
// waitCh starts open (not pre-closed): ScreenCapture.Read treats a closed
// waitCh as "the capture is done" and returns immediately without touching
// stdout, so a pre-closed channel here would end streaming before it ever
// started.
func (s *BroadcastSink) AsCapture() *ScreenCapture {
	s.mu.Lock()
	if s.capture != nil {
		c := s.capture
		s.mu.Unlock()
		return c
	}
	c := &ScreenCapture{stdout: s.pr, waitCh: make(chan struct{})}
	c.extraClose = s.Close
	s.capture = c
	closed := s.closed
	reason := s.closeReason
	s.mu.Unlock()

	if closed {
		// Narrow race: the sink was already closed (e.g. overflow) before
		// AsCapture was ever called. Propagate that now instead of leaving
		// a waitCh nothing will ever close.
		s.signalCaptureDone(reason)
	}
	return c
}

// Close closes this sink, signalling EOF (or, if it was already closed with
// a specific reason such as ErrSinkOverflow, that reason) to its reader —
// and, if AsCapture was called, unblocking any Stop() pending on the
// returned ScreenCapture.
func (s *BroadcastSink) Close() {
	s.close()
}
