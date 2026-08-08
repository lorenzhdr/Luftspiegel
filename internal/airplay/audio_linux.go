//go:build linux

package airplay

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// StartAudioCapture launches a GStreamer pipeline that captures system audio
// (the default sink's PulseAudio monitor, or PipeWire) and feeds raw PCM into
// the built-in ALAC encoder via AudioCapture.ReadFrame.
//
// tcpPort is part of the shared cross-platform signature (see
// audio_windows.go, which uses it to pick the local TCP listener port) and is
// unused on Linux, which has no TCP audio source.
func StartAudioCapture(ctx context.Context, testTone bool, tcpPort int) (*AudioCapture, error) {
	_ = tcpPort

	captureCtx, cancel := context.WithCancel(ctx)

	// Detect audio source
	var srcArgs []string
	if testTone {
		srcArgs = []string{"audiotestsrc", "wave=sine", "freq=440", "is-live=true",
			"samplesperbuffer=352"}
		dbg("[AUDIO] using test tone (440 Hz sine wave, live, spf=352)")
	} else if exec.Command("gst-inspect-1.0", "pulsesrc").Run() == nil {
		monitor := detectPulseMonitor()
		if monitor == "" {
			cancel()
			return nil, fmt.Errorf("no PulseAudio monitor source found")
		}
		srcArgs = []string{"pulsesrc", fmt.Sprintf("device=%s", monitor)}
		dbg("[AUDIO] using pulsesrc device=%s", monitor)
	} else if exec.Command("gst-inspect-1.0", "pipewiresrc").Run() == nil {
		srcArgs = []string{"pipewiresrc"}
		dbg("[AUDIO] using pipewiresrc")
	} else {
		cancel()
		return nil, fmt.Errorf("no audio source available (need pulsesrc or pipewiresrc)")
	}

	ac := &AudioCapture{
		cancel: cancel,
		waitCh: make(chan struct{}),
	}

	gstArgs := []string{"--quiet"}
	gstArgs = append(gstArgs, srcArgs...)
	gstArgs = append(gstArgs,
		"!", "audioconvert",
		"!", "audioresample",
		"!", "audio/x-raw,rate=44100,channels=2,format=S16LE",
		"!", "queue", "max-size-buffers=2", "max-size-bytes=0", "max-size-time=0", "leaky=downstream",
		"!", "fdsink", "fd=1", "sync=false", "async=false",
	)
	dbg("[AUDIO] ALAC verbatim pipeline: gst-launch-1.0 %s", strings.Join(gstArgs, " "))

	gstCmd := exec.CommandContext(captureCtx, "gst-launch-1.0", gstArgs...)
	gstStdout, err := gstCmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("gst stdout pipe: %w", err)
	}
	gstStderr, _ := gstCmd.StderrPipe()

	if err := gstCmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start ALAC gst pipeline: %w", err)
	}
	go logStderr("AUDIO-GST", gstStderr)

	ac.cmd = gstCmd
	ac.pcmPipe = gstStdout
	go func() {
		ac.waitErr = gstCmd.Wait()
		close(ac.waitCh)
	}()

	return ac, nil
}

// detectPulseMonitor finds the default PulseAudio sink's monitor source name.
func detectPulseMonitor() string {
	out, err := exec.Command("pactl", "get-default-sink").Output()
	if err != nil {
		dbg("[AUDIO] pactl get-default-sink failed: %v", err)
		return ""
	}
	sinkName := strings.TrimSpace(string(out))
	if sinkName == "" {
		return ""
	}
	return sinkName + ".monitor"
}
