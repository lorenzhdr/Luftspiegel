package daemon

import (
	"sync"
	"testing"
	"time"

	"doubletake/internal/airplay"
)

// The tests in this file cover the rule that no blocking teardown work may
// run while d.mu is held. Session teardown can take 60-120s against a
// receiver that fell off the network (MirrorSession.Close waits for worker
// goroutines parked in an RTSP request with a 30s read deadline, then sends a
// synchronous TEARDOWN); holding the mutex across that froze the entire
// control channel and made the GUI look hung.
//
// -race is unavailable on windows/arm64, so these tests synchronise through
// explicit channel handshakes rather than sleeps. Timeouts appear only as
// failure detectors, never as a success condition.

// blockingTeardown returns a teardown callback that parks until release is
// closed, plus a channel that is closed once the callback has been entered.
func blockingTeardown() (fn func(*airplay.BroadcastSink, *airplay.MirrorSession, *airplay.AirPlayClient, *airplay.ScreenCapture),
	entered chan struct{}, release chan struct{}) {

	entered = make(chan struct{})
	release = make(chan struct{})
	var once sync.Once
	fn = func(*airplay.BroadcastSink, *airplay.MirrorSession, *airplay.AirPlayClient, *airplay.ScreenCapture) {
		once.Do(func() { close(entered) })
		<-release
	}
	return fn, entered, release
}

func daemonWithStream(target string, teardownFn func(*airplay.BroadcastSink, *airplay.MirrorSession, *airplay.AirPlayClient, *airplay.ScreenCapture)) *Daemon {
	return &Daemon{
		streams: map[string]*activeStream{
			target: {device: "test", deviceIP: target, state: StateStreaming},
		},
		teardownFn: teardownFn,
	}
}

// TestHandleDisconnectReleasesLockDuringTeardown is the core regression test:
// a status poll must stay responsive while a teardown is still running, and
// must already report the stream as gone.
func TestHandleDisconnectReleasesLockDuringTeardown(t *testing.T) {
	const target = "192.168.178.125"
	fn, entered, release := blockingTeardown()
	defer close(release)

	d := daemonWithStream(target, fn)

	disconnectReturned := make(chan struct{})
	go func() {
		d.handleDisconnect(Request{Cmd: "disconnect", Target: target})
		close(disconnectReturned)
	}()

	// Wait until the teardown is genuinely in flight before probing.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown callback was never entered")
	}

	statusDone := make(chan Response, 1)
	go func() { statusDone <- d.handleStatus() }()

	select {
	case resp := <-statusDone:
		if len(resp.Streams) != 0 {
			t.Fatalf("status still lists %d stream(s) while teardown is in flight; want 0", len(resp.Streams))
		}
		if resp.State != StateIdle {
			t.Fatalf("status state = %q, want %q", resp.State, StateIdle)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("handleStatus blocked while a teardown was in flight — d.mu is being held across the teardown")
	}

	select {
	case <-disconnectReturned:
	case <-time.After(disconnectTeardownWait + 2*time.Second):
		t.Fatal("handleDisconnect did not return within its teardown timeout")
	}
}

// TestHandleDisconnectReturnsWithinTimeout: a teardown that never finishes
// must not pin the request forever.
func TestHandleDisconnectReturnsWithinTimeout(t *testing.T) {
	const target = "192.168.178.125"
	fn, entered, release := blockingTeardown()
	defer close(release)

	d := daemonWithStream(target, fn)

	done := make(chan Response, 1)
	start := time.Now()
	go func() { done <- d.handleDisconnect(Request{Cmd: "disconnect", Target: target}) }()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown callback was never entered")
	}

	select {
	case resp := <-done:
		if !resp.OK {
			t.Fatalf("handleDisconnect reported failure for a stream that was removed: %q", resp.Error)
		}
		if elapsed := time.Since(start); elapsed < disconnectTeardownWait {
			t.Fatalf("handleDisconnect returned after %v, before the %v teardown wait elapsed", elapsed, disconnectTeardownWait)
		}
	case <-time.After(disconnectTeardownWait + 2*time.Second):
		t.Fatalf("handleDisconnect did not return within %v", disconnectTeardownWait+2*time.Second)
	}
}

// TestShutdownWaitsForInFlightTeardown: Shutdown must not return while a
// TEARDOWN started by an earlier disconnect is still running, otherwise the
// process exit kills it and the receiver refuses the next connection.
func TestShutdownWaitsForInFlightTeardown(t *testing.T) {
	const target = "192.168.178.125"
	fn, entered, release := blockingTeardown()

	d := daemonWithStream(target, fn)

	// Let a disconnect run into its timeout, leaving the teardown in flight.
	d.handleDisconnect(Request{Cmd: "disconnect", Target: target})

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("teardown callback was never entered")
	}

	shutdownDone := make(chan struct{})
	go func() {
		d.Shutdown()
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
		close(release)
		t.Fatal("Shutdown returned while a teardown was still in flight")
	case <-time.After(300 * time.Millisecond):
		// Expected: still waiting.
	}

	close(release)
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after the teardown completed")
	}
}

// TestShutdownDoesNotDeadlockWhenTeardownTouchesLock proves Shutdown releases
// d.mu before waiting on the teardown WaitGroup.
func TestShutdownDoesNotDeadlockWhenTeardownTouchesLock(t *testing.T) {
	const target = "192.168.178.125"
	d := &Daemon{
		streams: map[string]*activeStream{
			target: {device: "test", deviceIP: target, state: StateStreaming},
		},
	}
	d.teardownFn = func(*airplay.BroadcastSink, *airplay.MirrorSession, *airplay.AirPlayClient, *airplay.ScreenCapture) {
		// A real teardown must not do this (documented on teardownFn), but
		// if Shutdown held d.mu while waiting, this would deadlock — which
		// is exactly what we want to catch.
		d.handleStatus()
	}

	done := make(chan struct{})
	go func() {
		d.Shutdown()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown deadlocked against a teardown that takes d.mu")
	}
}

// TestStopAllTearsDownStreamsConcurrently: N dead receivers must cost one
// timeout, not N. Previously stopAllLocked closed sessions serially.
func TestStopAllTearsDownStreamsConcurrently(t *testing.T) {
	targets := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}

	var mu sync.Mutex
	running, maxRunning := 0, 0
	allStarted := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	d := &Daemon{streams: map[string]*activeStream{}}
	for _, tgt := range targets {
		d.streams[tgt] = &activeStream{device: "test", deviceIP: tgt, state: StateStreaming}
	}
	var once sync.Once
	d.teardownFn = func(*airplay.BroadcastSink, *airplay.MirrorSession, *airplay.AirPlayClient, *airplay.ScreenCapture) {
		mu.Lock()
		running++
		if running > maxRunning {
			maxRunning = running
		}
		reached := running == len(targets)
		mu.Unlock()
		if reached {
			once.Do(func() { close(allStarted) })
		}
		<-release
		mu.Lock()
		running--
		mu.Unlock()
	}

	go d.handleDisconnect(Request{Cmd: "disconnect"})

	select {
	case <-allStarted:
		// All three teardowns were in flight at the same time.
	case <-time.After(2 * time.Second):
		mu.Lock()
		got := maxRunning
		mu.Unlock()
		t.Fatalf("only %d of %d teardowns ran concurrently; they are still serialised", got, len(targets))
	}
}

// TestRemoveStreamLockedReturnsCaptureWithoutStopping: the blocking Stop()
// must be handed to the caller, not run under the lock.
func TestRemoveStreamLockedReturnsCaptureWithoutStopping(t *testing.T) {
	const target = "192.168.178.125"
	capture := &airplay.ScreenCapture{}
	d := &Daemon{
		streams: map[string]*activeStream{
			target: {device: "test", deviceIP: target, state: StateStreaming},
		},
		capture: capture,
	}

	d.mu.Lock()
	got := d.removeStreamLocked(target)
	d.mu.Unlock()

	if got != capture {
		t.Fatalf("removeStreamLocked returned %p, want the detached capture %p", got, capture)
	}
	if d.capture != nil {
		t.Error("d.capture should be nil after the last stream was removed")
	}
	if d.broadcast != nil {
		t.Error("d.broadcast should be nil after the last stream was removed")
	}
	if !d.captureStopExpected {
		t.Error("captureStopExpected should be set before the capture is handed off")
	}
}

// TestRemoveStreamLockedReleasesAudioOwner: the audio slot must be freed so a
// later connect can take it over.
func TestRemoveStreamLockedReleasesAudioOwner(t *testing.T) {
	const target = "192.168.178.125"
	d := &Daemon{
		streams: map[string]*activeStream{
			target: {device: "test", deviceIP: target, state: StateStreaming},
		},
		audioOwner: target,
	}

	d.mu.Lock()
	d.removeStreamLocked(target)
	owner := d.audioOwner
	d.mu.Unlock()

	if owner != "" {
		t.Fatalf("audioOwner = %q after the owning stream was removed, want empty", owner)
	}
}

// TestSecondStreamReportsNoAudio: a stream that could not claim the single
// audio source must report has_audio:false rather than advertising audio that
// is not flowing.
func TestSecondStreamReportsNoAudio(t *testing.T) {
	d := &Daemon{
		streams: map[string]*activeStream{
			"10.0.0.1": {device: "first", deviceIP: "10.0.0.1", state: StateStreaming},
			"10.0.0.2": {device: "second", deviceIP: "10.0.0.2", state: StateStreaming, audioSuppressed: true},
		},
		audioOwner: "10.0.0.1",
	}

	d.mu.Lock()
	resp := d.statusResponseLocked(true, "")
	d.mu.Unlock()

	found := false
	for _, s := range resp.Streams {
		if s.DeviceIP != "10.0.0.2" {
			continue
		}
		found = true
		if s.HasAudio {
			t.Error("suppressed stream reports has_audio:true; the GUI would claim audio that is not being sent")
		}
	}
	if !found {
		t.Fatal("second stream missing from status response")
	}
}
