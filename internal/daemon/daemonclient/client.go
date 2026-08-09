package daemonclient

import (
	"encoding/json"
	"fmt"
	"time"

	"doubletake/internal/daemon"
)

// Client communicates with a running doubletake daemon over its control
// channel (a Unix domain socket on Unix-likes, a loopback TCP address on
// Windows — see daemon.DialControl).
type Client struct {
	SocketPath string
}

// New creates a client that connects to the daemon at the given control
// channel address.
func New(socketPath string) *Client {
	return &Client{SocketPath: socketPath}
}

// NewDefault creates a client using the default control channel address.
func NewDefault() *Client {
	return &Client{SocketPath: daemon.DefaultControlAddr()}
}

// Status returns the daemon's current state.
func (c *Client) Status() (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "status"})
}

// Stats returns the daemon's current state, including the streaming stream's
// connection-quality statistics (Response.Stats). There is no separate
// "stats" command on the wire: the control channel is one-shot per
// connection (see daemon.handleConn), and status already carries the stats
// snapshot alongside the rest of the state, so this is just Status under a
// name that matches what the caller is after.
func (c *Client) Stats() (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "status"})
}

// Discover triggers device discovery and returns found devices.
func (c *Client) Discover() (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "discover"})
}

// Devices returns the cached list of discovered devices.
func (c *Client) Devices() (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "devices"})
}

// Connect starts mirroring to the specified target (or first discovered device if empty).
func (c *Client) Connect(target string, port int, pin string) (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "connect", Target: target, Port: port, Pin: pin})
}

// Disconnect stops all active mirroring sessions.
func (c *Client) Disconnect() (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "disconnect"})
}

// DisconnectTarget stops the mirroring session to a specific receiver IP.
func (c *Client) DisconnectTarget(target string) (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "disconnect", Target: target})
}

// Mute mutes mirrored audio on all active sessions.
func (c *Client) Mute() (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "mute"})
}

// MuteTarget mutes mirrored audio on the session to a specific receiver IP.
func (c *Client) MuteTarget(target string) (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "mute", Target: target})
}

// Unmute unmutes mirrored audio on all active sessions.
func (c *Client) Unmute() (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "unmute"})
}

// UnmuteTarget unmutes mirrored audio on the session to a specific receiver IP.
func (c *Client) UnmuteTarget(target string) (*daemon.Response, error) {
	return c.send(daemon.Request{Cmd: "unmute", Target: target})
}

func (c *Client) send(req daemon.Request) (*daemon.Response, error) {
	conn, err := daemon.DialControl(c.SocketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	var resp daemon.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return &resp, nil
}
