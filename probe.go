package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"

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

// selfCheckPortal probes checkURI from the Wi-Fi interface itself,
// bypassing the tailscale tunnel when the fwmark works. Verdict:
//   - 3xx redirect with Location -> portal, URL = Location
//   - 2xx with HTML login body    -> portal, URL = gateway candidate
//   - 2xx plain / 204            -> no portal
//   - other                       -> portal (unknown URL)
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
