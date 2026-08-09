package airplay

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// CaptureConfig holds screen capture settings.
type CaptureConfig struct {
	FPS     int
	Bitrate int    // Video bitrate in kbps (0 = auto)
	HWAccel string // "auto", "nvenc", "vaapi", "openh264", or "none" (x264)

	X11WindowID   uint64
	X11WindowName string

	ShowCursor bool // show the mouse cursor in the captured video (Wayland, X11, and Windows)

	RestoreToken     string
	SaveRestoreToken func(string) error

	// OutputIndex selects which display adapter output (monitor) to capture on
	// Windows, via ffmpeg's ddagrab (0-based, matches DXGI output enumeration
	// order). Ignored on Linux.
	OutputIndex int

	// MaxHeight, if greater than zero, proportionally downscales the Windows
	// ffmpeg capture so the encoded output is at most this many pixels tall
	// (width follows the source aspect ratio; both dimensions stay even, as
	// required by yuv/nv12 encoding). Zero means native resolution. Ignored on
	// Linux.
	MaxHeight int

	// StubFile, if set, bypasses the platform capture backend entirely (no
	// ffmpeg/GStreamer process is started) and instead replays a pre-recorded
	// Annex-B H.264 file at roughly FPS access units per second, looping at
	// EOF. Intended for exercising the mirror/streaming pipeline without a
	// working capture backend. See StartStubCapture.
	StubFile string

	// GOPSeconds is the keyframe interval in seconds. 0 selects the default.
	GOPSeconds int
	// RateControl selects the h264_mf rate-control strategy:
	// "" or "display_remoting" (default), or "cbr_live" for
	// -rate_control cbr -scenario live_streaming.
	RateControl string
}

// ValidateHWAccel checks a capture encoder preference. An empty value keeps the
// zero-value CaptureConfig useful and is treated as auto.
func ValidateHWAccel(method string) error {
	switch method {
	case "", "auto", "nvenc", "vaapi", "openh264", "none":
		return nil
	default:
		return fmt.Errorf("unknown H.264 encoder %q (want auto, nvenc, vaapi, openh264, or none)", method)
	}
}

const (
	defaultVideoBitrateKbps = 4500
	minVideoBitrateKbps     = 1800
	maxVideoBitrateKbps     = 12000

	// GOP length bounds, in seconds, for CaptureConfig.GOPSeconds.
	minGOPSeconds     = 1
	maxGOPSeconds     = 10
	defaultGOPSeconds = 4

	// Synthetic test capture has no real display to size itself from, so it
	// uses a fixed resolution.
	testCaptureWidth  = 1920
	testCaptureHeight = 1080
)

// ValidateRateControl checks a capture rate-control preference. An empty
// value keeps the zero-value CaptureConfig useful and is treated as the
// display_remoting default.
func ValidateRateControl(rateControl string) error {
	switch rateControl {
	case "", "display_remoting", "cbr_live":
		return nil
	default:
		return fmt.Errorf("unknown rate control %q (want display_remoting or cbr_live)", rateControl)
	}
}

// ScreenCapture manages screen capture. StartCapture, StartTestCapture, and
// StartStubCapture (Linux/GStreamer, Windows/ffmpeg, and the file-replay stub,
// respectively) all produce a *ScreenCapture whose Read() yields a raw H.264
// Annex-B byte stream with in-band SPS/PPS; nothing else (no resolution or
// timestamp side-channel — see mirror.go for how those are derived from the
// stream itself).
type ScreenCapture struct {
	cmd      *exec.Cmd // capture process (gst-launch-1.0 on Linux, ffmpeg on Windows); nil for the stub backend
	stdout   io.ReadCloser
	cancel   context.CancelFunc
	waitCh   chan struct{} // closed when the capture is done (process exited, or stub goroutine returned)
	waitErr  error         // set before waitCh is closed
	stopOnce sync.Once

	// extraClose, if set, runs during Stop() for platform-specific cleanup that
	// isn't covered by cancel/stdout/cmd — e.g. closing the Wayland portal's
	// D-Bus session on Linux. Nil everywhere else.
	extraClose func()

	// encoderName and rateControlName record which video encoder and
	// rate-control strategy the platform backend actually launched with, so
	// the caller can hand it to a session's stats collector (which alone
	// knows the encoder choice; see StartCapture in capture_windows.go).
	// Left empty on backends that don't set them.
	encoderName     string
	rateControlName string
}

// EncoderName returns the video encoder actually in use (e.g. "h264_mf" or
// "libx264"), or "" if the active backend hasn't recorded one.
func (sc *ScreenCapture) EncoderName() string {
	return sc.encoderName
}

// RateControlName returns the rate-control strategy actually in use (e.g.
// "display_remoting" or "cbr_live"), or "" if not applicable/recorded.
func (sc *ScreenCapture) RateControlName() string {
	return sc.rateControlName
}

func (sc *ScreenCapture) Read(buf []byte) (int, error) {
	select {
	case <-sc.waitCh:
		if sc.waitErr != nil {
			return 0, fmt.Errorf("capture exited: %w", sc.waitErr)
		}
		return 0, io.EOF
	default:
	}
	return sc.stdout.Read(buf)
}

// Stop is safe to call concurrently and more than once (cmd/doubletake/main.go
// does both: a deferred Stop() and a <-ctx.Done() goroutine that also calls
// it). sync.Once makes the body run exactly once; callers that lose the race
// return immediately without waiting for the first call to finish — that
// matches the previous stopped-bool behavior and is intentional here, unlike
// a typical Once-guarded teardown.
func (sc *ScreenCapture) Stop() {
	sc.stopOnce.Do(func() {
		if sc.cancel != nil {
			sc.cancel()
		}

		// Close stdout to unblock any pending Read() call.
		if sc.stdout != nil {
			sc.stdout.Close()
		}

		if sc.extraClose != nil {
			sc.extraClose()
		}

		if sc.cmd != nil && sc.cmd.Process != nil {
			_ = sc.cmd.Process.Signal(os.Interrupt)
		}

		select {
		case <-sc.waitCh:
		case <-time.After(2 * time.Second):
			if sc.cmd != nil && sc.cmd.Process != nil {
				_ = sc.cmd.Process.Kill()
			}
			<-sc.waitCh
		}
	})
}

// logStderr relays a capture process's stderr to the debug log line by line.
// Shared by the Linux GStreamer backend and audio.go's GStreamer-based audio
// capture; the Windows ffmpeg backend uses its own variant that additionally
// retains a tail of recent output for error messages (see capture_windows.go).
func logStderr(prefix string, r io.Reader) {
	if r == nil {
		return
	}
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		dbg("[%s] %s", prefix, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		dbg("[%s] stderr read error: %v", prefix, err)
	}
}
