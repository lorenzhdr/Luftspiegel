package airplay

import (
	"math"
	"sync"
	"testing"
	"time"
)

func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v (±%v)", name, got, want, tol)
	}
}

// A nil *SessionStats has to absorb every call: tests elsewhere build
// MirrorSession literals without stats, and the recording sites in the hot
// path deliberately carry no nil guard of their own.
func TestNilSessionStatsIsSafe(t *testing.T) {
	var s *SessionStats
	s.SetVideoParams(1920, 1080, "h264_mf", "display_remoting", time.Second)
	s.SetTargetLatency(time.Second)
	s.RecordFrame(100, true, time.Millisecond)
	s.RecordSocketWrite(time.Millisecond)
	s.RecordRTT(time.Millisecond)
	s.RecordReceiverRenderLatency(time.Millisecond)
	s.RecordAudioFrame()
	s.RecordAudioBuffered(1024)
	s.RecordAudioUnderrun(512)
	s.RecordAudioDrain(512)

	if got := s.Snapshot(); got.FramesSent != 0 || got.History != nil {
		t.Fatalf("nil stats produced a non-zero snapshot: %+v", got)
	}
}

func TestSnapshotCountersAndStaticParams(t *testing.T) {
	s := newSessionStats()
	s.SetVideoParams(1728, 1080, "h264_mf", "cbr_live", 100*time.Millisecond)

	s.RecordFrame(1000, true, 5*time.Millisecond)
	s.RecordFrame(500, false, 7*time.Millisecond)
	s.RecordFrame(500, false, 9*time.Millisecond)
	s.RecordRTT(4 * time.Millisecond)
	s.RecordReceiverRenderLatency(62 * time.Millisecond)

	snap := s.Snapshot()

	if snap.FramesSent != 3 {
		t.Errorf("FramesSent = %d, want 3", snap.FramesSent)
	}
	if snap.Keyframes != 1 {
		t.Errorf("Keyframes = %d, want 1", snap.Keyframes)
	}
	if snap.BytesSent != 2000 {
		t.Errorf("BytesSent = %d, want 2000", snap.BytesSent)
	}
	if snap.Width != 1728 || snap.Height != 1080 {
		t.Errorf("size = %dx%d, want 1728x1080", snap.Width, snap.Height)
	}
	if snap.Encoder != "h264_mf" || snap.RateControl != "cbr_live" {
		t.Errorf("encoder/rateControl = %q/%q", snap.Encoder, snap.RateControl)
	}
	approx(t, "TargetLatencyMs", snap.TargetLatencyMs, 100, 0.001)
	approx(t, "RTTMs", snap.RTTMs, 4, 0.001)
	approx(t, "ReceiverRenderLatencyMs", snap.ReceiverRenderLatencyMs, 62, 0.001)
	if snap.UptimeSec < 0 {
		t.Errorf("UptimeSec = %v, want >= 0", snap.UptimeSec)
	}
}

// RecordReceiverRenderLatency and RecordAudioBuffered are gauges: a later
// value replaces the earlier one rather than accumulating.
func TestGaugesReplaceRatherThanAccumulate(t *testing.T) {
	s := newSessionStats()
	s.RecordReceiverRenderLatency(80 * time.Millisecond)
	s.RecordReceiverRenderLatency(62 * time.Millisecond)
	s.RecordAudioBuffered(8000)
	s.RecordAudioBuffered(4000)

	snap := s.Snapshot()
	approx(t, "ReceiverRenderLatencyMs", snap.ReceiverRenderLatencyMs, 62, 0.001)
	// 4000 bytes at 176400 B/s ≈ 22.68ms
	approx(t, "AudioBufferMs", snap.AudioBufferMs, 4000*1000.0/176400.0, 0.01)
}

// Non-positive RTT is meaningless (a clock skew artefact) and must not enter
// the sample window at all.
func TestRecordRTTIgnoresNonPositive(t *testing.T) {
	s := newSessionStats()
	s.RecordRTT(5 * time.Millisecond)
	s.RecordRTT(0)
	s.RecordRTT(-2 * time.Millisecond)

	approx(t, "RTTMs", s.Snapshot().RTTMs, 5, 0.001)
}

// RTT is reported as a median so a single inflated sample — the RTSP mutex
// being held by another request, or a digest retry doubling the round trip —
// does not become the displayed value until the next measurement two seconds
// later.
func TestRecordRTTMedianAbsorbsOutlier(t *testing.T) {
	s := newSessionStats()
	for i := 0; i < 6; i++ {
		s.RecordRTT(4 * time.Millisecond)
	}
	s.RecordRTT(900 * time.Millisecond) // contention outlier

	if got := s.Snapshot().RTTMs; got > 10 {
		t.Errorf("RTTMs = %v, want the outlier absorbed (<=10)", got)
	}
}

// A sustained change in network conditions must still show through within a
// few samples — the median may not be so sticky that it hides a real shift.
func TestRecordRTTTracksSustainedChange(t *testing.T) {
	s := newSessionStats()
	for i := 0; i < statsRTTRing; i++ {
		s.RecordRTT(4 * time.Millisecond)
	}
	for i := 0; i < statsRTTRing; i++ {
		s.RecordRTT(40 * time.Millisecond)
	}
	approx(t, "RTTMs after sustained change", s.Snapshot().RTTMs, 40, 0.001)
}

func TestAudioCountersAccumulate(t *testing.T) {
	s := newSessionStats()
	s.RecordAudioFrame()
	s.RecordAudioFrame()
	s.RecordAudioUnderrun(1764) // 10ms of silence
	s.RecordAudioUnderrun(1764)
	s.RecordAudioDrain(17640) // 100ms drained
	s.RecordAudioDrain(0)     // ignored

	snap := s.Snapshot()
	if snap.AudioFramesSent != 2 {
		t.Errorf("AudioFramesSent = %d, want 2", snap.AudioFramesSent)
	}
	if snap.AudioUnderruns != 2 {
		t.Errorf("AudioUnderruns = %d, want 2", snap.AudioUnderruns)
	}
	approx(t, "AudioSilenceMs", snap.AudioSilenceMs, 20, 0.1)
	approx(t, "AudioDrainMs", snap.AudioDrainMs, 100, 0.1)
}

func TestDurationRingPercentiles(t *testing.T) {
	var r durationRing
	if p50, p95 := r.percentiles(); p50 != 0 || p95 != 0 {
		t.Fatalf("empty ring returned %v/%v, want 0/0", p50, p95)
	}

	// 1..100ms: nearest-rank puts p50 at index 50 (51ms) and p95 at index 95.
	for i := 1; i <= 100; i++ {
		r.add(time.Duration(i) * time.Millisecond)
	}
	p50, p95 := r.percentiles()
	approx(t, "p50", p50, 51, 0.001)
	approx(t, "p95", p95, 96, 0.001)
	if p95 < p50 {
		t.Errorf("p95 (%v) < p50 (%v)", p95, p50)
	}
}

// The ring keeps only the most recent statsLatencyRing samples, so a long run
// of small values must not drag the percentiles down forever.
func TestDurationRingOverwritesOldest(t *testing.T) {
	var r durationRing
	for i := 0; i < statsLatencyRing; i++ {
		r.add(1 * time.Millisecond)
	}
	for i := 0; i < statsLatencyRing; i++ {
		r.add(50 * time.Millisecond)
	}
	p50, _ := r.percentiles()
	approx(t, "p50 after full overwrite", p50, 50, 0.001)
}

func TestAUHoldPercentilesReachSnapshot(t *testing.T) {
	s := newSessionStats()
	for i := 1; i <= 20; i++ {
		s.RecordFrame(100, false, time.Duration(i)*time.Millisecond)
	}
	snap := s.Snapshot()
	if snap.AUHoldMsP50 <= 0 || snap.AUHoldMsP95 <= 0 {
		t.Fatalf("AUHold percentiles not populated: %+v", snap)
	}
	if snap.AUHoldMsP95 < snap.AUHoldMsP50 {
		t.Errorf("p95 (%v) < p50 (%v)", snap.AUHoldMsP95, snap.AUHoldMsP50)
	}
}

// A zero auHold means the frame had no usable start timestamp; it must be
// excluded from the percentiles rather than pulling them toward zero.
func TestZeroAUHoldExcludedFromPercentiles(t *testing.T) {
	s := newSessionStats()
	s.RecordFrame(100, false, 40*time.Millisecond)
	for i := 0; i < 50; i++ {
		s.RecordFrame(100, false, 0)
	}
	snap := s.Snapshot()
	approx(t, "AUHoldMsP50", snap.AUHoldMsP50, 40, 0.001)
	if snap.FramesSent != 51 {
		t.Errorf("FramesSent = %d, want 51 (zero-hold frames still count)", snap.FramesSent)
	}
}

// History is fixed-length, oldest-first, and excludes the in-progress slot so
// sparklines do not dip at their right edge.
func TestSnapshotHistoryShape(t *testing.T) {
	s := newSessionStats()
	// Backdate the session so a full history window has elapsed.
	s.startedAt = time.Now().Add(-90 * time.Second)
	s.RecordFrame(1000, false, time.Millisecond)

	snap := s.Snapshot()
	if len(snap.History) != statsSlots-1 {
		t.Fatalf("len(History) = %d, want %d", len(snap.History), statsSlots-1)
	}
	// The frame just recorded landed in the current (in-progress) slot, which
	// is deliberately excluded — so every returned point is an idle zero.
	for i, p := range snap.History {
		if p.FPS != 0 || p.BitrateKbps != 0 {
			t.Fatalf("History[%d] = %+v, want zero (in-progress slot must be excluded)", i, p)
		}
	}
}

// A frame recorded into an already-completed slot must show up in the history
// with the correct per-second rate, scaled from the 500ms slot width.
func TestHistoryRatesScaleFromSlotWidth(t *testing.T) {
	s := newSessionStats()
	s.startedAt = time.Now().Add(-10 * time.Second)

	// Place 15 frames of 1000 bytes into the slot immediately before the
	// current one, i.e. the last completed slot.
	prev := s.epochAt(time.Now()) - 1
	s.mu.Lock()
	b := s.bucketLocked(prev)
	b.frames = 15
	b.bytes = 15000
	s.mu.Unlock()

	snap := s.Snapshot()
	last := snap.History[len(snap.History)-1]
	// 15 frames per 500ms = 30fps; 15000 bytes per 500ms = 240 kbit/s.
	approx(t, "history FPS", last.FPS, 30, 0.001)
	approx(t, "history BitrateKbps", last.BitrateKbps, 240, 0.001)
	// Headline figures mirror the last completed slot.
	approx(t, "snapshot FPS", snap.FPS, 30, 0.001)
	approx(t, "snapshot BitrateKbps", snap.BitrateKbps, 240, 0.001)
}

// A slot whose epoch has wrapped around the ring must read as idle rather than
// replaying stale counts from a minute ago.
func TestStaleBucketReadsAsIdle(t *testing.T) {
	s := newSessionStats()
	s.startedAt = time.Now().Add(-10 * time.Second)

	current := s.epochAt(time.Now())
	s.mu.Lock()
	// Write into the ring slot the current epoch will map to, but tag it with
	// an epoch a full ring ago.
	idx := int(current % int64(statsSlots))
	s.buckets[idx] = slotBucket{epoch: current - int64(statsSlots), frames: 99, bytes: 99999}
	s.mu.Unlock()

	for _, p := range s.Snapshot().History {
		if p.FPS != 0 || p.BitrateKbps != 0 {
			t.Fatalf("stale bucket leaked into history: %+v", p)
		}
	}
}

// bucketLocked resets a bucket lazily when its epoch no longer matches, so
// consecutive slots must not accumulate into one another.
func TestBucketResetsOnEpochChange(t *testing.T) {
	s := newSessionStats()
	s.mu.Lock()
	b1 := s.bucketLocked(5)
	b1.frames = 10
	b2 := s.bucketLocked(5 + int64(statsSlots)) // same ring index, new epoch
	got := b2.frames
	s.mu.Unlock()

	if got != 0 {
		t.Errorf("bucket for new epoch had frames = %d, want 0", got)
	}
}

func TestPcmBytesToMs(t *testing.T) {
	approx(t, "one second", pcmBytesToMs(176400), 1000, 0.001)
	approx(t, "negative", pcmBytesToMs(-5), 0, 0.001)
	approx(t, "zero", pcmBytesToMs(0), 0, 0.001)
}

// The recording sites run from the frame loop, the feedback loop and the audio
// goroutine simultaneously; -race must stay clean.
func TestConcurrentRecordingIsRaceFree(t *testing.T) {
	s := newSessionStats()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.RecordFrame(100, j%30 == 0, time.Duration(j)*time.Microsecond)
				s.RecordSocketWrite(time.Duration(j) * time.Microsecond)
				s.RecordAudioFrame()
				s.RecordAudioBuffered(j * 10)
				s.RecordRTT(time.Duration(n+1) * time.Millisecond)
				_ = s.Snapshot()
			}
		}(i)
	}
	wg.Wait()

	if got := s.Snapshot().FramesSent; got != 8*200 {
		t.Errorf("FramesSent = %d, want %d", got, 8*200)
	}
}
