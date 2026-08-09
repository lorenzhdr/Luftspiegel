package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"doubletake/internal/daemon"
	"doubletake/internal/daemon/daemonclient"
)

func main() {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socketPath := fs.String("socket", daemon.DefaultControlAddr(), "daemon control channel address (Unix socket path on Linux/macOS; host:port or bare port on Windows)")
	fs.Usage = usage

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	args := fs.Args()
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}

	client := daemonclient.New(*socketPath)
	cmd := args[0]

	var resp *daemon.Response
	var err error

	switch cmd {
	case "status":
		resp, err = client.Status()
	case "stats":
		resp, err = client.Stats()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		printStats(resp)
		if !resp.OK {
			os.Exit(1)
		}
		return
	case "discover":
		resp, err = client.Discover()
	case "devices":
		resp, err = client.Devices()
	case "connect":
		target := ""
		pin := ""
		if len(args) >= 2 {
			target = args[1]
		}
		if len(args) >= 3 {
			pin = args[2]
		}
		resp, err = client.Connect(target, 0, pin)
	case "pin":
		if len(args) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: doubletake-ctl pin <PIN-or-password>\n")
			os.Exit(1)
		}
		resp, err = client.Connect("", 0, args[1])
	case "disconnect":
		if len(args) >= 2 {
			resp, err = client.DisconnectTarget(args[1])
		} else {
			resp, err = client.Disconnect()
		}
	case "mute":
		if len(args) >= 2 {
			resp, err = client.MuteTarget(args[1])
		} else {
			resp, err = client.Mute()
		}
	case "unmute":
		if len(args) >= 2 {
			resp, err = client.UnmuteTarget(args[1])
		} else {
			resp, err = client.Unmute()
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(resp)

	if !resp.OK {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: doubletake-ctl [-socket path] <command> [args]\n\nCommands:\n  status                              Show daemon state and all active streams\n  stats                               Show connection-quality statistics for the streaming stream\n  discover                            Discover AirPlay devices on the network\n  devices                             List cached discovered devices\n  connect [target] [PIN-or-password]  Start mirroring (to target IP, or first free device)\n  pin <PIN-or-password>               Submit pairing credentials for a waiting device\n  disconnect [target]                 Stop mirroring (all streams, or only the given IP)\n  mute [target]                       Mute mirrored audio (all streams, or only the given IP)\n  unmute [target]                     Unmute mirrored audio (all streams, or only the given IP)\n\nFlags:\n  -socket path                        Override daemon socket path (default: %s)\n", daemon.DefaultControlAddr())
}

// printStats renders resp.Stats (populated by the daemon's "status" command
// — see Client.Stats) as compact, human-readable text grouped by
// video/network/audio, rather than the raw JSON the other commands print.
// This is the tool used for latency A/B comparisons, so the numbers that
// matter most for that (au_hold_ms, fps, bitrate_kbps, rtt_ms,
// receiver_render_latency_ms) are surfaced up top rather than buried in a
// field dump. The history sparkline array is deliberately never printed
// here (up to 120 points — not something you read in a terminal).
func printStats(resp *daemon.Response) {
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "error: %s\n", resp.Error)
		return
	}
	if resp.Stats == nil {
		fmt.Println("not currently streaming")
		return
	}
	s := resp.Stats

	fmt.Printf("%s (%s) — streaming %.0fs\n\n", resp.Device, resp.DeviceIP, s.UptimeSec)

	fmt.Println("Video:")
	fmt.Printf("  encoder             %s / %s\n", orDash(s.Encoder), orDash(s.RateControl))
	if s.Width > 0 && s.Height > 0 {
		fmt.Printf("  resolution          %dx%d\n", s.Width, s.Height)
	}
	fmt.Printf("  fps                 %.1f\n", s.FPS)
	fmt.Printf("  bitrate             %.0f kbps\n", s.BitrateKbps)
	fmt.Printf("  frames sent         %d (%d keyframes)\n", s.FramesSent, s.Keyframes)
	fmt.Printf("  au hold (send lag)  p50 %.1f ms / p95 %.1f ms\n", s.AUHoldMsP50, s.AUHoldMsP95)
	fmt.Printf("  socket write        p50 %.1f ms / p95 %.1f ms\n", s.SocketWriteMsP50, s.SocketWriteMsP95)

	fmt.Println("\nNetwork:")
	fmt.Printf("  target latency      %.1f ms\n", s.TargetLatencyMs)
	fmt.Printf("  rtt                 %.1f ms\n", s.RTTMs)
	fmt.Printf("  receiver latency    %.1f ms\n", s.ReceiverRenderLatencyMs)

	fmt.Println("\nAudio:")
	if !resp.HasAudio {
		fmt.Println("  (no audio)")
	} else {
		fmt.Printf("  frames sent         %d\n", s.AudioFramesSent)
		fmt.Printf("  buffer              %.0f ms\n", s.AudioBufferMs)
		fmt.Printf("  underruns           %d (%.0f ms silence, %.0f ms drained)\n", s.AudioUnderruns, s.AudioSilenceMs, s.AudioDrainMs)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
