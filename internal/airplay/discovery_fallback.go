package airplay

import (
	"context"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"howett.net/plist"
)

const (
	// fallbackScanPort is the AirPlay RTSP/HTTP control port every receiver
	// listens on; mDNS advertises it too, but it's fixed in practice.
	fallbackScanPort = 7000

	// fallbackDialTimeout bounds a single TCP connect probe.
	fallbackDialTimeout = 600 * time.Millisecond

	// fallbackInfoTimeout bounds the HTTP GET /info request to a host that
	// already answered on fallbackScanPort.
	fallbackInfoTimeout = 1500 * time.Millisecond

	// fallbackMaxWorkers caps concurrent connect probes so a /24 scan
	// finishes in roughly fallbackDialTimeout instead of 254x that.
	fallbackMaxWorkers = 64

	// fallbackBudget is the hard ceiling for an entire fallback scan,
	// regardless of how many subnets/hosts are candidates.
	fallbackBudget = 5 * time.Second
)

// nonLANAdapterNamePatterns are substrings (case-insensitive) of interface
// names that indicate a virtual, VPN, or otherwise non-LAN adapter. Scanning
// these wastes the time budget and can trigger VPN/firewall noise.
var nonLANAdapterNamePatterns = []string{
	"vethernet", "vmware", "virtualbox", "wsl", "loopback",
	"tailscale", "zerotier", "hamachi", "docker",
	"nordlynx", "openvpn", "tap",
}

// fallbackInfoResponse decodes the subset of the AirPlay GET /info binary
// plist response that identifies a receiver. The full schema lives in
// ReceiverInfo (client.go); this fallback only needs enough to populate a
// DeviceInfo entry.
type fallbackInfoResponse struct {
	Name     string `plist:"name"`
	Model    string `plist:"model"`
	DeviceID string `plist:"deviceID"`
}

// DiscoverAirPlayDevicesFallback scans reachable private (RFC1918) /24
// subnets for hosts listening on the AirPlay control port (TCP 7000), for use
// when mDNS discovery finds nothing — e.g. because another mDNS responder or
// a VPN already owns UDP 5353 on this machine. It is intentionally
// conservative: only /24 networks are scanned (never a /16 or larger), probes
// run with a short timeout and bounded concurrency, and the whole scan is
// capped by fallbackBudget and cancellable via ctx.
//
// This never returns an error for a normal "found nothing" or "couldn't
// parse a response" outcome — a host that answers on port 7000 is reported
// even if its /info response can't be parsed, since that is still stronger
// evidence than nothing. The returned error is reserved for inputs the
// caller passed in being unusable (currently: never, but the signature
// leaves room for it).
func DiscoverAirPlayDevicesFallback(ctx context.Context) ([]AirPlayDevice, error) {
	ctx, cancel := context.WithTimeout(ctx, fallbackBudget)
	defer cancel()

	targets := fallbackScanTargets()
	if len(targets) == 0 {
		return nil, nil
	}

	resultsCh := make(chan AirPlayDevice, len(targets))
	sem := make(chan struct{}, fallbackMaxWorkers)
	var wg sync.WaitGroup

	dialer := net.Dialer{Timeout: fallbackDialTimeout}

scan:
	for _, ip := range targets {
		select {
		case <-ctx.Done():
			break scan
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()

			addr := net.JoinHostPort(ip, "7000")
			conn, err := dialer.DialContext(ctx, "tcp", addr)
			if err != nil {
				return
			}
			conn.Close()

			resultsCh <- fetchFallbackDeviceInfo(ctx, ip, fallbackScanPort)
		}(ip)
	}

	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var devices []AirPlayDevice
	for dev := range resultsCh {
		devices = append(devices, dev)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].IP < devices[j].IP })
	return devices, nil
}

// fetchFallbackDeviceInfo fetches and parses GET /info from a host already
// known to accept TCP connections on port. It always returns a usable
// AirPlayDevice (at minimum IP+Port) — a failed request or an unparseable
// response is logged at debug level and otherwise swallowed, per
// DiscoverAirPlayDevicesFallback's contract.
func fetchFallbackDeviceInfo(ctx context.Context, ip string, port int) AirPlayDevice {
	dev := AirPlayDevice{IP: ip, Port: port}

	reqCtx, cancel := context.WithTimeout(ctx, fallbackInfoTimeout)
	defer cancel()

	url := "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/info"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		dbg("[discovery-fallback] build request for %s: %v", ip, err)
		return dev
	}

	client := &http.Client{Timeout: fallbackInfoTimeout}
	resp, err := client.Do(req)
	if err != nil {
		dbg("[discovery-fallback] GET %s: %v", url, err)
		return dev
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		dbg("[discovery-fallback] read body from %s: %v", url, err)
		return dev
	}

	var info fallbackInfoResponse
	if _, err := plist.Unmarshal(body, &info); err != nil {
		dbg("[discovery-fallback] parse /info plist from %s: %v", url, err)
		return dev
	}

	dev.Name = info.Name
	dev.Model = info.Model
	dev.DeviceID = info.DeviceID
	return dev
}

// fallbackScanTargets enumerates candidate host IPv4 addresses across all
// usable local /24 private subnets, skipping virtual/VPN adapters and
// anything larger than a /24 (to avoid scanning tens of thousands of hosts).
func fallbackScanTargets() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	seen := make(map[string]struct{})
	var targets []string

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isNonLANAdapterName(iface.Name) {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || !ip4.IsPrivate() {
				continue
			}
			ones, bits := ipNet.Mask.Size()
			if bits != 32 || ones != 24 {
				// Only scan /24s: large networks (e.g. /16 corporate LANs)
				// would mean tens of thousands of probes, and anything not
				// exactly /24 is unusual enough here to skip rather than
				// guess at host-count math.
				continue
			}

			for _, host := range hostsInIPv4_24(ip4, ipNet.Mask) {
				if host == ip4.String() {
					continue // skip our own address
				}
				if _, dup := seen[host]; dup {
					continue
				}
				seen[host] = struct{}{}
				targets = append(targets, host)
			}
		}
	}

	return targets
}

// hostsInIPv4_24 returns every usable host address ("*.1".."*.254") in the
// /24 network containing ip.
func hostsInIPv4_24(ip net.IP, mask net.IPMask) []string {
	network := ip.Mask(mask)
	base := []byte{network[0], network[1], network[2]}

	hosts := make([]string, 0, 254)
	for i := 1; i <= 254; i++ {
		hosts = append(hosts, net.IPv4(base[0], base[1], base[2], byte(i)).String())
	}
	return hosts
}

func isNonLANAdapterName(name string) bool {
	lower := strings.ToLower(name)
	for _, pattern := range nonLANAdapterNamePatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}
