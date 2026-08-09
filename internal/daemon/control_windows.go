//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// defaultControlPort is the TCP port the daemon listens on for the control
// channel on Windows, where Unix domain sockets aren't available. The
// Electron GUI dials this port to talk to the daemon.
const defaultControlPort = 7654

// defaultControlAddr returns the default control channel address on Windows:
// a loopback-only TCP address. Never bind 0.0.0.0 here — the control channel
// is unauthenticated and must not be reachable from the network.
func defaultControlAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", defaultControlPort)
}

// normalizeControlAddr accepts either a bare port ("7654"), a host:port pair
// ("127.0.0.1:7654"), or an empty string (default), and always returns a
// loopback host:port address.
func normalizeControlAddr(addr string) string {
	if addr == "" {
		return defaultControlAddr()
	}
	if port, err := strconv.Atoi(addr); err == nil {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	if host, port, err := net.SplitHostPort(addr); err == nil {
		// Force loopback. The control channel has no authentication
		// whatsoever (see handleRequest), so binding it to a wildcard or a
		// LAN address would let anyone on the network connect/disconnect
		// streams and mirror this screen. "-socket :7654" and
		// "-socket 0.0.0.0:7654" used to do exactly that, despite the
		// promise in this function's own doc comment.
		if !isLoopbackHost(host) {
			log.Printf("[daemon] control address %q is not loopback; the control channel is unauthenticated and has been restricted to 127.0.0.1:%s", addr, port)
			return net.JoinHostPort("127.0.0.1", port)
		}
		return addr
	}
	// A bare host without a port; append the default port.
	if !isLoopbackHost(addr) {
		log.Printf("[daemon] control host %q is not loopback; the control channel is unauthenticated and has been restricted to 127.0.0.1", addr)
		return defaultControlAddr()
	}
	return net.JoinHostPort(addr, strconv.Itoa(defaultControlPort))
}

// isLoopbackHost reports whether host is safe to bind the unauthenticated
// control channel to.
//
// An empty host means the wildcard address (":7654" binds all interfaces),
// which is explicitly not loopback. Anything that isn't a literal loopback IP
// or "localhost" is treated as unsafe — including names this process cannot
// resolve, so an unresolvable host can never fail open.
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// listenControl opens the control channel listener. addr may be a bare port,
// a host:port pair, or empty for the default.
//
// Unlike a Unix domain socket, a TCP listener leaves nothing on disk to clean
// up on exit, and there is no "stale socket file" to remove before binding.
func listenControl(addr string) (net.Listener, error) {
	addr = normalizeControlAddr(addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if isAddrInUse(err) {
			return nil, fmt.Errorf("listen %s: address already in use — is doubletake already running?", addr)
		}
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return ln, nil
}

// wsaEADDRINUSE is the raw Winsock error code (WSAEADDRINUSE) returned by
// bind() when the address is already in use. Note this is distinct from
// syscall.EADDRINUSE on Windows: that constant belongs to Go's separate
// POSIX-errno-emulation block and never matches a real network syscall
// error here, so errors.Is(err, syscall.EADDRINUSE) would silently never
// fire — the raw errno has to be compared directly instead.
const wsaEADDRINUSE syscall.Errno = 10048

func isAddrInUse(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == wsaEADDRINUSE
}

// dialControl connects to the control channel at addr (bare port, host:port,
// or empty for the default).
func dialControl(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", normalizeControlAddr(addr), timeout)
}

// cleanupControlAddr is a no-op on Windows: a TCP listener leaves nothing on
// disk to remove when the daemon shuts down.
func cleanupControlAddr(addr string) {}
