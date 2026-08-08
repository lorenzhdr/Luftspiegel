// Command pcmtest simulates doubletake's Windows audio-source contract
// without needing the real Electron GUI: it connects to the doubletake TCP
// audio listener (see internal/airplay/audio_windows.go) as a client and
// streams a continuous sine-wave tone as raw PCM — s16le, 44100 Hz, stereo,
// interleaved, little endian, no header — exactly the wire format the GUI is
// contractually required to send.
//
// Useful for exercising internal/airplay's Windows audio capture path (silence
// fallback, reconnect, buffer bounding) end-to-end without an Apple TV or the
// GUI. Kept under tools/ for future manual/regression testing, not just this
// change.
//
// Usage:
//
//	go run ./tools/pcmtest [-addr 127.0.0.1:7655] [-freq 440] [-duration 30s]
//
// duration=0 streams until interrupted (Ctrl+C).
package main

import (
	"encoding/binary"
	"flag"
	"log"
	"math"
	"net"
	"time"
)

const (
	sampleRate = 44100
	channels   = 2
	bytesPerSample = 2
	bytesPerFrame  = channels * bytesPerSample
)

func main() {
	addr := flag.String("addr", "127.0.0.1:7655", "doubletake audio TCP listener address")
	freq := flag.Float64("freq", 440, "sine tone frequency in Hz")
	duration := flag.Duration("duration", 0, "how long to stream (0 = until interrupted)")
	chunkMs := flag.Int("chunk-ms", 20, "PCM chunk size in milliseconds per write, simulating the GUI's WASAPI callback cadence")
	flag.Parse()

	conn, err := net.Dial("tcp", *addr)
	if err != nil {
		log.Fatalf("connect to %s: %v", *addr, err)
	}
	defer conn.Close()
	log.Printf("connected to %s; streaming %.1f Hz sine, %d Hz stereo s16le, %dms chunks",
		*addr, *freq, sampleRate, *chunkMs)

	framesPerChunk := sampleRate * (*chunkMs) / 1000
	if framesPerChunk <= 0 {
		framesPerChunk = 1
	}
	chunk := make([]byte, framesPerChunk*bytesPerFrame)
	ticker := time.NewTicker(time.Duration(*chunkMs) * time.Millisecond)
	defer ticker.Stop()

	var phase float64
	step := 2 * math.Pi * (*freq) / sampleRate

	deadline := time.Time{}
	if *duration > 0 {
		deadline = time.Now().Add(*duration)
	}

	var totalBytes int64
	start := time.Now()
	for {
		for i := 0; i < framesPerChunk; i++ {
			v := int16(math.Sin(phase) * 16000)
			off := i * bytesPerFrame
			binary.LittleEndian.PutUint16(chunk[off:], uint16(v))
			binary.LittleEndian.PutUint16(chunk[off+2:], uint16(v))
			phase += step
			if phase > 2*math.Pi {
				phase -= 2 * math.Pi
			}
		}
		if _, err := conn.Write(chunk); err != nil {
			log.Fatalf("write after %v, %d bytes sent: %v", time.Since(start), totalBytes, err)
		}
		totalBytes += int64(len(chunk))

		if !deadline.IsZero() && time.Now().After(deadline) {
			log.Printf("done: streamed %d bytes over %v", totalBytes, time.Since(start))
			return
		}
		<-ticker.C
	}
}
