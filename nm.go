package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// NetworkManager connectivity states (libnm NMConnectivityState).
const (
	nmConnectivityUnknown = 0
	nmConnectivityNone    = 1
	nmConnectivityPortal  = 2
	nmConnectivityLimited = 3
	nmConnectivityFull    = 4
)

const (
	nmDest          = "org.freedesktop.NetworkManager"
	nmPath          = dbus.ObjectPath("/org/freedesktop/NetworkManager")
	propInterface   = "org.freedesktop.DBus.Properties"
	nmInterface     = nmDest
	nmTimeout       = 5 * time.Second
	portalHTTPGet   = "GET"
	portalUserAgent = "traytail/1.0"

	// forceInterval throttles fresh CheckConnectivity() calls: NM only
	// re-checks every ~300s on its own, too slow to notice a newly
	// joined captive portal.
	forceInterval = time.Minute
)

// clockNow is a seam for tests (throttle + hysteresis timing).
var clockNow = time.Now

// nmObject is the slice of dbus.BusObject used here; an interface so
// tests can fake the manager object without a real bus.
type nmObject interface {
	Call(method string, flags dbus.Flags, args ...any) *dbus.Call
}

// nmObjectAt resolves the object for a path on a connection-like type.
type nmConn interface {
	objectAt(path dbus.ObjectPath) nmObject
	close() error
}

// realConn wraps *dbus.Conn.
type realConn struct{ c *dbus.Conn }

func (r *realConn) objectAt(path dbus.ObjectPath) nmObject {
	return r.c.Object(nmDest, path)
}
func (r *realConn) close() error { return r.c.Close() }

// realDial opens the system bus; seam for tests.
var realDial = func(ctx context.Context) (nmConn, error) {
	_ = ctx
	c, err := dbus.SystemBus()
	if err != nil {
		return nil, err
	}
	return &realConn{c: c}, nil
}

// getProperty fetches a typed property from an arbitrary NM object.
// Decodes the DBus variant into out's pointer type.
func getProperty(obj nmObject, path, iface, name string, out any) error {
	var v dbus.Variant
	err := obj.Call(propInterface+".Get", 0, iface, name).Store(&v)
	if err != nil {
		return err
	}
	_ = path
	switch o := out.(type) {
	case *uint32:
		u, _ := v.Value().(uint32)
		*o = u
	case *bool:
		b, _ := v.Value().(bool)
		*o = b
	case *string:
		s, _ := v.Value().(string)
		*o = s
	case *dbus.ObjectPath:
		p, _ := v.Value().(dbus.ObjectPath)
		*o = p
	case *[]string:
		switch raw := v.Value().(type) {
		case string:
			if raw != "" {
				*o = []string{raw}
			}
		case []string:
			*o = raw
		}
	default:
		return fmt.Errorf("getProperty %s: unsupported type %T", name, out)
	}
	return nil
}

// nmGate bundles the NM interactions behind testable seams.
type nmGate struct {
	lastForce      time.Time
	warnedDisabled bool
}

var nmGateState = &nmGate{}

// connectivity returns the current connectivity state. Every
// forceInterval it triggers a fresh CheckConnectivity so a newly
// joined portal is noticed within a minute; between checks the cached
// property verdict is read. When the NM check is disabled entirely,
// warns once and always reports the (stale) property.
func (g *nmGate) connectivity(ctx context.Context) (int, error) {
	conn, err := realDial(ctx)
	if err != nil {
		return nmConnectivityUnknown, err
	}
	defer conn.close()
	obj := conn.objectAt(nmPath)

	var enabled bool
	if err := getProperty(obj, "", nmInterface, "ConnectivityCheckEnabled", &enabled); err == nil && !enabled {
		if !g.warnedDisabled {
			g.warnedDisabled = true
			logWarnf("NetworkManager connectivity check is disabled ([connectivity] in NetworkManager.conf): portal detection inactive")
		}
		var state uint32
		_ = getProperty(obj, "", nmInterface, "Connectivity", &state)
		return int(state), nil
	}

	now := clockNow()
	if now.Sub(g.lastForce) >= forceInterval {
		g.lastForce = now
		var res dbus.Variant
		if err := obj.Call(nmInterface+".CheckConnectivity", 0).Store(&res); err == nil {
			state, _ := res.Value().(uint32)
			return int(state), nil
		}
	}
	var state uint32
	if err := getProperty(obj, "", nmInterface, "Connectivity", &state); err != nil {
		return nmConnectivityUnknown, err
	}
	return int(state), nil
}

// nmConnectivity is a plain state read (seam kept for tests and
// future callers); the gate is the primary entry point.
var nmConnectivity = func(ctx context.Context) (int, error) {
	return nmGateState.connectivity(ctx)
}

// nmCheckURIs reads the connectivity-check probe URI(s): older NM
// exposes a single string; newer NM an array — handle both.
var nmCheckURIs = func(ctx context.Context) ([]string, error) {
	conn, err := realDial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.close()
	obj := conn.objectAt(nmPath)
	var uris []string
	if err := getProperty(obj, "", nmInterface, "ConnectivityCheckUri", &uris); err != nil {
		return nil, err
	}
	return uris, nil
}

// primaryGateway resolves the IPv4 gateway of the primary connection
// over DBus: PrimaryConnection -> Ip4Config -> Gateway. Most
// hotel/airport portals serve their login page at http://<gateway>/.
func primaryGateway(ctx context.Context) (string, error) {
	conn, err := realDial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.close()
	gw, err := gatewayOn(conn)
	if err != nil {
		return "", err
	}
	return gw, nil
}

// gatewayOn walks PrimaryConnection -> Ip4Config -> Gateway on one
// connection (also used by the fake in tests).
func gatewayOn(conn nmConn) (string, error) {
	mgr := conn.objectAt(nmPath)
	var act dbus.ObjectPath
	if err := getProperty(mgr, "", nmInterface, "PrimaryConnection", &act); err != nil {
		return "", err
	}
	if act == "" || act == "/" {
		return "", fmt.Errorf("no primary connection")
	}
	var ip4 dbus.ObjectPath
	if err := getProperty(mgr, "", nmInterface, "Ip4Config", &ip4); err != nil {
		return "", err
	}
	if ip4 == "" || ip4 == "/" {
		ac := conn.objectAt(act)
		if err := getProperty(ac, string(act), nmDest+".Connection.Active", "Ip4Config", &ip4); err != nil {
			return "", err
		}
	}
	if ip4 == "" || ip4 == "/" {
		return "", fmt.Errorf("no Ip4Config on primary connection")
	}
	ipObj := conn.objectAt(ip4)
	var gw string
	if err := getProperty(ipObj, string(ip4), nmDest+".IP4Config", "Gateway", &gw); err != nil {
		return "", err
	}
	return gw, nil
}

// httpDo is the seam used by FetchPortalURL; tests swap it out.
var httpDo = func(req *http.Request) (*http.Response, error) {
	client := &http.Client{
		Timeout: nmTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Never follow: the first redirect Location IS the portal.
			return http.ErrUseLastResponse
		},
	}
	return client.Do(req)
}

// FetchPortalURL probes checkURI without following redirects. A
// hijacked check is either a redirect (Location = portal login URL)
// or a 200 with a replaced (HTML login) body. For the body case the
// login page is usually the gateway itself, so the gateway candidate
// URL is returned.
func FetchPortalURL(ctx context.Context, checkURI string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, portalHTTPGet, checkURI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", portalUserAgent)
	resp, err := httpDo(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	loc := resp.Header.Get("Location")
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && loc != "" {
		return loc, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if isHijackedBody(resp.Header.Get("Content-Type"), body) {
			return gatewayLoginCandidate(), nil
		}
		return "", nil // portal didn't hijack this probe
	}
	return "", fmt.Errorf("portal probe: unexpected status %d", resp.StatusCode)
}

// isHijackedBody lightly inspects a 200-body: NM's check expects an
// exact plain payload ("OK" style); anything remotely shaped like an
// HTML login page counts as hijacked.
func isHijackedBody(contentType string, body []byte) bool {
	ct := strings.ToLower(contentType)
	if ct != "" && !strings.Contains(ct, "text/html") {
		return false
	}
	s := strings.ToLower(string(bytes.TrimSpace(body)))
	for _, marker := range []string{"<html", "<!doctype", "captive", "portal", "login"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// gatewayState stores the last resolved default gateway (updated on
// every portal check) for the 200-body fallback URL.
var gatewayState struct {
	mu sync.Mutex
	ip string
}

func setGatewayCandidate(ip string) {
	gatewayState.mu.Lock()
	gatewayState.ip = ip
	gatewayState.mu.Unlock()
}

func gatewayCandidate() string {
	gatewayState.mu.Lock()
	defer gatewayState.mu.Unlock()
	return gatewayState.ip
}

func resetGatewayCandidate() {
	gatewayState.mu.Lock()
	gatewayState.ip = ""
	gatewayState.mu.Unlock()
}

// gatewayLoginCandidate builds the gateway login URL when a gateway
// candidate was seen.
func gatewayLoginCandidate() string {
	if gw := gatewayCandidate(); gw != "" {
		return "http://" + gw + "/"
	}
	return ""
}

// hysteresis is how many consecutive PORTAL readings must be observed
// before tickPortal acts (protects against single flapping checks).
var portalHysteresis = 2

// CheckPortal returns the current connectivity state plus the portal
// login URL when the state reads PORTAL. On a live portal the gateway
// is resolved first so 200-body portals have a login candidate.
func CheckPortal(ctx context.Context) (int, string) {
	state, err := nmGateState.connectivity(ctx)
	if err != nil {
		return nmConnectivityUnknown, ""
	}
	if state != nmConnectivityPortal {
		return state, ""
	}
	if gw, err := primaryGateway(ctx); err == nil && gw != "" {
		setGatewayCandidate(gw)
	}
	uris, err := nmCheckURIs(ctx)
	if err != nil || len(uris) == 0 {
		return state, gatewayLoginCandidate()
	}
	url, perr := FetchPortalURL(ctx, uris[0])
	if perr != nil {
		return state, gatewayLoginCandidate()
	}
	if url != "" {
		return state, url
	}
	return state, gatewayLoginCandidate()
}

// portalStateName renders a connectivity state for logs.
func portalStateName(s int) string {
	switch s {
	case nmConnectivityPortal:
		return "portal"
	case nmConnectivityFull:
		return "full"
	case nmConnectivityLimited:
		return "limited"
	case nmConnectivityNone:
		return "none"
	default:
		return "unknown"
	}
}

// logWarnf is a seam so nm.go logging stays consistent with the app
// logger (wired in main.go to log.Printf with the traytail prefix).
var logWarnf = func(format string, args ...any) {}