package airplay

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Session statistics for connection-quality reporting.
//
// Two design constraints shaped this:
//
//   - The recording calls sit in the per-frame hot path (StreamFrames ->
//     flushVCL -> sendFrame, ~30-60x/s for video, ~125x/s for audio). Plain
//     counters are therefore atomics; only the ring buffers take a mutex, and
//     that mutex is never held across I/O.
//   - Rate and history must not depend on anyone calling Snapshot. A GUI that
//     polls at 1Hz, a one-shot `doubletake-ctl stats` and a session nobody
//     watches all have to produce the same numbers, so rates are accumulated
//     into fixed time-slot buckets as frames arrive rather than sampled by a
//     ticker goroutine. No extra goroutine, no lifecycle to unwind on teardown.
const (
	// statsSlot is the accumulation window for rates. 500ms rather than a full
	// second on purpose: with a 1s GOP, one-second averages hide exactly the
	// per-keyframe bitrate spikes the statistics exist to make visible.
	statsSlot = 500 * time.Millisecond
	// statsSlots covers 60s of history at statsSlot resolution, plus one slot
	// of slack so the bucket currently being written is never also the oldest
	// one being read.
	statsSlots = int(60*time.Second/statsSlot) + 1
	// statsLatencyRing bounds the per-frame duration samples kept for
	// percentiles — ~8s of video at 30fps, enough to be representative without
	// growing without bound.
	statsLatencyRing = 256
	// statsRTTRing is much smaller because RTT is sampled once per /feedback
	// round trip (every 2s), not per frame: 8 samples is ~16s of history, long
	// enough to absorb a contention outlier and short enough that a genuine
	// change in network conditions shows up within a few ticks.
	statsRTTRing = 8
)

// HistoryPoint is one statsSlot-sized bucket of the rolling history, oldest
// first. Used by the GUI to draw sparklines.
type HistoryPoint struct {
	FPS         float64 `json:"fps"`
	BitrateKbps float64 `json:"bitrate_kbps"`
	AUHoldMs    float64 `json:"au_hold_ms"`
}

// StatsSnapshot is a consistent, serialisable copy of a session's statistics.
// The json tags are the wire contract shared with the daemon control channel
// and the Electron GUI — the daemon passes this through verbatim rather than
// re-mapping it, so renaming a field here changes the GUI's contract too.
type StatsSnapshot struct {
	UptimeSec float64 `json:"uptime_sec"`

	// Video
	FramesSent  uint64  `json:"frames_sent"`
	Keyframes   uint64  `json:"keyframes"`
	BytesSent   uint64  `json:"bytes_sent"`
	FPS         float64 `json:"fps"`
	BitrateKbps float64 `json:"bitrate_kbps"`

	// AUHold is the time from the first byte of an access unit being parsed to
	// its send completing. This is the part of end-to-end latency this program
	// controls, and the metric the NAL parser's idle-flush is measured against.
	AUHoldMsP50 float64 `json:"au_hold_ms_p50"`
	AUHoldMsP95 float64 `json:"au_hold_ms_p95"`

	// SocketWrite isolates network stalls from local processing delay.
	SocketWriteMsP50 float64 `json:"socket_write_ms_p50"`
	SocketWriteMsP95 float64 `json:"socket_write_ms_p95"`

	// Network. Both are reported by the receiver, so both can be absent:
	// RTTMs stays 0 until the first /feedback round trip completes, and
	// ReceiverRenderLatencyMs only appears if the receiver announced it.
	RTTMs                   float64 `json:"rtt_ms"`
	ReceiverRenderLatencyMs float64 `json:"receiver_render_latency_ms"`

	// Audio
	AudioFramesSent uint64  `json:"audio_frames_sent"`
	AudioBufferMs   float64 `json:"audio_buffer_ms"`
	AudioUnderruns  uint64  `json:"audio_underruns"`
	AudioSilenceMs  float64 `json:"audio_silence_ms"`
	AudioDrainMs    float64 `json:"audio_drain_ms"`

	// Static session parameters, echoed so the GUI can show what is actually
	// in effect rather than what was requested.
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	Encoder         string  `json:"encoder"`
	RateControl     string  `json:"rate_control"`
	TargetLatencyMs float64 `json:"target_latency_ms"`

	History []HistoryPoint `json:"history"`
}

// slotBucket accumulates one statsSlot of activity. epoch identifies which
// slot it holds so a stale bucket can be recognised and reset lazily instead
// of being cleared by a timer.
type slotBucket struct {
	epoch       int64
	frames      uint64
	bytes       uint64
	auHoldSum   time.Duration
	auHoldCount int
}

// SessionStats collects per-session metrics. The zero value is not usable;
// construct with newSessionStats.
type SessionStats struct {
	startedAt time.Time

	// Plain counters — atomic, no lock.
	framesSent        atomic.Uint64
	keyframes         atomic.Uint64
	bytesSent         atomic.Uint64
	audioFramesSent   atomic.Uint64
	audioUnderruns    atomic.Uint64
	audioSilenceBytes atomic.Uint64
	audioDrainBytes   atomic.Uint64

	// Gauges — last observed value rather than a total.
	audioBufferedBytes      atomic.Int64
	receiverRenderLatencyUs atomic.Int64

	mu          sync.Mutex
	buckets     [statsSlots]slotBucket
	auHold      durationRing
	socketWrite durationRing
	rtt         durationRing

	// Static parameters, set once during setup and read under mu.
	width, height        int
	encoder, rateControl string
	targetLatency        time.Duration
}

func newSessionStats() *SessionStats {
	s := &SessionStats{startedAt: time.Now()}
	s.auHold.cap = statsLatencyRing
	s.socketWrite.cap = statsLatencyRing
	s.rtt.cap = statsRTTRing
	return s
}

// durationRing keeps the most recent samples for percentile estimation,
// overwriting oldest-first. Capacity is set on first use so different metrics
// can retain different amounts of history: per-frame metrics want a wide
// window, a metric sampled every two seconds wants a narrow one or it would
// take minutes to reflect a real change.
type durationRing struct {
	samples []time.Duration
	cap     int
	next    int
	filled  bool
}

func (r *durationRing) add(d time.Duration) {
	if r.cap == 0 {
		r.cap = statsLatencyRing
	}
	if r.samples == nil {
		r.samples = make([]time.Duration, r.cap)
	}
	r.samples[r.next] = d
	r.next = (r.next + 1) % r.cap
	if r.next == 0 {
		r.filled = true
	}
}

func (r *durationRing) len() int {
	if r.filled {
		return r.cap
	}
	return r.next
}

// sorted returns a sorted copy of the retained samples, or nil when empty.
func (r *durationRing) sortedCopy() []time.Duration {
	n := r.len()
	if n == 0 {
		return nil
	}
	out := make([]time.Duration, n)
	copy(out, r.samples[:n])
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// percentiles returns the p50 and p95 of the retained samples in milliseconds.
// Returns (0, 0) when no samples have been recorded yet.
func (r *durationRing) percentiles() (p50, p95 float64) {
	sorted := r.sortedCopy()
	if sorted == nil {
		return 0, 0
	}
	return millis(quantile(sorted, 0.50)), millis(quantile(sorted, 0.95))
}

// median returns the p50 of the retained samples in milliseconds.
func (r *durationRing) median() float64 {
	sorted := r.sortedCopy()
	if sorted == nil {
		return 0
	}
	return millis(quantile(sorted, 0.50))
}

// quantile picks the nearest-rank element of an already sorted slice.
func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q * float64(len(sorted)))
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func millis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// epochAt maps a wall-clock instant to its statsSlot index since session start.
func (s *SessionStats) epochAt(t time.Time) int64 {
	return int64(t.Sub(s.startedAt) / statsSlot)
}

// bucketLocked returns the bucket for epoch, resetting it first if it still
// holds an older slot's data. Callers must hold s.mu.
func (s *SessionStats) bucketLocked(epoch int64) *slotBucket {
	b := &s.buckets[int(epoch%int64(statsSlots))]
	if b.epoch != epoch {
		*b = slotBucket{epoch: epoch}
	}
	return b
}

// SetVideoParams records the encoder parameters actually in effect, so the GUI
// can display what is running rather than what was configured.
func (s *SessionStats) SetVideoParams(width, height int, encoder, rateControl string, targetLatency time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.width, s.height = width, height
	s.encoder, s.rateControl = encoder, rateControl
	s.targetLatency = targetLatency
}

// SetEncoder records which encoder and rate-control strategy the capture
// backend actually launched with. Separate from SetVideoParams because only
// the capture layer knows these, and it learns them after the session (and its
// negotiated latency) already exists — overwriting the whole parameter set at
// that point would clobber the negotiated value with a pre-negotiation one.
func (s *SessionStats) SetEncoder(encoder, rateControl string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.encoder, s.rateControl = encoder, rateControl
}

// SetTargetLatency records the session latency actually negotiated with the
// receiver, which may differ from what was requested (see the Audio-Latency
// header handling in mirror.go).
func (s *SessionStats) SetTargetLatency(d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targetLatency = d
}

// RecordFrame records one sent access unit. auHold is the time from the AU's
// first byte being parsed to the send completing.
func (s *SessionStats) RecordFrame(bytes int, keyframe bool, auHold time.Duration) {
	if s == nil {
		return
	}
	s.framesSent.Add(1)
	s.bytesSent.Add(uint64(bytes))
	if keyframe {
		s.keyframes.Add(1)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bucketLocked(s.epochAt(time.Now()))
	b.frames++
	b.bytes += uint64(bytes)
	if auHold > 0 {
		b.auHoldSum += auHold
		b.auHoldCount++
		s.auHold.add(auHold)
	}
}

// RecordSocketWrite records how long one frame's socket write took.
func (s *SessionStats) RecordSocketWrite(d time.Duration) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.socketWrite.add(d)
}

// RecordRTT records a round-trip time measured against the receiver.
//
// Reported as a median rather than the latest reading. The measurement is
// taken around an RTSP request that holds the client mutex for its whole
// duration and may retry internally on a digest challenge, so an individual
// sample can be inflated by contention that has nothing to do with the
// network. At one sample every two seconds a single outlier would otherwise
// stay on screen until the next one arrived; a median over the retained
// samples absorbs it while still tracking a genuine change within a few ticks.
func (s *SessionStats) RecordRTT(d time.Duration) {
	if s == nil || d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rtt.add(d)
}

// RecordReceiverRenderLatency records the arrival-to-render delay the receiver
// reports about itself.
func (s *SessionStats) RecordReceiverRenderLatency(d time.Duration) {
	if s == nil || d < 0 {
		return
	}
	s.receiverRenderLatencyUs.Store(int64(d / time.Microsecond))
}

// RecordAudioFrame records one encoded audio frame handed to the receiver.
func (s *SessionStats) RecordAudioFrame() {
	if s == nil {
		return
	}
	s.audioFramesSent.Add(1)
}

// RecordAudioBuffered updates the current PCM backlog gauge.
func (s *SessionStats) RecordAudioBuffered(bytes int) {
	if s == nil {
		return
	}
	s.audioBufferedBytes.Store(int64(bytes))
}

// RecordAudioUnderrun records a read that had to be padded with silence, along
// with how many bytes of silence were inserted.
func (s *SessionStats) RecordAudioUnderrun(silenceBytes int) {
	if s == nil {
		return
	}
	s.audioUnderruns.Add(1)
	if silenceBytes > 0 {
		s.audioSilenceBytes.Add(uint64(silenceBytes))
	}
}

// RecordAudioDrain records PCM discarded to pull the backlog back down toward
// its target. Read alongside the underrun count: drain climbing while
// underruns stay flat means the buffer converged; both climbing together means
// it is oscillating and the target is set too low.
func (s *SessionStats) RecordAudioDrain(bytes int) {
	if s == nil || bytes <= 0 {
		return
	}
	s.audioDrainBytes.Add(uint64(bytes))
}

// Snapshot returns a consistent copy of the current statistics.
func (s *SessionStats) Snapshot() StatsSnapshot {
	if s == nil {
		return StatsSnapshot{}
	}
	now := time.Now()
	snap := StatsSnapshot{
		UptimeSec:               now.Sub(s.startedAt).Seconds(),
		FramesSent:              s.framesSent.Load(),
		Keyframes:               s.keyframes.Load(),
		BytesSent:               s.bytesSent.Load(),
		ReceiverRenderLatencyMs: float64(s.receiverRenderLatencyUs.Load()) / 1000,
		AudioFramesSent:         s.audioFramesSent.Load(),
		AudioBufferMs:           pcmBytesToMs(s.audioBufferedBytes.Load()),
		AudioUnderruns:          s.audioUnderruns.Load(),
		AudioSilenceMs:          pcmBytesToMs(int64(s.audioSilenceBytes.Load())),
		AudioDrainMs:            pcmBytesToMs(int64(s.audioDrainBytes.Load())),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	snap.Width, snap.Height = s.width, s.height
	snap.Encoder, snap.RateControl = s.encoder, s.rateControl
	snap.TargetLatencyMs = millis(s.targetLatency)
	snap.AUHoldMsP50, snap.AUHoldMsP95 = s.auHold.percentiles()
	snap.SocketWriteMsP50, snap.SocketWriteMsP95 = s.socketWrite.percentiles()
	snap.RTTMs = s.rtt.median()

	// Build history oldest-first, ending at the last *completed* slot. The slot
	// in progress is skipped: it is only partially filled, and including it
	// would make every sparkline dip at its right edge.
	current := s.epochAt(now)
	snap.History = make([]HistoryPoint, 0, statsSlots-1)
	for epoch := current - int64(statsSlots) + 1; epoch < current; epoch++ {
		if epoch < 0 {
			continue
		}
		b := &s.buckets[int(epoch%int64(statsSlots))]
		if b.epoch != epoch {
			// Never written, or already overwritten: a genuinely idle slot.
			snap.History = append(snap.History, HistoryPoint{})
			continue
		}
		snap.History = append(snap.History, HistoryPoint{
			FPS:         float64(b.frames) / statsSlot.Seconds(),
			BitrateKbps: float64(b.bytes) * 8 / 1000 / statsSlot.Seconds(),
			AUHoldMs:    avgMillis(b.auHoldSum, b.auHoldCount),
		})
	}

	// Headline rate figures come from the most recent completed slot so they
	// agree with the right-hand edge of the sparklines.
	if n := len(snap.History); n > 0 {
		snap.FPS = snap.History[n-1].FPS
		snap.BitrateKbps = snap.History[n-1].BitrateKbps
	}

	return snap
}

func avgMillis(total time.Duration, count int) float64 {
	if count == 0 {
		return 0
	}
	return millis(total / time.Duration(count))
}

// pcmBytesToMs converts a PCM byte count to milliseconds at the fixed AirPlay
// mirroring audio format (44100Hz, stereo, s16le — see audio.go).
func pcmBytesToMs(bytes int64) float64 {
	if bytes <= 0 {
		return 0
	}
	return float64(bytes) * 1000 / float64(audioBytesPerSecond)
}

// audioBytesPerSecond is the wire rate of the mirroring audio format: 44100
// frames/s * 2 channels * 2 bytes. Declared here rather than reusing the
// Windows-only pcmOutBytesPerSec so stats build on every platform.
const audioBytesPerSecond = 44100 * 2 * 2
