package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// Tailscale's fwmark bypass: packets marked 0x80000 skip tailscale's
// policy-routing table (ip rule 5210: fwmark 0x80000 lookup main) and
// follow the normal routing table — i.e. the real Wi-Fi default route.
const bypassMark = 0x80000

// probeMarkEnabled toggles the fwmark attempt (tests).
var probeMarkEnabled = true

// selfProbeDialer builds a dialer bound to the Wi-Fi interface IP and
// (best effort) marked with tailscale's bypass fwmark. Without
// CAP_NET_ADMIN the mark fails with EPERM; we then degrade to an
// unmarked bound dial, which still sees the portal whenever the
// tailscale tunnel is not routing everything.
func selfProbeDialer(bindIP string, mark bool) *net.Dialer {
	return &net.Dialer{
		Timeout:   nmTimeout,
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(bindIP)},
		Control: func(network, address string, c syscall.RawConn) error {
			if !mark || !probeMarkEnabled {
				return nil
			}
			var opErr error
			err := c.Control(func(fd uintptr) {
				opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, bypassMark)
			})
			if err != nil {
				return err
			}
			if opErr != nil {
				// EPERM without CAP_NET_ADMIN: degrade to unmarked
				logProbeFallback(opErr)
			}
			return nil // proceed unmarked rather than failing the probe
		},
	}
}

// probeHTTPDo is the self-probe transport seam (tests swap it).
var probeHTTPDo = func(dialer *net.Dialer, req *http.Request) (*http.Response, error) {
	client := &http.Client{
		Timeout: nmTimeout,
		Transport: &http.Transport{
			DialContext:       dialer.DialContext,
			DialTLS:           nil,
			ForceAttemptHTTP2: false,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client.Do(req)
}

// resolveVia queries one DNS server directly and returns the IPv4
// addresses for host. The seam lets the wall test target the
// *portal's* DNS server (from NM) instead of the system resolver —
// the walled verdict lives in what that server answers for A records.
// NAT64-synthesized AAAA answers are deliberately ignored.
var resolveVia = func(ctx context.Context, server, host string) ([]string, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", net.JoinHostPort(server, "53"))
		},
	}
	return r.LookupHost(ctx, host)
}

// Wall-control domains: stable real-internet hosts that captive
// portals never allowlist. Their A-records vanishing while an
// allowlisted check-domain still resolves = DNS-wall portal
// (wifionice class: DNS allowlist via NAT64, native unfiltered IPv6,
// standard check URIFS deliberately allowed).
var wallControlDomains = []string{"neverssl.com", "tailscale.com"}

// wallCheckDomain is allowlisted by most portals on purpose (Apple's
// captive check) so it serves as the always-resolves control.
const wallCheckDomain = "captive.apple.com"

// appleSuccessBody is the EXACT response when the internet is open;
// any other body from that host = content-injection hijack.
const appleSuccessBody = "<HTML><HEAD><TITLE>Success</TITLE></HEAD><BODY>Success</BODY></HTML>"

// dnsServerFor reads the IPv4 DNS servers of NM's primary Ip4Config.
// The wall test queries the portal's own DNS; the system resolver
// (127.0.0.53 stub) would hide the hijack from us.
var dnsServerFor = func(ctx context.Context) (string, error) {
	conn, err := realDial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.close()
	mgr := conn.objectAt(nmPath)
	var act dbus.ObjectPath
	if err := getProperty(mgr, "", nmInterface, "PrimaryConnection", &act); err != nil {
		return "", err
	}
	if act == "" || act == "/" {
		return "", fmt.Errorf("no primary connection")
	}
	ac := conn.objectAt(act)
	var ip4 dbus.ObjectPath
	if err := getProperty(ac, string(act), nmDest+".Connection.Active", "Ip4Config", &ip4); err != nil {
		return "", err
	}
	if ip4 == "" || ip4 == "/" {
		return "", fmt.Errorf("no Ip4Config")
	}
	var packed []uint32
	if err := getProperty(conn.objectAt(ip4), string(ip4), nmDest+".IP4Config", "Nameservers", &packed); err != nil {
		return "", err
	}
	if len(packed) == 0 {
		return "", fmt.Errorf("no DNS servers")
	}
	return packedUint32ToIPv4(packed[0]), nil
}

// packedUint32ToIPv4 decodes NM's packed-nameserver format (uint32,
// host byte order of the little-endian wire bytes) to dotted quad.
func packedUint32ToIPv4(n uint32) string {
	// godbus delivers the wire bytes as a uint32 in host order; NM
	// packs the IP big-endian INTO THE WIRE, which arrives
	// byte-reversed in native u arithmetic — i.e. the first octet is
	// the least significant byte (verified live: 0x955DF80A -> 10.240.93.149).
	b := []byte{byte(n & 0xff), byte(n >> 8 & 0xff), byte(n >> 16 & 0xff), byte(n >> 24 & 0xff)}
	return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
}

// aRecordsAreWalled asks the portal DNS for the IPv4 address of host.
// Returns (resolvesA bool) — ignoring AAAA/NAT64 synthetics.
func aRecordsAreWalled(ctx context.Context, server, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, nmTimeout)
	defer cancel()
	addrs, err := resolveVia(ctx, server, host)
	if err != nil {
		return true // resolution failed = walled (or DNS dead)
	}
	for _, a := range addrs {
		if net.ParseIP(a).To4() != nil {
			return false // real IPv4 answer
		}
	}
	return true // only IPv6 answers (NAT64 synthetic) = walled for v4
}

// selfCheckDNSWall runs the allowlist-vs-control matrix through the
// portal DNS, extended with a routing-wall probe:
//
//  1. DNS wall: control domain has no A record while the check
//     domain resolves -> portal (walled DNS / NXDOMAIN / NAT64-only).
//  2. Routing wall: control domain's A record connects to :80 while
//     the check domain's :80 stays dead (or both fine) — probe both
//     TCP-wise; control blocked + check open -> portal (wifionice
//     v4-routing style: DNS answers happily, TCP is walled).
//
// Returns (isPortal, nil) only on a clear VERDICT; (false, nil)
// otherwise. The login URL is not derivable here (no redirect).
func selfCheckDNSWall(ctx context.Context) (bool, error) {
	server, err := dnsServerFor(ctx)
	if err != nil {
		return false, err
	}
	checkOK := !aRecordsAreWalled(ctx, server, wallCheckDomain)
	if !checkOK {
		// allowlisted check-domain doesn't resolve: DNS is simply dead
		return false, nil
	}
	for _, host := range wallControlDomains {
		if aRecordsAreWalled(ctx, server, host) {
			return true, nil // walled A-records + alive allowlist = portal
		}
		// Routing wall: A-resolves but TCP :80 connect fails while the
		// check domain's :80 works (live-verified on wifionice).
		ips, rerr := resolveVia(ctx, server, host)
		if rerr != nil || len(ips) == 0 {
			continue
		}
		if v4 := firstIPv4Record(ips); v4 != "" {
			if !tcpPortOpen(ctx, v4, 80) && checkTCPOpen(ctx, server) {
				return true, nil
			}
		}
	}
	return false, nil
}

// firstIPv4Record picks the first IPv4 address of a resolver answer.
func firstIPv4Record(ips []string) string {
	for _, ip := range ips {
		if net.ParseIP(ip).To4() != nil {
			return ip
		}
	}
	return ""
}

// tcpPortOpen dials host:port with the portal-wall timeout. Seam for
// tests: production dials for real.
var tcpPortOpen = func(ctx context.Context, host string, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, nmTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// checkTCPOpen verifies the check domain's :80 is reachable — guards
// against declaring a portal when ALL v4 is unreachable (plain
// outages produce no verdict). Uses the check domain's A record
// through the same server.
func checkTCPOpen(ctx context.Context, server string) bool {
	ips, err := resolveVia(ctx, server, wallCheckDomain)
	if err != nil {
		return false
	}
	if v4 := firstIPv4Record(ips); v4 != "" {
		return tcpPortOpen(ctx, v4, 80)
	}
	return false
}

// appleCheckProbes the Apple hotspot URL over the probe dialer. Three
// verdicts: OK (exact success body), hijacked (other body/redirect),
// unknown (transport error). Works for content-injection portals; on
// allowlist portals (wifionice) Apple is allowed through and the body
// matches — hence the separate DNS-wall check.
func appleCheck(ctx context.Context, bindIP string) (verdict int, url string) {
	const appleURL = "http://captive.apple.com/hotspot-detect.html"
	req, err := http.NewRequestWithContext(ctx, portalHTTPGet, appleURL, nil)
	if err != nil {
		return probeUnknown, ""
	}
	req.Header.Set("User-Agent", portalUserAgent)
	resp, err := probeHTTPDo(selfProbeDialer(bindIP, true), req)
	if err != nil {
		return probeUnknown, ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			return probeHijacked, loc
		}
		return probeHijacked, ""
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 &&
		strings.TrimSpace(string(body)) == appleSuccessBody {
		return probeOK, ""
	}
	if resp.StatusCode == http.StatusNoContent {
		return probeOK, "" // 204: portal answered with the canonical OK
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return probeHijacked, "" // 2xx with replaced content
	}
	return probeHijacked, ""
}

// probe verdicts for appleCheck.
const (
	probeOK       = 0
	probeHijacked = 1
	probeUnknown  = 2
)

func selfCheckPortal(ctx context.Context, bindIP, checkURI, gateway string) (bool, string) {
	if bindIP == "" || checkURI == "" {
		return false, ""
	}
	req, err := http.NewRequestWithContext(ctx, portalHTTPGet, checkURI, nil)
	if err != nil {
		return false, ""
	}
	req.Header.Set("User-Agent", portalUserAgent)
	resp, err := probeHTTPDo(selfProbeDialer(bindIP, true), req)
	if err != nil {
		// Unreachable wifi path counts as portal-unknown, not portal:
		// NM's verdict (if any) remains the source of truth.
		return false, ""
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			return true, loc
		}
		return true, gatewayURL(gateway)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if isHijackedBody(resp.Header.Get("Content-Type"), body) {
			return true, gatewayURL(gateway)
		}
		return false, ""
	}
	return true, gatewayURL(gateway)
}

func gatewayURL(gateway string) string {
	if gateway == "" {
		return ""
	}
	return "http://" + gateway + "/"
}

// logProbeFallback throttles the EPERM fallback notice to once.
var logProbeFallback = throttleNotice()

func throttleNotice() func(error) {
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			logWarnf("fwmark bypass unavailable (%v): portal probes can't escape an active tunnel; enable NM's connectivity check or install traytail with setcap cap_net_admin", err)
		})
	}
}

// bindIPFor resolves the source IP of the default-route interface —
// the address the self-probe must bind to. Uses the default route via
// net interfaces; returns "" when nothing suitable exists.
func bindIPFor() string {
	addrs, err := interfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || !ipnet.IP.IsGlobalUnicast() {
			continue
		}
		ip := ipnet.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		// skip tailscale's CGNAT space and docker bridges
		if strings.HasPrefix(ip.String(), "100.") || strings.HasPrefix(ip.String(), "172.17.") {
			continue
		}
		return ip.String()
	}
	return ""
}
