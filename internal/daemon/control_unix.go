//go:build !windows

package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// defaultControlAddr returns the default control channel address on
// Unix-likes: a Unix domain socket path derived from XDG_RUNTIME_DIR (or
// /tmp when unset), matching the original doubletake behavior.
func defaultControlAddr() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, "doubletake.sock")
}

// listenControl opens the control channel listener. addr is a filesystem
// path for the Unix domain socket. A stale socket file left behind by a
// previous, uncleanly terminated run is removed first, and the resulting
// socket is chmod'd to owner-only.
func listenControl(addr string) (net.Listener, error) {
	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket %s: %w", addr, err)
	}
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	if err := os.Chmod(addr, 0700); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod socket %s: %w", addr, err)
	}
	return ln, nil
}

// dialControl connects to the control channel at addr, a Unix domain socket path.
func dialControl(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", addr, timeout)
}

// cleanupControlAddr removes the Unix domain socket file at addr, if present.
func cleanupControlAddr(addr string) {
	os.Remove(addr)
}
