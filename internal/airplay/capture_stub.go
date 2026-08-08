package airplay

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// StartStubCapture replays a pre-recorded Annex-B H.264 file as a
// *ScreenCapture instead of driving a real capture backend. No ffmpeg or
// GStreamer process is started. It exists so the mirror/streaming pipeline
// (pairing, FairPlay, RTP timing) can be exercised end-to-end without a
// working platform capture backend — see the -stub-file flag in
// cmd/doubletake.
//
// The file is split into access units (a run of non-VCL NALs — SPS, PPS,
// SEI, AUD — followed by exactly one VCL slice NAL) using the same NAL
// scanner the mirror session itself uses (h264Parser), and replayed at
// roughly cfg.FPS access units per second so downstream timing behaves like
// a live capture. Playback loops back to the start of the file at EOF so a
// long-running soak test is possible.
func StartStubCapture(ctx context.Context, cfg CaptureConfig) (*ScreenCapture, error) {
	data, err := os.ReadFile(cfg.StubFile)
	if err != nil {
		return nil, fmt.Errorf("read stub file %q: %w", cfg.StubFile, err)
	}

	units := splitAnnexBAccessUnits(data)
	if len(units) == 0 {
		return nil, fmt.Errorf("stub file %q contains no H.264 access units", cfg.StubFile)
	}
	dbg("[CAPTURE] stub capture: %d access units from %q", len(units), cfg.StubFile)

	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	interval := time.Second / time.Duration(fps)

	captureCtx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()

	capture := &ScreenCapture{
		stdout: pr,
		cancel: cancel,
		waitCh: make(chan struct{}),
	}

	go func() {
		defer close(capture.waitCh)
		defer pw.Close()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for i := 0; ; i = (i + 1) % len(units) {
			select {
			case <-captureCtx.Done():
				capture.waitErr = captureCtx.Err()
				return
			case <-ticker.C:
				if _, err := pw.Write(units[i]); err != nil {
					capture.waitErr = err
					return
				}
			}
		}
	}()

	return capture, nil
}

// splitAnnexBAccessUnits scans a complete Annex-B H.264 buffer and groups its
// NAL units into access units: each returned slice contains zero or more
// leading non-VCL NALs (parameter sets, SEI, AUD, ...) followed by exactly
// one VCL slice NAL (type 1-5), start codes included, ready to be replayed
// byte-for-byte. A trailing run of non-VCL NALs with no following slice (rare
// for a well-formed capture) is dropped.
func splitAnnexBAccessUnits(data []byte) [][]byte {
	// h264Parser (mirror.go) already implements Annex-B NAL scanning; append a
	// synthetic trailing start code so its lookahead-based scanner flushes the
	// final real NAL in the file (it otherwise withholds whatever follows the
	// last start code, since more data could still be coming on a live pipe).
	padded := make([]byte, len(data)+4)
	copy(padded, data)
	padded[len(data)+3] = 1 // 00 00 00 01

	nals := newH264Parser().Push(padded)

	var units [][]byte
	auStart := -1
	for i, nal := range nals {
		if auStart < 0 {
			auStart = i
		}
		switch nalType(nal) {
		case 1, 2, 3, 4, 5: // VCL slice — completes the access unit
			var au []byte
			for _, part := range nals[auStart : i+1] {
				au = append(au, part...)
			}
			units = append(units, au)
			auStart = -1
		}
	}
	return units
}
