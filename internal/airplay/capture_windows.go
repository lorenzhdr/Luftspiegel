//go:build windows

package airplay

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// StartCapture captures the Windows desktop via ffmpeg's ddagrab (DXGI
// Desktop Duplication) source, encoded with h264_mf (Media Foundation
// hardware encoding) by default, and streams raw Annex-B H.264 on stdout —
// same contract as the Linux GStreamer backend (see capture.go and mirror.go
// for how the receiving side derives resolution and timing).
//
// Verified ffmpeg invocation this is modeled on (Snapdragon X Elite, arm64):
//
//	ffmpeg -hide_banner -loglevel error -init_hw_device d3d11va
//	  -filter_complex "ddagrab=<idx>:framerate=<fps>:draw_mouse=<0|1>,hwdownload,format=bgra,fps=<fps>,scale=trunc(iw/2)*2:trunc(ih/2)*2,format=nv12"
//	  -c:v h264_mf -hw_encoding 1 -scenario display_remoting
//	  -b:v <kbps>k -g <fps> -fps_mode cfr
//	  -f h264 pipe:1
//
// Three requirements are load-bearing and were found by measurement, not
// documentation: format=nv12 must be the last filter stage (h264_mf accepts
// the yuv420p ddagrab would otherwise hand it, but the Media Foundation
// transform then dies with "Generic error in an external library"); fps=<fps>
// in the filter chain plus -fps_mode cfr together are required or ddagrab
// floods the muxer with "non monotonically increasing dts"; and
// -rate_control ld_vbr/gld_vbr is rejected by the Qualcomm MFT with
// E_INVALIDARG, so only -scenario display_remoting (or -rate_control cbr
// -scenario live_streaming) may be used for rate control.
func StartCapture(ctx context.Context, cfg CaptureConfig) (*ScreenCapture, error) {
	if err := ValidateHWAccel(cfg.HWAccel); err != nil {
		return nil, err
	}
	if cfg.StubFile != "" {
		return StartStubCapture(ctx, cfg)
	}

	ffmpegPath, err := resolveFFmpegPath()
	if err != nil {
		return nil, err
	}

	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	bitrate := windowsBitrateKbps(cfg.Bitrate)
	encoderArgs, err := windowsEncoderArgs(cfg.HWAccel, bitrate)
	if err != nil {
		return nil, err
	}

	drawMouse := 0
	if cfg.ShowCursor {
		drawMouse = 1
	}
	filter := fmt.Sprintf(
		"ddagrab=%d:framerate=%d:draw_mouse=%d,hwdownload,format=bgra,fps=%d,%s,format=nv12",
		cfg.OutputIndex, fps, drawMouse, fps, windowsScaleFilter(cfg.MaxHeight),
	)

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-init_hw_device", "d3d11va",
		"-filter_complex", filter,
	}
	args = append(args, encoderArgs...)
	args = append(args, "-g", strconv.Itoa(fps), "-fps_mode", "cfr", "-f", "h264", "pipe:1")

	dbg("[CAPTURE] %s %s", ffmpegPath, strings.Join(args, " "))
	return startFFmpegCapture(ctx, ffmpegPath, args)
}

// StartTestCapture creates a synthetic H.264 stream (ffmpeg's lavfi testsrc2)
// with the configured encoder — the Windows counterpart of the Linux
// videotestsrc path, used for the -test flag and daemon test mode.
func StartTestCapture(ctx context.Context, cfg CaptureConfig) (*ScreenCapture, error) {
	if err := ValidateHWAccel(cfg.HWAccel); err != nil {
		return nil, err
	}
	if cfg.StubFile != "" {
		return StartStubCapture(ctx, cfg)
	}

	ffmpegPath, err := resolveFFmpegPath()
	if err != nil {
		return nil, err
	}

	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	bitrate := windowsBitrateKbps(cfg.Bitrate)
	encoderArgs, err := windowsEncoderArgs(cfg.HWAccel, bitrate)
	if err != nil {
		return nil, err
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=%d", testCaptureWidth, testCaptureHeight, fps),
		"-vf", "format=nv12",
	}
	args = append(args, encoderArgs...)
	args = append(args, "-g", strconv.Itoa(fps), "-fps_mode", "cfr", "-f", "h264", "pipe:1")

	dbg("[CAPTURE] launching %s (test mode) %s", ffmpegPath, strings.Join(args, " "))
	return startFFmpegCapture(ctx, ffmpegPath, args)
}

// windowsBitrateKbps applies the CaptureConfig.Bitrate override (0 = auto) and
// clamps it to the same [min, max] range the Linux encoder-selection path
// uses. Unlike Linux's captureBitrateKbps, this does not attempt a
// resolution-aware recommendation: ddagrab's native capture resolution isn't
// known until the process starts, and per the task's ffmpeg contract the
// caller supplies -bitrate explicitly for anything other than a 1080p-ish
// default.
func windowsBitrateKbps(bitrate int) int {
	if bitrate <= 0 {
		return defaultVideoBitrateKbps
	}
	if bitrate < minVideoBitrateKbps {
		return minVideoBitrateKbps
	}
	if bitrate > maxVideoBitrateKbps {
		return maxVideoBitrateKbps
	}
	return bitrate
}

// windowsEncoderArgs returns the ffmpeg -c:v/... arguments for the requested
// HWAccel value. Only "" / "auto" (h264_mf hardware encoding) and "none"
// (libx264 software encoding) are available on Windows; nvenc/vaapi/openh264
// name Linux-only GStreamer encoders and are rejected with an explanatory
// error. ValidateHWAccel intentionally still accepts all of them so its
// Linux behavior (and the -hwaccel flag's shared help text) is unchanged —
// only StartCapture/StartTestCapture reject the Windows-unsupported values.
func windowsEncoderArgs(hwaccel string, bitrateKbps int) ([]string, error) {
	switch hwaccel {
	case "", "auto":
		return []string{
			"-c:v", "h264_mf",
			"-hw_encoding", "1",
			"-scenario", "display_remoting",
			"-b:v", fmt.Sprintf("%dk", bitrateKbps),
		}, nil
	case "none":
		return []string{
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-b:v", fmt.Sprintf("%dk", bitrateKbps),
		}, nil
	case "nvenc", "vaapi", "openh264":
		return nil, fmt.Errorf("-hwaccel %s is not available on Windows (that's a Linux/GStreamer encoder); use -hwaccel auto (h264_mf hardware encoding) or -hwaccel none (libx264 software encoding)", hwaccel)
	default:
		// ValidateHWAccel rejects anything else before this is reached.
		return nil, fmt.Errorf("unsupported -hwaccel %q on Windows", hwaccel)
	}
}

// windowsScaleFilter returns the ffmpeg scale filter stage for the capture
// filter chain. With no max height it only forces even width/height (yuv/nv12
// requires this; ddagrab's native resolution is not guaranteed even).
// Otherwise it downscales proportionally so the output is at most maxHeight
// pixels tall, never upscales, and still forces even dimensions.
//
// This deliberately does not use scale's force_original_aspect_ratio option:
// that option only resolves ambiguity when BOTH dimensions are given as a
// fixed bounding box, and does nothing when the other side is left automatic
// (-2, as here) — measured behavior is that it applies maxHeight literally
// and upscales past the source resolution. min(ih,maxHeight) as an explicit
// height expression is what actually caps at the source size.
func windowsScaleFilter(maxHeight int) string {
	if maxHeight > 0 {
		// The comma inside min(...) must be backslash-escaped: ffmpeg's filtergraph
		// parser splits filter stages on unescaped commas wherever they appear,
		// including inside a nested function-call argument list.
		return fmt.Sprintf(`scale=-2:trunc(min(ih\,%d)/2)*2`, maxHeight)
	}
	return "scale=trunc(iw/2)*2:trunc(ih/2)*2"
}

// resolveFFmpegPath finds the ffmpeg binary to drive capture with, in order:
// the DOUBLETAKE_FFMPEG environment variable, ffmpeg.exe next to the running
// executable (the repo ships one at bin/ffmpeg.exe, native arm64 with ddagrab
// and h264_mf), and finally PATH.
func resolveFFmpegPath() (string, error) {
	if p := os.Getenv("DOUBLETAKE_FFMPEG"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("DOUBLETAKE_FFMPEG=%q is set but not usable: %w", p, err)
		}
		return p, nil
	}
	if exePath, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exePath), "ffmpeg.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("ffmpeg not found: set DOUBLETAKE_FFMPEG=<path to ffmpeg.exe>, place ffmpeg.exe next to doubletake's executable, or add ffmpeg to PATH")
}

// startFFmpegCapture launches ffmpeg with the given arguments and wires its
// stdout into a *ScreenCapture. stderr is relayed to the debug log and its
// last ~1500 characters are retained so a failed process leaves a diagnosable
// error in waitErr instead of a bare exit status.
func startFFmpegCapture(ctx context.Context, ffmpegPath string, args []string) (*ScreenCapture, error) {
	captureCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(captureCtx, ffmpegPath, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	tail := newTailBuffer(1500)
	go streamFFmpegStderr(stderr, tail)

	capture := &ScreenCapture{
		cmd:    cmd,
		stdout: stdout,
		cancel: cancel,
		waitCh: make(chan struct{}),
	}
	go func() {
		waitErr := cmd.Wait()
		if waitErr != nil {
			if t := tail.String(); t != "" {
				waitErr = fmt.Errorf("%w (ffmpeg stderr: %s)", waitErr, t)
			}
		}
		capture.waitErr = waitErr
		close(capture.waitCh)
	}()

	return capture, nil
}

// streamFFmpegStderr relays ffmpeg's stderr to the debug log line by line
// while also retaining the last bytes in tail, for use in error messages
// after the process has already exited and its pipe is gone.
func streamFFmpegStderr(r io.Reader, tail *tailBuffer) {
	if r == nil {
		return
	}
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		dbg("[FFMPEG] %s", line)
		tail.Write([]byte(line + "\n"))
	}
}

// tailBuffer is a small thread-safe ring buffer that keeps only the most
// recent limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{limit: limit}
}

func (t *tailBuffer) Write(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > t.limit {
		t.data = t.data[len(t.data)-t.limit:]
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.data))
}
