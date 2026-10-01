package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// ---- fakes ----

type fakeUI struct {
	mu      sync.Mutex
	updates int
	icon    []byte
	tooltip string
	items   []MenuItem
	reg     bool
	closed  bool
}

func (f *fakeUI) Update(icon []byte, tooltip string, items []MenuItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates++
	f.icon, f.tooltip, f.items = icon, tooltip, items
}

func (f *fakeUI) RegisterWithWatcher() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reg
}

func (f *fakeUI) setReg(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reg = v
}

func (f *fakeUI) Close() { f.closed = true }

func (f *fakeUI) snapshot() (int, []byte, string, []MenuItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updates, f.icon, f.tooltip, f.items
}

// fakeCLI installs the shell fake and returns a handle; CLI failures
// surface as non-zero exits, responses are read from resp.txt.
type fakeCLI = fakeTailscale

// execRecorder captures execCommand calls.
type execRecorder struct {
	mu     sync.Mutex
	calls  [][]string
	stdins []string
}

func (r *execRecorder) record(args []string, stdin string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	r.stdins = append(r.stdins, stdin)
}

func (r *execRecorder) all() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string{}, r.calls...)
}

// newApp builds an app wired to a fake UI and cancelled context.
func newTestApp(t *testing.T, ui trayUI) *app {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	return &app{ctx: ctx, stop: stop, sni: ui, refreshCh: make(chan struct{}, 1)}
}

func restoreExec(t *testing.T) *execRecorder {
	t.Helper()
	rec := &execRecorder{}
	orig := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		rec.record(append([]string{name}, args...), "")
		return exec.Command("true")
	}
	t.Cleanup(func() { execCommand = orig })
	return rec
}

// restoreIfaceAddrs replaces the interfaceAddrs seam with a fixed
// address list (parsed from CIDR strings); nil restores "no addrs".
func restoreIfaceAddrs(t *testing.T, cidrs []string) {
	t.Helper()
	orig := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		var out []net.Addr
		for _, c := range cidrs {
			ipp, err := netip.ParsePrefix(c)
			if err != nil {
				t.Fatalf("bad cidr %q: %v", c, err)
			}
			ipnet := net.CIDRMask(ipp.Bits(), ipp.Addr().BitLen())
			out = append(out, &net.IPNet{IP: net.IP(ipp.Addr().AsSlice()), Mask: ipnet})
		}
		return out, nil
	}
	t.Cleanup(func() { interfaceAddrs = orig })
}

func restoreExecFailing(t *testing.T) *execRecorder {
	t.Helper()
	rec := &execRecorder{}
	orig := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		rec.record(append([]string{name}, args...), "")
		return exec.Command("false")
	}
	t.Cleanup(func() { execCommand = orig })
	return rec
}

// ---- buildUI ----

func TestBuildUIOnline(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running", TailscaleIPs: []string{"100.64.0.1"}}
	icon, tooltip, items := a.buildUI(st, nil)

	if tooltip == "" || items == nil {
		t.Fatalf("online build: tooltip=%q items=%d", tooltip, len(items))
	}
	if len(icon) != iconSize*iconSize*4 {
		t.Errorf("icon size = %d", len(icon))
	}
	// online menu should contain Copy IP item
	if !hasLabel(items, "Copy IP: 100.64.0.1") {
		t.Errorf("missing Copy IP item: %v", labels(items))
	}
}

func TestBuildUIOffline(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Stopped"}
	_, tooltip, items := a.buildUI(st, nil)
	if tooltip != "Tailscale: Stopped" {
		t.Errorf("tooltip = %q", tooltip)
	}
	if !hasLabel(items, "Connect") {
		t.Errorf("offline menu should offer Connect: %v", labels(items))
	}
	if !hasLabel(items, "Quit") {
		t.Error("offline menu should offer Quit")
	}
}

func TestBuildUIOfflineWithAuthURL(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	// AuthURL present means a login flow is in progress: buildUI routes
	// any non-running state with an AuthURL to the login-required menu.
	st := &Status{BackendState: "Stopped", AuthURL: "https://login.tailscale.com/a/x"}
	_, tooltip, items := a.buildUI(st, nil)
	if tooltip != "Tailscale: login required" {
		t.Errorf("tooltip = %q", tooltip)
	}
	if !hasLabel(items, "Open login page") {
		t.Errorf("AuthURL should add Open login page item: %v", labels(items))
	}
}

func TestBuildUINeedsLogin(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/x"}
	icon, tooltip, items := a.buildUI(st, nil)
	if tooltip != "Tailscale: login required" {
		t.Errorf("tooltip = %q", tooltip)
	}
	if !hasLabel(items, "Open login page") {
		t.Errorf("menu = %v", labels(items))
	}
	_ = icon
}

func TestBuildUINeedsLoginNoURL(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "NeedsLogin"}
	_, tooltip, items := a.buildUI(st, nil)
	if tooltip != "Tailscale: login required" {
		t.Errorf("tooltip = %q", tooltip)
	}
	if !hasLabel(items, "Run 'tailscale up' to log in") {
		t.Errorf("menu = %v", labels(items))
	}
}

func TestBuildUIExitNodeActive(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{
		BackendState: "Running",
		TailscaleIPs: []string{"100.64.0.1"},
		Peer: map[string]Peer{
			"e": mvPeer("de-fra-wg-001", true),
		},
	}
	icon, tooltip, items := a.buildUI(st, nil)
	// Location data present -> parent shows the full city
	if !hasLabel(items, "Exit node: fra") && !hasLabel(items, "Exit node: Berlin") {
		if !hasLabel(items, "Exit node: "+testCities["fra"]) && !hasLabel(items, "Exit node: fra") {
			t.Errorf("submenu parent should show a city-like label: %v", labels(items))
		}
	}
	// exit-node icon is the green-ring pixmap: pixel(16,1) is green
	if px := pixelAt(t, icon, 16, 1); px != [4]byte{255, 90, 200, 100} {
		t.Errorf("icon should be the exit-node ring icon, got %v at ring", px)
	}
	if tooltip == "" {
		t.Error("tooltip empty")
	}
}

func hasLabel(items []MenuItem, label string) bool {
	for _, it := range items {
		if it.Label == label {
			return true
		}
		if hasLabel(it.Submenu, label) {
			return true
		}
	}
	return false
}

func findLabel(items []MenuItem, label string) *MenuItem {
	for i := range items {
		if items[i].Label == label {
			return &items[i]
		}
		if sub := findLabel(items[i].Submenu, label); sub != nil {
			return sub
		}
	}
	return nil
}

func labels(items []MenuItem) []string {
	out := []string{}
	var walk func([]MenuItem)
	walk = func(is []MenuItem) {
		for _, it := range is {
			if it.Label != "" {
				out = append(out, it.Label)
			}
			walk(it.Submenu)
		}
	}
	walk(items)
	return out
}

// ---- menuOnline ----

func TestMenuOnlineQuitCancels(t *testing.T) {
	ui := &fakeUI{}
	a := newTestApp(t, ui)
	st := &Status{BackendState: "Running", TailscaleIPs: []string{"100.64.0.1"}}
	items := a.menuOnline(st, nil, nil)

	quit := findLabel(items, "Quit")
	if quit == nil || quit.OnClick == nil {
		t.Fatal("Quit item missing or has no handler")
	}
	quit.OnClick()
	select {
	case <-a.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Quit did not cancel context")
	}
}

func TestMenuOnlineInfoRowAndAdmin(t *testing.T) {
	rec := restoreExec(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{
		BackendState: "Running",
		TailscaleIPs: []string{"100.64.0.1"},
		Self:         Peer{HostName: "haruhi"},
	}
	items := a.menuOnline(st, nil, nil)

	// No selected profile: info row is just the hostname.
	info := findLabel(items, "haruhi")
	if info == nil {
		t.Fatalf("info row missing: %v", labels(items))
	}
	info.OnClick()
	if got := rec.all(); len(got) != 1 || got[0][1] != "https://login.tailscale.com/admin/machines" {
		t.Errorf("info click = %v", got)
	}

	admin := findLabel(items, "Admin console")
	if admin == nil {
		t.Fatal("Admin console item missing")
	}
	admin.OnClick()
	if len(rec.all()) != 2 {
		t.Errorf("admin click recorded %d calls", len(rec.all()))
	}
}

func TestMenuOnlineCopyIP(t *testing.T) {
	rec := restoreExec(t)
	// exit-node list is not installed here, so `menuOnline` construction
	// skips the CLI; nothing else should call it either.
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running", TailscaleIPs: []string{"100.64.0.2"}}
	items := a.menuOnline(st, nil, nil)
	nBaseline := len(ft.callsSoFar(t))

	copyItem := findLabel(items, "Copy IP: 100.64.0.2")
	if copyItem == nil {
		t.Fatal("Copy IP item missing")
	}
	copyItem.OnClick()
	if got := len(ft.callsSoFar(t)) - nBaseline; got != 0 {
		t.Errorf("copy should not call CLI, %d extra calls", got)
	}
	if len(rec.all()) != 1 {
		t.Errorf("copy click recorded %d exec calls", len(rec.all()))
	}
}

func TestMenuOnlineDisconnect(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running", TailscaleIPs: []string{"100.64.0.2"}}
	items := a.menuOnline(st, nil, nil)

	d := findLabel(items, "Disconnect")
	if d == nil {
		t.Fatal("Disconnect missing")
	}
	d.OnClick()
	select {
	case <-a.refreshCh:
	case <-time.After(time.Second):
		t.Error("Disconnect should request refresh")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "down" {
		t.Errorf("Disconnect ran %v", got)
	}
}

func TestMenuOnlineProfiles(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running", TailscaleIPs: []string{"100.64.0.1"}, Self: Peer{HostName: "h"}}
	profiles := []Profile{
		{ID: "p1", Nickname: "one@x", Tailnet: "x.io", Selected: true},
		{ID: "p2", Nickname: "two@y", Tailnet: "y.io"},
	}
	items := a.menuOnline(st, nil, profiles)

	profileSub := findLabel(items, "Profile: one@x")
	if profileSub == nil {
		t.Fatalf("Profile submenu missing: %v", labels(items))
	}
	active := findLabel(items, "● one@x")
	if active == nil || active.OnClick != nil {
		t.Errorf("active profile should be a radio item with nil handler: %v", labels(items))
	}
	other := findLabel(items, "○ two@y")
	if other == nil || other.OnClick == nil {
		t.Fatal("inactive profile missing click handler")
	}
	other.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	calls := ft.callsSoFar(t)
	// find the `switch p2` call among all recorded calls (menu build
	// runs `exit-node list` first, switch triggers a status refresh)
	var switchCall []string
	for i, c := range calls {
		if c[0] == "switch" && c[1] == "p2" {
			switchCall = c
			// online switch: the very next CLI call must not be `up`
			if i+1 < len(calls) && calls[i+1][0] == "up" {
				t.Errorf("online profile switch must not run `up`, calls: %v", calls)
			}
			break
		}
	}
	if switchCall == nil {
		t.Errorf("switch call missing: %v", calls)
	}
	select {
	case <-a.refreshCh:
	case <-time.After(time.Second):
		t.Error("profile switch should request refresh")
	}
}

func TestMenuOnlineNoProfilesSubmenuWhenSingle(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running", TailscaleIPs: []string{"100.64.0.1"}}
	items := a.menuOnline(st, nil, []Profile{{ID: "p1", Nickname: "only", Selected: true}})
	for _, it := range items {
		if len(it.Submenu) > 0 && it.Submenu[0].Label == "only" {
			t.Fatal("single profile should not produce a submenu")
		}
	}
}

// ---- exit node menu ----

func TestExitSuffix(t *testing.T) {
	installFakeTailscale(t) // block resolution of the real CLI
	st := &Status{BackendState: "Running"}
	if got := exitSuffix(nil, st); got != ": off" {
		t.Errorf("nil exit = %q", got)
	}
	// exit-node list unavailable (no fake installed): hostname fallback
	if got := exitSuffix(&Peer{HostName: "homeserver"}, st); got != ": homeserver" {
		t.Errorf("own node = %q", got)
	}
	if got := exitSuffix(&Peer{HostName: "de-fra-wg-001", Tags: []string{"tag:mullvad-exit-node-de"}}, st); got != ": fra" {
		t.Errorf("mullvad = %q", got)
	}
	// tagged mullvad but hostname lacks a city segment: SplitN still
	// yields two parts ("mullvad", "only"), so the label is that second
	// token, not the hostname.
	if got := exitSuffix(&Peer{HostName: "mullvad-only", Tags: []string{"tag:mullvad"}}, st); got != ": only" {
		t.Errorf("mullvad without city = %q", got)
	}
	// a hostname without dashes at all falls back to the full hostname
	if got := exitSuffix(&Peer{HostName: "mullvadsolo", Tags: []string{"tag:mullvad"}}, st); got != ": mullvadsolo" {
		t.Errorf("mullvad single-token host = %q", got)
	}
	// DNS-name-based detection (no tags) also yields the city
	if got := exitSuffix(&Peer{HostName: "nl-ams-wg-003", DNSName: "nl-ams-wg-003.mullvad.ts.net."}, st); got != ": ams" {
		t.Errorf("mullvad via DNS = %q", got)
	}
}

// nodeListFixture mirrors the earlier `tailscale exit-node list` table
// as raw status peers: own node (no Location), multi-word cities, and
// a selected Mullvad row.
func nodeListFixture() *Status {
	return &Status{
		BackendState: "Running",
		Peer: map[string]Peer{
			"own": {HostName: "zds-nabara", DNSName: "zds-nabara.tailb4e47d.ts.net.",
				ExitNodeOption: true, Online: true,
				TailscaleIPs: []string{"100.107.25.96", "fd7a::a901:199b"}},
			"mv-al": {HostName: "al-tia-wg-001", DNSName: "al-tia-wg-001.mullvad.ts.net.",
				ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"},
				TailscaleIPs: []string{"100.77.189.15"},
				Location:     &Location{Country: "Albania", CountryCode: "AL", City: "Tirana", CityCode: "TIA", Priority: 50}},
			"mv-au": {HostName: "au-adl-wg-301", DNSName: "au-adl-wg-301.mullvad.ts.net.",
				ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"},
				TailscaleIPs: []string{"100.65.216.13"},
				Location:     &Location{Country: "Australia", CountryCode: "AU", City: "Adelaide", CityCode: "ADL", Priority: 50}},
			"mv-de": {HostName: "de-ber-wg-001", DNSName: "de-ber-wg-001.mullvad.ts.net.",
				ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"},
				TailscaleIPs: []string{"100.123.112.108"},
				Location:     &Location{Country: "Germany", CountryCode: "DE", City: "Berlin", CityCode: "BER", Priority: 50},
				ExitNode:     true},
		},
	}
}

func TestExitNodeInfos(t *testing.T) {
	nodes := ExitNodeInfos(nodeListFixture())
	if len(nodes) != 4 {
		t.Fatalf("nodes = %d, want 4", len(nodes))
	}
	// SortPeers: al-tia, au-adl, de-ber, then zds-nabara ("n" < "z"... check order)
	var own, al, de *ExitNodeInfo
	for i := range nodes {
		switch {
		case nodes[i].Country == "":
			own = &nodes[i]
		case nodes[i].Country == "Albania":
			al = &nodes[i]
		case nodes[i].Country == "Germany":
			de = &nodes[i]
		}
	}
	if own == nil || own.City != "" || own.Selected {
		t.Errorf("own node wrong: %+v", own)
	}
	if own == nil || own.Hostname != "zds-nabara.tailb4e47d.ts.net" {
		t.Errorf("own hostname wrong: %+v", own)
	}
	if al == nil || al.City != "Tirana" || al.IP != "100.77.189.15" {
		t.Errorf("Albania row wrong: %+v", al)
	}
	if de == nil || !de.Selected || de.City != "Berlin" || de.Country != "Germany" {
		t.Errorf("selected row wrong: %+v", de)
	}
}

func TestExitNodeInfosEmpty(t *testing.T) {
	if got := ExitNodeInfos(&Status{}); len(got) != 0 {
		t.Errorf("empty status nodes = %v", got)
	}
}

func TestFirstIPv4(t *testing.T) {
	if got := firstIPv4([]string{"fd7a::1", "100.64.0.1/32"}); got != "100.64.0.1" {
		t.Errorf("firstIPv4 = %q", got)
	}
	if got := firstIPv4(nil); got != "" {
		t.Errorf("firstIPv4(nil) = %q", got)
	}
	if got := firstIPv4([]string{"fd7a::1"}); got != "fd7a::1" {
		t.Errorf("v6 fallback = %q", got)
	}
}

func TestSplitMullvad(t *testing.T) {
	cc, city := splitMullvad("de-fra-wg-001")
	if cc != "de" || city != "fra" {
		t.Errorf("got (%q, %q)", cc, city)
	}
	cc, city = splitMullvad("plain")
	if cc != "plain" || city != "" {
		t.Errorf("plain host: got (%q, %q)", cc, city)
	}
}

const smartAutoLabel = "Auto (smart: away→own, home→Mullvad)"

// testCities maps hostname city codes to full city names like the
// real Location data (Berlin, Tirana, ...).
var testCities = map[string]string{
	"ber": "Berlin",
	"tia": "Tirana",
	"adl": "Adelaide",
	"fra": "Frankfurt",
}

// testCountries maps country codes to full country names.
var testCountries = map[string]string{
	"de": "Germany",
	"al": "Albania",
	"au": "Australia",
}

// mvPeer builds a tagged Mullvad test peer with Location attached.
func mvPeer(host string, selected bool) Peer {
	parts := strings.SplitN(host, "-", 3)
	code := "ber"
	if len(parts) >= 2 {
		code = parts[1]
	}
	cc := parts[0]
	city, ok := testCities[code]
	if !ok {
		city = code
	}
	country, ok := testCountries[cc]
	if !ok {
		country = strings.ToUpper(cc) + "land"
	}
	p := Peer{
		HostName:       host,
		DNSName:        host + ".mullvad.ts.net.",
		ExitNodeOption: true,
		Online:         true,
		Tags:           []string{"tag:mullvad-exit-node"},
		TailscaleIPs:   []string{"100.123.112.108"},
		Location: &Location{
			Country:     country,
			CountryCode: strings.ToUpper(cc),
			City:        city,
			CityCode:    strings.ToUpper(code),
			Priority:    50,
		},
	}
	if selected {
		p.ExitNode = true
	}
	return p
}

// cityLand maps a city code to its country name used in tests.
func cityLand(s string) string { return strings.ToUpper(s) + "land" }

func TestExitNodeMenuOffAndAuto(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running", Peer: map[string]Peer{}}
	items := a.exitNodeMenu(st, nil)

	off := findLabel(items, "● Off")
	if off == nil {
		t.Fatalf("Off missing: %v", labels(items))
	}
	if !hasLabel(items, "○ "+smartAutoLabel) {
		t.Errorf("Auto missing: %v", labels(items))
	}
}

func TestExitNodeMenuOffClick(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running", Peer: map[string]Peer{}}
	items := a.exitNodeMenu(st, nil)

	off := findLabel(items, "● Off")
	if off == nil {
		t.Fatalf("Off missing: %v", labels(items))
	}
	off.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "set" || got[1] != "--exit-node=" {
		t.Errorf("off ran %v", got)
	}
	select {
	case <-a.refreshCh:
	case <-time.After(time.Second):
		t.Error("Off click should request refresh")
	}
}

func TestAutoExitNodeActive(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setDebugPrefs(t, `{"AutoExitNode":"any","ExitNodeID":"nYGkypCDoe11CNTRL"}`)
	if !AutoExitNodeActive(context.Background()) {
		t.Error("auto:any prefs should report active")
	}
	ft.setDebugPrefs(t, `{"ExitNodeID":"nYGkypCDoe11CNTRL"}`)
	if AutoExitNodeActive(context.Background()) {
		t.Error("manual selection should not report auto")
	}
	ft.setDebugPrefs(t, ``) // no prefs file -> empty output -> parse fails
	if AutoExitNodeActive(context.Background()) {
		t.Error("unparseable prefs should not report auto")
	}
}

func TestExitNodeMenuAutoMode(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true, atHome: true}
	active := mvPeer("de-ber-wg-001", true)
	st := &Status{
		BackendState: "Running",
		Peer:         map[string]Peer{"mv": active},
	}
	items := a.exitNodeMenu(st, &active)

	if !hasLabel(items, "● "+smartAutoLabel) {
		t.Errorf("Auto should be active in smart-auto mode: %v", labels(items))
	}
	if !hasLabel(items, "○ Off") {
		t.Errorf("Off should be inactive: %v", labels(items))
	}
	// smart-auto applied the node itself: city IS marked and country hoisted
	mullvad := findLabel(items, "Mullvad")
	if mullvad == nil {
		t.Fatal("Mullvad submenu missing")
	}
	if !hasLabel(mullvad.Submenu, "● Berlin") {
		t.Errorf("applied city should be marked in smart-auto mode: %v", labels(mullvad.Submenu))
	}
}

func TestExitNodeMenuAutoParentLabel(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true, atHome: true}
	active := mvPeer("de-ber-wg-001", true)
	st := &Status{BackendState: "Running", Peer: map[string]Peer{"mv": active}}
	_, _, menu := a.buildUI(st, nil)
	// parent shows the applied node's city, not "auto"
	if !hasLabel(menu, "Exit node: Berlin") {
		t.Errorf("parent label in smart-auto mode: %v", labels(menu))
	}
}

func TestExitNodeMenuAutoClickEnables(t *testing.T) {
	ft := installFakeTailscale(t)
	// no local IPs in the advertised LANs -> "away" -> should pick own node
	restoreIfaceAddrs(t, nil)
	// status resp must include the own node so enableSmartAuto finds it
	ft.setResponse(t, `{"BackendState":"Running","Peer":{"own":{"HostName":"zds-nabara","DNSName":"zds-nabara.tailb4e47d.ts.net.","ExitNodeOption":true,"Online":true,"PrimaryRoutes":["192.168.178.0/24"]}}}`)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{}
	own := Peer{HostName: "zds-nabara", DNSName: "zds-nabara.tailb4e47d.ts.net.", ExitNodeOption: true, Online: true, PrimaryRoutes: []string{"192.168.178.0/24"}}
	st := &Status{
		BackendState: "Running",
		Peer:         map[string]Peer{"own": own},
	}
	items := a.exitNodeMenu(st, nil)

	auto := findLabel(items, "○ "+smartAutoLabel)
	if auto == nil {
		t.Fatalf("Auto missing: %v", labels(items))
	}
	auto.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "set" || got[1] != "--exit-node=zds-nabara" {
		t.Errorf("smart auto enable ran %v, want own node (away)", got)
	}
	if !a.smartPrimed() {
		t.Error("smart auto should be primed after enabling")
	}
}

func TestExitNodeMenuOwnAndMullvad(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{
		BackendState: "Running",
		Peer: map[string]Peer{
			"own":  {HostName: "zds-nabara", DNSName: "zds-nabara.tailb4e47d.ts.net.", ExitNodeOption: true, Online: true, TailscaleIPs: []string{"100.107.25.96"}},
			"mv-a": mvPeer("al-tia-wg-001", false),
			"mv-b": mvPeer("de-ber-wg-001", false),
			"off":  {HostName: "offline-node", ExitNodeOption: true, Online: false},
			"noex": {HostName: "plain", Online: true},
		},
	}
	items := a.exitNodeMenu(st, nil)

	if !hasLabel(items, "○ zds-nabara") {
		t.Errorf("own node missing: %v", labels(items))
	}
	if !hasLabel(items, "Mullvad") {
		t.Fatalf("Mullvad submenu missing: %v", labels(items))
	}
	mullvad := findLabel(items, "Mullvad")
	if !hasLabel(mullvad.Submenu, "Albania") || !hasLabel(mullvad.Submenu, "Germany") {
		t.Errorf("full country names missing: %v", labels(mullvad.Submenu))
	}
	if hasLabel(items, "offline-node") || hasLabel(items, "plain") {
		t.Errorf("offline / non-exit peers leaked into menu: %v", labels(items))
	}
	germany := findLabel(mullvad.Submenu, "Germany")
	if germany == nil {
		t.Fatal("Germany submenu missing")
	}
}

func TestExitNodeMenuActiveCountryHoisted(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	active := mvPeer("de-ber-wg-001", true)
	st := &Status{
		BackendState: "Running",
		Peer:         map[string]Peer{"mv": active},
	}
	items := a.exitNodeMenu(st, &active)

	mullvad := findLabel(items, "Mullvad")
	if mullvad == nil {
		t.Fatal("Mullvad submenu missing")
	}
	first := mullvad.Submenu[0]
	if first.Label != "● Germany" {
		t.Errorf("active country should be hoisted with ●, got %q (all: %v)", first.Label, labels(mullvad.Submenu))
	}
	berlin := findLabel(mullvad.Submenu, "● Berlin")
	if berlin == nil {
		t.Errorf("active city should carry ● marker: %v", labels(mullvad.Submenu))
	}
}

func TestExitNodeMenuParentLabelCity(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	active := mvPeer("de-ber-wg-001", true)
	st := &Status{
		BackendState: "Running",
		Peer:         map[string]Peer{"mv": active},
	}
	_, _, menu := a.buildUI(st, nil)
	if !hasLabel(menu, "Exit node: Berlin") {
		t.Errorf("parent should show full city: %v", labels(menu))
	}
}

func TestExitNodeMenuOwnActiveChecked(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	active := Peer{HostName: "zds-nabara", DNSName: "zds-nabara.tailb4e47d.ts.net.", ExitNodeOption: true, Online: true, ExitNode: true}
	st := &Status{BackendState: "Running", Peer: map[string]Peer{"own": active}}
	items := a.exitNodeMenu(st, &active)
	if !hasLabel(items, "● zds-nabara") {
		t.Errorf("active own node should have ● marker: %v", labels(items))
	}
}

func TestExitNodeMenuCityClickSetsExitNode(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{
		BackendState: "Running",
		Peer: map[string]Peer{
			"mv-a": mvPeer("al-tia-wg-001", false),
		},
	}
	items := a.exitNodeMenu(st, nil)

	mullvad := findLabel(items, "Mullvad")
	if mullvad == nil {
		t.Fatalf("Mullvad submenu missing: %v", labels(items))
	}
	albania := findLabel(mullvad.Submenu, "Albania")
	if albania == nil {
		t.Fatalf("ALland missing: %v", labels(mullvad.Submenu))
	}
	tirana := findLabel(albania.Submenu, "○ Tirana")
	if tirana == nil {
		t.Fatalf("Tirana item missing: %v", labels(albania.Submenu))
	}
	tirana.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "set" || got[1] != "--exit-node=al-tia-wg-001.mullvad.ts.net" {
		t.Errorf("city click ran %v", got)
	}
}

func TestExitNodeMenuNoNodesPlaceholder(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running", Peer: map[string]Peer{}}
	items := a.exitNodeMenu(st, nil)
	if !hasLabel(items, "● Off") || !hasLabel(items, "○ "+smartAutoLabel) {
		t.Fatalf("Off/Auto should always be present: %v", labels(items))
	}
	if hasLabel(items, "Mullvad") {
		t.Errorf("Mullvad submenu without any nodes: %v", labels(items))
	}
}

// ---- listRowActive + own-item deselect ----

// TestListRowActiveByIP covers the renamed-device case: the status
// peer's HostName is user-chosen ("Nabara") while the exit-node list
// row carries the DNS name ("zds-nabara...."); only the Tailscale IP
// links them.
func TestListRowActiveByIP(t *testing.T) {
	row := ExitNodeInfo{IP: "100.107.25.96", Hostname: "zds-nabara.tailb4e47d.ts.net", Country: "", City: ""}
	renamed := Peer{HostName: "Nabara", DNSName: "zds-nabara.tailb4e47d.ts.net.", TailscaleIPs: []string{"100.107.25.96", "fd7a::a901:199b"}}

	if !listRowActive(row, &renamed) {
		t.Error("same Tailscale IP should match despite renamed HostName")
	}
	other := Peer{HostName: "Nabara", TailscaleIPs: []string{"100.99.99.99"}}
	if listRowActive(row, &other) {
		t.Error("different IP with unmatchable hostname should not match")
	}
	if listRowActive(row, nil) {
		t.Error("nil exit peer should never match")
	}
}

func TestListRowActiveHostnameFallback(t *testing.T) {
	// No IP in the row (older parse): hostname fallback still works.
	row := ExitNodeInfo{Hostname: "de-ber-wg-001.mullvad.ts.net"}
	p := Peer{HostName: "de-ber-wg-001", DNSName: "de-ber-wg-001.mullvad.ts.net."}
	if !listRowActive(row, &p) {
		t.Error("hostname fallback should match")
	}
}

func TestPeerExitAvailableIP(t *testing.T) {
	st := &Status{Peer: map[string]Peer{
		"a": {HostName: "Node-renamed", DNSName: "something-else.ts.net.", ExitNodeOption: true, Online: true, TailscaleIPs: []string{"100.20.30.40/32"}},
		"b": {HostName: "b", ExitNodeOption: true, Online: false}, // offline -> never matches
	}}

	// IP match despite unrelated names
	if !peerExitAvailableIP(st, ExitNodeInfo{IP: "100.20.30.40", Hostname: "unmatchable.example.com"}) {
		t.Error("IP match should work despite unrelated hostname")
	}
	// hostname match unaffected
	if !peerExitAvailableIP(st, ExitNodeInfo{Hostname: "Node-renamed"}) {
		t.Error("plain hostname match should still work")
	}
	// no IP in row, no hostname match
	if peerExitAvailableIP(st, ExitNodeInfo{Hostname: "ghost"}) {
		t.Error("no IP + hostname mismatch should not match")
	}
	// offline peer with same IP must not count
	off := &Status{Peer: map[string]Peer{"b": st.Peer["b"]}}
	if peerExitAvailableIP(off, ExitNodeInfo{IP: "100.99.0.1", Hostname: "b"}) {
		// "b" hostname matches (peer is offline) -> false via peerExitAvailable gate
		t.Error("offline peer should never report available")
	}
	// offline peer, IP-only path
	off2 := &Status{Peer: map[string]Peer{"off": {HostName: "x", ExitNodeOption: true, Online: false, TailscaleIPs: []string{"100.1.1.1"}}}}
	if peerExitAvailableIP(off2, ExitNodeInfo{IP: "100.1.1.1", Hostname: "unrelated"}) {
		t.Error("offline peer IP should not match")
	}
}

func TestExitNodeMenuOwnActiveByIP(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	// Renamed device: HostName "Nabara", same IP as the list row.
	active := Peer{
		HostName:       "Nabara",
		DNSName:        "zds-nabara.tailb4e47d.ts.net.",
		ExitNodeOption: true,
		Online:         true,
		ExitNode:       true,
		TailscaleIPs:   []string{"100.107.25.96"},
	}
	st := &Status{BackendState: "Running", Peer: map[string]Peer{"own": active}}
	items := a.exitNodeMenu(st, &active)
	if !hasLabel(items, "● zds-nabara") {
		t.Errorf("renamed own node should be marked active via IP: %v", labels(items))
	}
}

func TestOwnExitItemClickDeselectsToMullvad(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ft := installFakeTailscale(t)
	// status incl. online Mullvad peer + own node; no debug prefs needed
	ft.setResponse(t, `{"BackendState":"Running","Peer":{
		"own":{"HostName":"Nabara","DNSName":"zds-nabara.tailb4e47d.ts.net.","ExitNodeOption":true,"Online":true,"TailscaleIPs":["100.107.25.96"]},
		"mv":{"HostName":"al-tia-wg-001","DNSName":"al-tia-wg-001.mullvad.ts.net.","ExitNodeOption":true,"Online":true,"Tags":["tag:mullvad-exit-node"]}
	}}`)
	restoreIfaceAddrs(t, nil)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true}
	active := Peer{HostName: "Nabara", DNSName: "zds-nabara.tailb4e47d.ts.net.", ExitNodeOption: true, Online: true, ExitNode: true, TailscaleIPs: []string{"100.107.25.96"}}
	st := &Status{BackendState: "Running", Peer: map[string]Peer{"own": active}}
	items := a.exitNodeMenu(st, &active)

	ownItem := findLabel(items, "● zds-nabara")
	if ownItem == nil {
		t.Fatalf("active own item missing: %v", labels(items))
	}
	ownItem.OnClick()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	var setCall []string
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "set" {
			setCall = c
		}
	}
	if setCall == nil || setCall[1] != "--exit-node=al-tia-wg-001" {
		t.Fatalf("deselect own should apply last-used Mullvad, ran %v", ft.callsSoFar(t))
	}
	if a.smart != nil {
		t.Error("deselect should disable smart auto")
	}
	if got := LoadLastMullvad(); got != "al-tia-wg-001" {
		t.Errorf("last-used should be persisted, got %q", got)
	}
}

func TestOwnExitItemClickInactiveSelects(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true}
	st := &Status{
		BackendState: "Running",
		Peer: map[string]Peer{
			"own": {HostName: "Nabara", DNSName: "zds-nabara.tailb4e47d.ts.net.", ExitNodeOption: true, Online: true, TailscaleIPs: []string{"100.107.25.96"}},
		},
	}
	items := a.exitNodeMenu(st, nil) // no active exit

	ownItem := findLabel(items, "○ zds-nabara")
	if ownItem == nil {
		t.Fatalf("inactive own item missing: %v", labels(items))
	}
	ownItem.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	var setCall []string
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "set" {
			setCall = c
		}
	}
	if setCall == nil || setCall[1] != "--exit-node=zds-nabara.tailb4e47d.ts.net" {
		t.Fatalf("inactive own click should select the node, ran %v", ft.callsSoFar(t))
	}
	if a.smart != nil {
		t.Error("manual select should disable smart auto")
	}
}

func TestApplyLastMullvadNoNodeAvailable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ft := installFakeTailscale(t)
	// status with no online Mullvad peers
	ft.setResponse(t, `{"BackendState":"Running","Peer":{}}`)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true}
	if got := a.applyLastMullvad(); got != "" {
		t.Errorf("applyLastMullvad with no nodes = %q, want empty", got)
	}
	if a.smart == nil {
		t.Error("unavailable target should not disable smart auto")
	}
}

// ---- captive portal ----

// fakeNMObj answers NM property reads and CheckConnectivity calls.
type fakeNMObj struct {
	state    uint32
	checkOn  bool
	checkURI string
	primary  dbus.ObjectPath
	ip4      dbus.ObjectPath // manager-level Ip4Config answer
	ip4AC    dbus.ObjectPath // ActiveConnection-level Ip4Config answer
	gateway  string
	forceCalled bool
}

func (o *fakeNMObj) Call(method string, flags dbus.Flags, args ...any) *dbus.Call {
	v := dbus.MakeVariant("")
	switch {
	case method == propInterface+".Get" && args[1] == "Connectivity":
		v = dbus.MakeVariant(o.state)
	case method == propInterface+".Get" && args[1] == "ConnectivityCheckEnabled":
		v = dbus.MakeVariant(o.checkOn)
	case method == propInterface+".Get" && args[1] == "ConnectivityCheckUri":
		v = dbus.MakeVariant(o.checkURI)
	case method == propInterface+".Get" && args[1] == "PrimaryConnection":
		v = dbus.MakeVariant(o.primary)
	case method == propInterface+".Get" && args[1] == "Ip4Config":
		v = dbus.MakeVariant(o.ip4)
	case method == propInterface+".Get" && args[1] == "Gateway":
		v = dbus.MakeVariant(o.gateway)
	case method == nmInterface+".CheckConnectivity":
		o.forceCalled = true
		v = dbus.MakeVariant(o.state)
	}
	// Call Store(&variantOut) semantics: body = one variant
	return &dbus.Call{Body: []any{dbus.MakeVariant(v.Value())}, Err: nil}
}

// fakeNMConn resolves manager + config objects.
type fakeNMConn struct {
	obj *fakeNMObj
}

func (c *fakeNMConn) objectAt(path dbus.ObjectPath) nmObject {
	if path == nmPath {
		return &nmProxyObj{host: c.obj}
	}
	// Ip4Config or ActiveConnection path: gateway lives here
	return &cfgObj{host: c.obj}
}

func (c *fakeNMConn) close() error { return nil }

// nmProxyObj serves manager-level properties; PrimaryConnection and
// Ip4Config reads are answered at manager level (property cache).
type nmProxyObj struct {
	host *fakeNMObj
}

func (o *nmProxyObj) Call(method string, flags dbus.Flags, args ...any) *dbus.Call {
	if method == propInterface+".Get" && len(args) > 1 {
		switch args[1] {
		case "PrimaryConnection":
			return &dbus.Call{Body: []any{dbus.MakeVariant(dbus.MakeVariant(o.host.primary).Value())}, Err: nil}
		case "Ip4Config":
			return &dbus.Call{Body: []any{dbus.MakeVariant(dbus.MakeVariant(o.host.ip4).Value())}, Err: nil}
		}
	}
	return o.host.Call(method, flags, args...)
}

// cfgObj serves Ip4Config/ActiveConnection object paths (Gateway and
// the AC-level Ip4Config used by the fallback walk).
type cfgObj struct {
	host *fakeNMObj
}

func (o *cfgObj) Call(method string, flags dbus.Flags, args ...any) *dbus.Call {
	if method == propInterface+".Get" && len(args) > 1 {
		switch args[1] {
		case "Gateway":
			return &dbus.Call{Body: []any{dbus.MakeVariant(dbus.MakeVariant(o.host.gateway).Value())}, Err: nil}
		case "Ip4Config":
			return &dbus.Call{Body: []any{dbus.MakeVariant(dbus.MakeVariant(o.host.ip4AC).Value())}, Err: nil}
		}
	}
	return &dbus.Call{Err: fmt.Errorf("fakeNM: unhandled %s %v", method, args)}
}

// restoreNM swaps the NM gate with the fake bus; hysteresis is forced
// to 2 (production value) but the throttle reset lets fresh checks run
// immediately. Returns the fake handle for assertions.
func restoreNM(t *testing.T, state int, checkURI string, probeURL string, probeErr error) *fakeNMObj {
	t.Helper()
	obj := &fakeNMObj{
		state:    uint32(state),
		checkOn:  true,
		checkURI: checkURI,
		primary:  dbus.ObjectPath("/org/freedesktop/NetworkManager/ActiveConnection/1"),
		ip4:      dbus.ObjectPath("/org/freedesktop/NetworkManager/IP4Config/1"),
		gateway:  "10.1.32.1",
	}
	conn := &fakeNMConn{obj: obj}
	origDial, origURIs, origHTTP, origHy, origClock := realDial, nmCheckURIs, httpDo, portalHysteresis, clockNow
	realDial = func(context.Context) (nmConn, error) { return conn, nil }
	nmCheckURIs = func(context.Context) ([]string, error) { return []string{checkURI}, nil }
	httpDo = func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{probeURL}}, Body: http.NoBody}, probeErr
	}
	portalHysteresis = 2
	resetGatewayCandidate()
	t.Cleanup(func() {
		realDial, nmCheckURIs, httpDo, portalHysteresis, clockNow = origDial, origURIs, origHTTP, origHy, origClock
		resetGatewayCandidate()
	})
	return obj
}

func TestCheckPortalClear(t *testing.T) {
	restoreNM(t, nmConnectivityFull, "http://check.example/ok", "", nil)
	state, url := CheckPortal(context.Background())
	if state != nmConnectivityFull || url != "" {
		t.Errorf("full state = (%d,%q)", state, url)
	}
}

func TestCheckPortalWithURL(t *testing.T) {
	restoreNM(t, nmConnectivityPortal, "http://check.example/ok", "http://portal.example/login", nil)
	state, url := CheckPortal(context.Background())
	if state != nmConnectivityPortal || url != "http://portal.example/login" {
		t.Errorf("portal state = (%d,%q)", state, url)
	}
}

func TestCheckPortalProbeError(t *testing.T) {
	// Portal present, probe fails -> state reported, gateway fallback URL
	restoreNM(t, nmConnectivityPortal, "http://check.example/ok", "", errors.New("timeout"))
	state, url := CheckPortal(context.Background())
	if state != nmConnectivityPortal {
		t.Errorf("state = %d, want portal", state)
	}
	if url != "http://10.1.32.1/" {
		t.Errorf("probe failure should fall back to gateway, got %q", url)
	}
}

func TestNMPortabilityStringURI(t *testing.T) {
	// older NM: ConnectivityCheckUri is a plain string
	restoreNM(t, nmConnectivityPortal, "", "", nil)
	orig := nmCheckURIs
	nmCheckURIs = func(context.Context) ([]string, error) { return orig(context.Background()) }
	// simulate string-typed value by direct probe: covered via restoreNM checkURI arg
	uris, err := nmCheckURIs(context.Background())
	if err != nil || len(uris) != 1 {
		t.Errorf("uris = %v err=%v", uris, err)
	}
}

func TestPortalStateNames(t *testing.T) {
	cases := map[int]string{
		nmConnectivityPortal:  "portal",
		nmConnectivityFull:    "full",
		nmConnectivityLimited: "limited",
		nmConnectivityNone:    "none",
		nmConnectivityUnknown: "unknown",
	}
	for s, want := range cases {
		if got := portalStateName(s); got != want {
			t.Errorf("portalStateName(%d) = %q, want %q", s, got, want)
		}
	}
}

func TestTickPortalDisconnectsOnPortal(t *testing.T) {
	ft := installFakeTailscale(t)
	restoreNM(t, nmConnectivityPortal, "http://check/x", "http://portal/login", nil)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true}
	st := &Status{BackendState: "Running"}

	a.tickPortal(st) // arms (hysteresis streak 1), no action yet
	if detected, _ := a.portalInfo(); detected {
		t.Error("single portal reading must not trigger detection (hysteresis)")
	}
	a.tickPortal(st) // second reading -> detect + disconnect
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "down" {
		t.Errorf("portal should disconnect, ran %v", ft.callsSoFar(t))
	}
	detected, url := a.portalInfo()
	if !detected || url != "http://portal/login" {
		t.Errorf("portalInfo = (%v,%q)", detected, url)
	}
}

func TestTickPortalDoesNotReDisconnect(t *testing.T) {
	ft := installFakeTailscale(t)
	restoreNM(t, nmConnectivityPortal, "http://check/x", "http://portal/login", nil)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running"}

	a.tickPortal(st) // arm: no disconnect yet
	a.tickPortal(st) // detect + disconnect
	nAfterDetect := len(ft.callsSoFar(t))
	a.tickPortal(&Status{BackendState: "Stopped"}) // persisting portal: no extra calls
	n := len(ft.callsSoFar(t))
	if n != nAfterDetect {
		t.Errorf("portal persist should be deduped: %v", ft.callsSoFar(t))
	}
}

func TestTickPortalReupsOnClear(t *testing.T) {
	ft := installFakeTailscale(t)
	restoreNM(t, nmConnectivityPortal, "http://check/x", "http://portal/login", nil)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running"}
	a.tickPortal(st) // arm (hysteresis)
	a.tickPortal(st) // detect + disconnect + mark reup

	// clear: NM says full; tailscale still reports Stopped (down happened)
	restoreNM(t, nmConnectivityFull, "http://check/x", "", nil)
	a.tickPortal(&Status{BackendState: "Stopped"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	calls := ft.callsSoFar(t)
	var sawDown, sawUp bool
	for _, c := range calls {
		if c[0] == "down" {
			sawDown = true
		}
		if c[0] == "up" {
			sawUp = true
		}
	}
	if !sawDown || !sawUp {
		t.Errorf("portal cycle should down+up, calls: %v", calls)
	}
	detected, _ := a.portalInfo()
	if detected {
		t.Error("portal should be cleared")
	}
}

func TestTickPortalClearNoReupWithoutNeed(t *testing.T) {
	ft := installFakeTailscale(t)
	// portal never detected; connectivity fine; tailscale stopped by user choice
	restoreNM(t, nmConnectivityFull, "http://check/x", "", nil)
	a := newTestApp(t, &fakeUI{})
	a.tickPortal(&Status{BackendState: "Stopped"})
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "up" {
			t.Errorf("no reup without a portal cycle: %v", ft.callsSoFar(t))
		}
	}
}

func TestMenuPortalItems(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Stopped"}
	items := a.menuPortal(st, "http://portal/login", nil)

	if !hasLabel(items, "Wi-Fi captive portal detected") {
		t.Fatalf("portal banner missing: %v", labels(items))
	}
	open := findLabel(items, "Open portal login")
	if open == nil {
		t.Fatal("Open portal login missing")
	}
	retry := findLabel(items, "Retry connectivity check")
	if retry == nil {
		t.Fatal("Retry missing")
	}
}

func TestMenuPortalNoURL(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Stopped"}
	items := a.menuPortal(st, "", nil)
	if !hasLabel(items, "No login URL discovered yet") {
		t.Errorf("no-URL placeholder missing: %v", labels(items))
	}
	if hasLabel(items, "Open portal login") {
		t.Errorf("Open portal login must not exist without a URL: %v", labels(items))
	}
}

func TestMenuPortalOpenClicksURL(t *testing.T) {
	rec := restoreExec(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Stopped"}
	items := a.menuPortal(st, "http://portal/login", nil)

	open := findLabel(items, "Open portal login")
	open.OnClick()
	got := rec.all()
	if len(got) != 1 || got[0][0] != "chromium-browser" || got[0][1] != "http://portal/login" {
		t.Errorf("open portal click = %v", got)
	}
}

func TestBuildUIPortalBeatsOtherStates(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	a.portal.mu.Lock()
	a.portal.detected = true
	a.portal.url = "http://portal/login"
	a.portal.mu.Unlock()

	// even when running, portal takes over the menu
	st := &Status{BackendState: "Running", TailscaleIPs: []string{"100.64.0.1"}}
	icon, tooltip, items := a.buildUI(st, nil)
	if tooltip != "Tailscale: Wi-Fi captive portal" {
		t.Errorf("tooltip = %q", tooltip)
	}
	if !hasLabel(items, "Open portal login") {
		t.Errorf("portal menu missing: %v", labels(items))
	}
	_ = icon
}

func TestPortalKeyInDedup(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	if k := portalKey(a); k != "" {
		t.Errorf("clean portalKey = %q", k)
	}
	a.portal.mu.Lock()
	a.portal.detected = true
	a.portal.url = "http://x"
	a.portal.mu.Unlock()
	if k := portalKey(a); k != "portal:http://x" {
		t.Errorf("portalKey = %q", k)
	}
}

func TestFetchPortalURLFollowsNMChain(t *testing.T) {
	// Direct seam test: 302 with Location -> URL; 200 -> empty
	orig := httpDo
	httpDo = func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://portal/pwn"}}, Body: http.NoBody}, nil
	}
	url, err := FetchPortalURL(context.Background(), "http://check/x")
	if err != nil || url != "https://portal/pwn" {
		t.Errorf("redirect probe = (%q,%v)", url, err)
	}
	httpDo = func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
	}
	url, err = FetchPortalURL(context.Background(), "http://check/x")
	if err != nil || url != "" {
		t.Errorf("clean probe = (%q,%v)", url, err)
	}
	httpDo = orig
}

// ---- portal improvements: throttle, body-sniff fallback, hysteresis ----

func TestCheckConnectivityForcedThrottled(t *testing.T) {
	obj := restoreNM(t, nmConnectivityFull, "http://check/x", "", nil)
	nmGateState.lastForce = time.Time{} // force a fresh run

	now := time.Now()
	origClock := clockNow
	clockNow = func() time.Time { return now }
	defer func() { clockNow = origClock }()

	// first call: throttled fresh check happens
	force1 := obj.forceCalled
	if _, err := nmGateState.connectivity(context.Background()); err != nil {
		t.Fatalf("connectivity: %v", err)
	}
	if !force1 && !obj.forceCalled {
		t.Fatal("first call should trigger CheckConnectivity")
	}
	forcedAfterFirst := obj.forceCalled

	// second call within forceInterval: cached property read, no fresh call
	obj.forceCalled = false
	clockNow = func() time.Time { return now.Add(10 * time.Second) }
	if _, err := nmGateState.connectivity(context.Background()); err != nil {
		t.Fatalf("connectivity 2: %v", err)
	}
	if obj.forceCalled {
		t.Error("throttle must suppress the second fresh check within interval")
	}

	// call after forceInterval: fresh check again
	obj.forceCalled = false
	clockNow = func() time.Time { return now.Add(forceInterval + time.Second) }
	if _, err := nmGateState.connectivity(context.Background()); err != nil {
		t.Fatalf("connectivity 3: %v", err)
	}
	if !obj.forceCalled {
		t.Error("call after forceInterval should force a fresh check")
	}
	_ = forcedAfterFirst
}

func TestPortalDisabledWarnsOnce(t *testing.T) {
	obj := restoreNM(t, nmConnectivityUnknown, "", "", nil)
	obj.checkOn = false

	var warns int
	origWarn := logWarnf
	logWarnf = func(string, ...any) { warns++ }
	defer func() { logWarnf = origWarn }()

	for i := 0; i < 3; i++ {
		state, err := nmGateState.connectivity(context.Background())
		if err != nil {
			t.Fatalf("connectivity: %v", err)
		}
		if state != nmConnectivityUnknown {
			t.Errorf("disabled check should degrade to property, got %d", state)
		}
	}
	if warns != 1 {
		t.Errorf("warned %d times, want exactly 1", warns)
	}
}

func TestPortalHysteresisFlapResists(t *testing.T) {
	ft := installFakeTailscale(t)
	restoreNM(t, nmConnectivityPortal, "http://check/x", "http://portal/login", nil)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running"}

	// flapping: portal, full, portal, full — nothing should ever fire
	restoreNM(t, nmConnectivityPortal, "http://check/x", "http://portal/login", nil)
	a.tickPortal(st)
	restoreNM(t, nmConnectivityFull, "http://check/x", "", nil)
	a.tickPortal(st)
	restoreNM(t, nmConnectivityPortal, "http://check/x", "http://portal/login", nil)
	a.tickPortal(st)
	restoreNM(t, nmConnectivityFull, "http://check/x", "", nil)
	a.tickPortal(st)

	for _, c := range ft.callsSoFar(t) {
		if c[0] == "down" {
			t.Errorf("flapping readings must never disconnect: %v", ft.callsSoFar(t))
		}
	}
	if detected, _ := a.portalInfo(); detected {
		t.Error("flapping must not leave portal detected")
	}
}

func TestHysteresisConfigurable(t *testing.T) {
	ft := installFakeTailscale(t)
	obj := restoreNM(t, nmConnectivityPortal, "http://check/x", "http://portal/login", nil)
	obj.state = nmConnectivityPortal
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running"}

	portalHysteresis = 3
	a.tickPortal(st) // streak 1
	a.tickPortal(st) // streak 2
	if detected, _ := a.portalInfo(); detected {
		t.Error("streak 2 must not trigger with hysteresis 3")
	}
	a.tickPortal(st) // streak 3 -> fire
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	var sawDown bool
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "down" {
			sawDown = true
		}
	}
	if !sawDown {
		t.Errorf("streak 3 should trigger disconnect, calls: %v", ft.callsSoFar(t))
	}
}

func TestGatewayFallbackRedirect(t *testing.T) {
	// Portal probe returns NO redirect and a plain 200 body, but NM's
	// verdict is PORTAL: the gateway login candidate is used anyway
	// (NM saw the hijack; our probe just failed to reproduce it).
	restoreNM(t, nmConnectivityPortal, "http://check/x", "", nil)
	origHTTP := httpDo
	httpDo = func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody}, nil
	}
	defer func() { httpDo = origHTTP }()

	state, url := CheckPortal(context.Background())
	if state != nmConnectivityPortal {
		t.Errorf("state = %d", state)
	}
	if url != "http://10.1.32.1/" {
		t.Errorf("NM verdict PORTAL should yield gateway candidate, got %q", url)
	}
	// without a gateway resolved, the candidate stays empty
	resetGatewayCandidate()
}

func TestGatewayFallbackBodySniff(t *testing.T) {
	// Portal serves HTML on the check URI without redirect: full
	// CheckPortal resolves the gateway over DBus and returns its
	// login URL.
	obj := restoreNM(t, nmConnectivityPortal, "http://check/x", "", nil)

	origHTTP := httpDo
	httpDo = func(req *http.Request) (*http.Response, error) {
		body := `<!DOCTYPE html><html><head><title>Hotel WiFi Login</title></head><body>Please sign in</body></html>`
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
	defer func() { httpDo = origHTTP }()

	state, url := CheckPortal(context.Background())
	if state != nmConnectivityPortal {
		t.Errorf("state = %d, want portal", state)
	}
	_ = obj
	// gateway candidate must come from the NM Gateway property (fake: 10.1.32.1)
	if url != "http://10.1.32.1/" {
		t.Errorf("body-sniff portal should resolve gateway URL, got %q", url)
	}
}

func TestIsHijackedBody(t *testing.T) {
	cases := []struct {
		ct   string
		body string
		want bool
	}{
		{"text/plain", "OK", false},
		{"application/json", `{"status":"ok"}`, false},
		{"text/html", "<html><body>login</body></html>", true},
		{"", "<html>wifi</html>", true},
		{"", "OK", false},
		{"text/html", "<!doctype html><body><h1>Guest Portal</h1></body>", true},
	}
	for i, tc := range cases {
		if got := isHijackedBody(tc.ct, []byte(tc.body)); got != tc.want {
			t.Errorf("case %d: isHijackedBody(%q) = %v, want %v", i, tc.body, got, tc.want)
		}
	}
}

func TestGatewayResolutionChained(t *testing.T) {
	// Full chain: NM PrimaryConnection -> Ip4Config -> Gateway
	obj := restoreNM(t, nmConnectivityPortal, "http://check/x", "", nil)
	gw, err := primaryGateway(context.Background())
	if err != nil {
		t.Fatalf("primaryGateway: %v", err)
	}
	if gw != "10.1.32.1" {
		t.Errorf("gateway = %q, want from fake", gw)
	}
	// manager-level Ip4Config missing -> ActiveConnection fallback path
	obj.ip4 = ""
	setGatewayCandidate("")
	if _, err := FetchPortalURL(context.Background(), "http://check/x"); err != nil {
		t.Log(err) // informational
	}
}

func TestGatewayOnNoPrimary(t *testing.T) {
	obj := restoreNM(t, nmConnectivityFull, "http://check/x", "", nil)
	obj.primary = "" // no active connection
	if _, err := primaryGateway(context.Background()); err == nil {
		t.Error("missing primary connection should error")
	}
	// Manager-level Ip4Config empty -> gatewayOn falls back to the
	// ActiveConnection object (AC-level Ip4Config answer).
	obj.primary = dbus.ObjectPath("/nm/AC")
	obj.ip4 = ""
	obj.ip4AC = dbus.ObjectPath("/nm/IP4")
	obj.gateway = "10.1.32.1"
	gw, err := primaryGateway(context.Background())
	if err != nil {
		t.Fatalf("gateway via ActiveConnection fallback: %v", err)
	}
	if gw != "10.1.32.1" {
		t.Errorf("gateway = %q", gw)
	}
}

func TestMenuPortalRetryClick(t *testing.T) {
	// Retry click while portal absent + no URL: no crash, refresh requested
	restoreNM(t, nmConnectivityFull, "http://check/x", "", nil)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running"}
	items := a.menuPortal(st, "", nil)

	retry := findLabel(items, "Retry connectivity check")
	retry.OnClick()
	select {
	case <-a.refreshCh:
	case <-time.After(time.Second):
		t.Error("retry should request refresh")
	}
}

func TestMenuPortalProfilesIncluded(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Stopped"}
	profiles := []Profile{
		{ID: "p1", Nickname: "one@x", Selected: true},
		{ID: "p2", Nickname: "two@y"},
	}
	items := a.menuPortal(st, "", profiles)
	if !hasLabel(items, "Profile: one@x") {
		t.Errorf("portal menu should keep the profile submenu: %v", labels(items))
	}
	quit := findLabel(items, "Quit")
	if quit == nil || quit.OnClick == nil {
		t.Fatal("portal menu should offer Quit")
	}
	quit.OnClick()
	select {
	case <-a.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("portal Quit did not cancel context")
	}
}

func TestGatewayLoginCandidateEmpty(t *testing.T) {
	resetGatewayCandidate()
	if got := gatewayLoginCandidate(); got != "" {
		t.Errorf("no gateway candidate = %q", got)
	}
}

func TestPortalRetryForcesFresh(t *testing.T) {
	// Retry entry: force a fresh CheckConnectivity by clearing the throttle
	obj := restoreNM(t, nmConnectivityFull, "http://check/x", "", nil)
	obj.forceCalled = false
	nmGateState.lastForce = time.Time{}
	if _, err := nmGateState.connectivity(context.Background()); err != nil {
		t.Fatalf("connectivity: %v", err)
	}
	if !obj.forceCalled {
		t.Error("retry (throttle cleared) should force a fresh check")
	}
}

func TestFakeNMForceCall(t *testing.T) {
	obj := restoreNM(t, nmConnectivityPortal, "http://check/x", "", nil)
	obj.forceCalled = false
	nmGateState.lastForce = time.Time{}
	state, err := nmGateState.connectivity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !obj.forceCalled {
		t.Error("fake harness: CheckConnectivity should hit the fake")
	}
	if state != nmConnectivityPortal {
		t.Errorf("state = %d, want portal", state)
	}
}

// ---- smart-auto state machine ----

func smartTestStatus() *Status {
	return &Status{
		BackendState: "Running",
		Peer: map[string]Peer{
			"own": {HostName: "zds-nabara", DNSName: "zds-nabara.tailb4e47d.ts.net.", ExitNodeOption: true, Online: true, PrimaryRoutes: []string{"192.168.178.0/24"}},
			"mv":  {HostName: "al-tia-wg-001", DNSName: "al-tia-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"}},
		},
	}
}

func TestTickSmartAutoPrimeOnly(t *testing.T) {
	ft := installFakeTailscale(t)
	restoreIfaceAddrs(t, nil) // away
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{}

	a.tickSmartAutoStartup(smartTestStatus(), true) // startup prime
	if a.smart.primed != true || a.smart.atHome != false {
		t.Fatalf("prime state wrong: %+v", a.smart)
	}
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "set" {
			t.Fatalf("startup priming must not apply anything, ran %v", c)
		}
	}
}

func TestTickSmartAutoRePrimeApplies(t *testing.T) {
	ft := installFakeTailscale(t)
	restoreIfaceAddrs(t, nil) // away
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{} // simulates un-primed state after disconnect/profile switch

	a.tickSmartAuto(smartTestStatus()) // re-prime applies immediately
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	var setCall []string
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "set" {
			setCall = c
		}
	}
	if setCall == nil || setCall[1] != "--exit-node=zds-nabara" {
		t.Errorf("re-prime should apply the policy target, ran %v", ft.callsSoFar(t))
	}
}

func TestTickSmartAutoFlipsAwayToHome(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{}

	// prime: away
	restoreIfaceAddrs(t, nil)
	a.tickSmartAuto(smartTestStatus())

	// flip: home (local IP inside advertised LAN)
	restoreIfaceAddrs(t, []string{"192.168.178.35/24"})
	a.tickSmartAuto(smartTestStatus())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	var setCall []string
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "set" {
			setCall = c
		}
	}
	if setCall == nil {
		t.Fatalf("flip should apply a node, calls: %v", ft.callsSoFar(t))
	}
	// home -> Mullvad: last-used empty, no "selected" row online in fixture
	// (fixture's selected row is Germany, not in st peers) -> first online peer: al-tia-wg-001
	if setCall[1] != "--exit-node=al-tia-wg-001" {
		t.Errorf("home flip applied %v, want last/first Mullvad", setCall)
	}
}

func TestTickSmartAutoFlipsHomeToAway(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{}

	// prime: home
	restoreIfaceAddrs(t, []string{"192.168.178.35/24"})
	a.tickSmartAuto(smartTestStatus())

	// flip: away
	restoreIfaceAddrs(t, nil)
	a.tickSmartAuto(smartTestStatus())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	var setCall []string
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "set" {
			setCall = c
		}
	}
	if setCall == nil {
		t.Fatalf("flip should apply own node, calls: %v", ft.callsSoFar(t))
	}
	if setCall[1] != "--exit-node=zds-nabara" {
		t.Errorf("away flip applied %v, want own node", setCall)
	}
}

func TestTickSmartAutoStableDoesNotThrash(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{}

	restoreIfaceAddrs(t, nil) // away, stays away
	a.tickSmartAuto(smartTestStatus())
	nAfterPrime := len(ft.callsSoFar(t))
	for i := 0; i < 5; i++ {
		a.tickSmartAuto(smartTestStatus())
	}
	if got := len(ft.callsSoFar(t)); got != nAfterPrime {
		t.Errorf("stable state applied %d extra calls", got-nAfterPrime)
	}
}

func TestTickSmartAutoPersistsLastMullvad(t *testing.T) {
	// Point the state file into a temp dir.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{}

	// prime: away (prime-only), then flip home -> applies first online
	// Mullvad (al-tia-wg-001) and stores it
	restoreIfaceAddrs(t, nil)
	a.tickSmartAuto(smartTestStatus())
	restoreIfaceAddrs(t, []string{"192.168.178.35/24"})
	a.tickSmartAuto(smartTestStatus())

	if got := LoadLastMullvad(); got != "al-tia-wg-001" {
		t.Errorf("stored last-mullvad = %q, want al-tia-wg-001", got)
	}

	// Simulate restart: fresh state machine; last-used wins over "first".
	a2 := newTestApp(t, &fakeUI{})
	a2.smart = &smartAuto{}
	if got := LoadLastMullvad(); got == "" {
		t.Fatal("state file missing after store")
	}
	// add a second online mullvad peer; last-used should still win
	st := smartTestStatus()
	st.Peer["mv2"] = Peer{HostName: "de-ber-wg-001", DNSName: "de-ber-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"}}
	// flip to away and back home to force a fresh pick
	restoreIfaceAddrs(t, nil)
	a2.tickSmartAuto(st)
	restoreIfaceAddrs(t, []string{"192.168.178.35/24"})
	a2.tickSmartAuto(st)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	var setCall []string
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "set" {
			setCall = c
		}
	}
	if setCall == nil || setCall[1] != "--exit-node=al-tia-wg-001" {
		t.Errorf("last-used should win over first peer, ran %v", setCall)
	}
}

func TestPickMullvadNode(t *testing.T) {
	st := &Status{Peer: map[string]Peer{
		"a": {HostName: "nl-ams-wg-001", DNSName: "nl-ams-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"}},
		"b": {HostName: "de-ber-wg-001", DNSName: "de-ber-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"}},
		"o": {HostName: "own", ExitNodeOption: true, Online: true},
	}}
	nodes := []ExitNodeInfo{
		{Hostname: "nl-ams-wg-001.mullvad.ts.net", Country: "Netherlands", City: "Amsterdam"},
		{Hostname: "de-ber-wg-001.mullvad.ts.net", Country: "Germany", City: "Berlin", Selected: true},
	}

	// last-used still online wins
	if got := PickMullvadNode(st, nodes, "de-ber-wg-001"); got != "de-ber-wg-001" {
		t.Errorf("last-used = %q", got)
	}
	// empty last-used -> selected row
	if got := PickMullvadNode(st, nodes, ""); got != "de-ber-wg-001" {
		t.Errorf("selected row = %q", got)
	}
	// last-used offline -> selected row
	if got := PickMullvadNode(st, nodes, "us-nyc-wg-999"); got != "de-ber-wg-001" {
		t.Errorf("offline last-used fallback = %q", got)
	}
	// nothing online -> ""
	empty := &Status{Peer: map[string]Peer{}}
	if got := PickMullvadNode(empty, nodes, ""); got != "" {
		t.Errorf("no online nodes = %q", got)
	}
}

func TestOwnExitNodeLANsAndAtHome(t *testing.T) {
	st := &Status{Peer: map[string]Peer{
		"own":    {HostName: "own", ExitNodeOption: true, Online: true, PrimaryRoutes: []string{"192.168.178.0/24"}},
		"mull":   {HostName: "de-ber-wg-001", Tags: []string{"tag:mullvad-exit-node"}, ExitNodeOption: true, Online: true, PrimaryRoutes: []string{"10.0.0.0/24"}},
		"offown": {HostName: "own2", ExitNodeOption: true, Online: false, PrimaryRoutes: []string{"172.16.0.0/12"}},
	}}
	lans := OwnExitNodeLANs(st)
	if len(lans) != 1 || lans[0].String() != "192.168.178.0/24" {
		t.Fatalf("lans = %v, want only own online node's LAN", lans)
	}

	// at home when local addr inside
	restoreIfaceAddrs(t, []string{"192.168.178.42/24"})
	if !AtHome(lans) {
		t.Error("should be at home")
	}
	// away otherwise
	restoreIfaceAddrs(t, []string{"10.9.9.9/24"})
	if AtHome(lans) {
		t.Error("should be away")
	}
	// no prefixes -> never home
	if AtHome(nil) {
		t.Error("no prefixes should not be home")
	}
}

func TestDisableSmartAutoOnManualPick(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true}
	st := &Status{
		BackendState: "Running",
		Peer: map[string]Peer{
			"mv-a": mvPeer("al-tia-wg-001", false),
		},
	}
	items := a.exitNodeMenu(st, nil)

	city := findLabel(mullvadSub(items), "○ Tirana")
	if city == nil {
		t.Fatalf("Tirana missing: %v", labels(mullvadSub(items)))
	}
	city.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if a.smart != nil {
		t.Error("manual pick should disable smart auto")
	}
	if got := ft.lastCall(t); got[0] != "set" || got[1] != "--exit-node=al-tia-wg-001.mullvad.ts.net" {
		t.Errorf("manual pick ran %v", got)
	}
}

func TestDisableSmartAutoOnOffClick(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true}
	st := &Status{BackendState: "Running", Peer: map[string]Peer{}}
	items := a.exitNodeMenu(st, nil)

	off := findLabel(items, "● Off")
	off.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if a.smart != nil {
		t.Error("Off click should disable smart auto")
	}
}

// mullvadSub finds the Mullvad submenu in the exit-node items.
func mullvadSub(items []MenuItem) []MenuItem {
	if m := findLabel(items, "Mullvad"); m != nil {
		return m.Submenu
	}
	return nil
}

// ---- offline profiles + smart-auto across disconnect/profile switch ----

func TestMenuOfflineShowsProfiles(t *testing.T) {
	installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Stopped"}
	profiles := []Profile{
		{ID: "p1", Nickname: "one@x", Selected: true},
		{ID: "p2", Nickname: "two@y"},
	}
	items := a.menuOffline(st, profiles)

	sub := findLabel(items, "Profile: one@x")
	if sub == nil {
		t.Fatalf("offline menu should show profiles: %v", labels(items))
	}
	if !hasLabel(items, "● one@x") || !hasLabel(items, "○ two@y") {
		t.Errorf("profile radio items missing: %v", labels(items))
	}
	if !hasLabel(items, "Connect") || !hasLabel(items, "Quit") {
		t.Errorf("Connect/Quit missing: %v", labels(items))
	}
}

func TestMenuOfflineSwitchConnects(t *testing.T) {
	ft := installFakeTailscale(t)
	// status fetched by onProfileSwitched; report Stopped so it connects
	ft.setResponse(t, `{"BackendState":"Stopped"}`)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true}
	st := &Status{BackendState: "Stopped"}
	profiles := []Profile{
		{ID: "p1", Nickname: "one@x", Selected: true},
		{ID: "p2", Nickname: "two@y"},
	}
	items := a.menuOffline(st, profiles)

	other := findLabel(items, "○ two@y")
	other.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	calls := ft.callsSoFar(t)
	// expect: switch p2 -> status --json -> up
	var swIdx, upIdx = -1, -1
	for i, c := range calls {
		if c[0] == "switch" && len(c) > 1 && c[1] == "p2" {
			swIdx = i
		}
		if c[0] == "up" {
			upIdx = i
		}
	}
	if swIdx == -1 {
		t.Fatalf("switch call missing: %v", calls)
	}
	if upIdx == -1 {
		t.Fatalf("offline switch should auto-connect (up missing): %v", calls)
	}
	if upIdx < swIdx {
		t.Errorf("`up` ran before `switch`: %v", calls)
	}
	if a.smart != nil && a.smartPrimed() {
		t.Error("profile switch should un-prime smart auto")
	}
}

func TestMenuNeedsLoginShowsProfiles(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "NeedsLogin"}
	profiles := []Profile{
		{ID: "p1", Nickname: "one@x", Selected: true},
		{ID: "p2", Nickname: "two@y"},
	}
	items := a.menuNeedsLogin(st, profiles)
	if !hasLabel(items, "Profile: one@x") {
		t.Errorf("needs-login menu should show profiles: %v", labels(items))
	}
}

func TestTickSmartAutoReAppliesAfterDisconnect(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, `{"BackendState":"Running"}`) // for enable/re-apply GetStatus
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{}

	// prime: away
	restoreIfaceAddrs(t, nil)
	a.tickSmartAuto(smartTestStatus())

	// disconnect: un-primes, no CLI calls
	nBefore := len(ft.callsSoFar(t))
	stopped := &Status{BackendState: "Stopped", Peer: smartTestStatus().Peer}
	a.tickSmartAuto(stopped)
	if a.smartPrimed() {
		t.Fatal("stopped state should un-prime smart auto")
	}
	if got := len(ft.callsSoFar(t)); got != nBefore {
		t.Errorf("stopped tick issued CLI calls: %d -> %d", nBefore, got)
	}

	// reconnect (still away): re-primes AND re-applies own node
	a.tickSmartAuto(smartTestStatus())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) < nBefore+2 {
		time.Sleep(5 * time.Millisecond)
	}
	calls := ft.callsSoFar(t)
	var setCall []string
	for _, c := range calls[nBefore:] {
		if c[0] == "set" {
			setCall = c
		}
	}
	if setCall == nil || setCall[1] != "--exit-node=zds-nabara" {
		t.Errorf("reconnect should re-apply own node (away), calls after stop: %v", calls[nBefore:])
	}
}

func TestProfileSwitchResetsSmartAuto(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true, atHome: true}

	a.onProfileSwitched()
	if a.smartPrimed() {
		t.Error("profile switch should un-prime smart auto")
	}
}

func TestOnProfileSwitchedRunningDoesNotConnect(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, `{"BackendState":"Running"}`)
	a := newTestApp(t, &fakeUI{})

	a.onProfileSwitched()
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "up" {
			t.Errorf("online profile switch must not run `up`: %v", ft.callsSoFar(t))
		}
	}
}

// ---- menuOffline / menuNeedsLogin ----

func TestMenuOfflineConnectAndQuit(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Stopped"}
	items := a.menuOffline(st, nil)

	connect := findLabel(items, "Connect")
	if connect == nil {
		t.Fatal("Connect missing")
	}
	connect.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "up" {
		t.Errorf("Connect ran %v", got)
	}
	select {
	case <-a.refreshCh:
	case <-time.After(time.Second):
		t.Error("Connect should request refresh")
	}

	quit := findLabel(items, "Quit")
	quit.OnClick()
	select {
	case <-a.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Quit did not cancel context")
	}
}

func TestMenuOfflineLogIn(t *testing.T) {
	rec := restoreExec(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Stopped", AuthURL: "https://login.tailscale.com/a/x"}
	items := a.menuOffline(st, nil)
	login := findLabel(items, "Log in")
	if login == nil {
		t.Fatal("Log in missing")
	}
	login.OnClick()
	if got := rec.all(); len(got) != 1 || got[0][1] != "https://login.tailscale.com/a/x" {
		t.Errorf("login click = %v", got)
	}
}

func TestMenuNeedsLoginQuit(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "NeedsLogin"}
	items := a.menuNeedsLogin(st, nil)
	quit := findLabel(items, "Quit")
	quit.OnClick()
	select {
	case <-a.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Quit did not cancel context")
	}
}

func TestMenuNeedsLoginOpenLogin(t *testing.T) {
	rec := restoreExec(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "NeedsLogin", AuthURL: "https://login.tailscale.com/a/x"}
	items := a.menuNeedsLogin(st, nil)
	open := findLabel(items, "Open login page")
	open.OnClick()
	if len(rec.all()) != 1 {
		t.Errorf("open click recorded %d calls", len(rec.all()))
	}
}

// ---- helpers ----

func TestCopyToClipboardWlCopy(t *testing.T) {
	rec := restoreExec(t)
	copyToClipboard("hello")
	if got := rec.all(); len(got) != 1 || got[0][0] != "wl-copy" {
		t.Errorf("calls = %v", got)
	}
}

func TestCopyToClipboardFallsBackToXclip(t *testing.T) {
	rec := restoreExecFailing(t)
	copyToClipboard("hello")
	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("calls = %v, want 2 (wl-copy then xclip)", got)
	}
	if got[0][0] != "wl-copy" || got[1][0] != "xclip" {
		t.Errorf("calls = %v", got)
	}
	if got[1][1] != "-selection" || got[1][1+1] != "clipboard" {
		t.Errorf("xclip args = %v", got[1])
	}
}

func TestOpenURL(t *testing.T) {
	rec := restoreExec(t)
	openURL("https://example.com")
	if got := rec.all(); len(got) != 1 || got[0][0] != "xdg-open" || got[0][1] != "https://example.com" {
		t.Errorf("calls = %v", got)
	}
}

func TestDedupKey(t *testing.T) {
	st := &Status{
		BackendState: "Running",
		TailscaleIPs: []string{"100.64.0.1"},
		Peer: map[string]Peer{
			"e": {HostName: "de-fra-wg-001", ExitNodeOption: true, Online: true},
		},
	}
	profiles := []Profile{{ID: "p1", Selected: true}}
	items := []MenuItem{}

	k1 := dedupKey(st, profiles, items)

	// same inputs -> same key
	if dedupKey(st, profiles, items) != k1 {
		t.Error("dedup key should be stable")
	}

	// peer goes offline -> key changes
	st.Peer["e"] = Peer{HostName: "de-fra-wg-001", ExitNodeOption: true, Online: false}
	if dedupKey(st, profiles, items) == k1 {
		t.Error("peer online change should change key")
	}
	st.Peer["e"] = Peer{HostName: "de-fra-wg-001", ExitNodeOption: true, Online: true}

	// profiles change
	if dedupKey(st, []Profile{{ID: "p2", Selected: true}}, items) == k1 {
		t.Error("profile change should change key")
	}

	// backend state change
	st2 := *st
	st2.BackendState = "Stopped"
	if dedupKey(&st2, profiles, items) == k1 {
		t.Error("state change should change key")
	}

	// auth url change
	st3 := *st
	st3.AuthURL = "https://x"
	if dedupKey(&st3, profiles, items) == k1 {
		t.Error("authurl change should change key")
	}

	// exit node selection change
	st.Peer["e"] = Peer{HostName: "de-fra-wg-001", ExitNodeOption: true, Online: true, ExitNode: true}
	if dedupKey(st, profiles, items) == k1 {
		t.Error("exit node selection should change key")
	}
}

func TestRequestRefreshDropsWhenFull(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	a.requestRefresh()
	a.requestRefresh() // must not block or panic
	select {
	case <-a.refreshCh:
	default:
		t.Fatal("first request should be buffered")
	}
}

// ---- refresh / registerLoop / runApp ----

func TestRefreshSendsUpdate(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, `{"BackendState":"Running","TailscaleIPs":["100.64.0.1"],"Self":{"HostName":"h"}}`)
	ui := &fakeUI{}
	a := newTestApp(t, ui)
	a.refresh()
	updates, _, _, items := ui.snapshot()
	if updates != 1 {
		t.Fatalf("updates = %d, want 1", updates)
	}
	if len(items) == 0 {
		t.Error("no menu items built")
	}
	if !hasLabel(items, "Copy IP: 100.64.0.1") {
		t.Errorf("menu = %v", labels(items))
	}
}

func TestRefreshDedups(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, `{"BackendState":"Running","TailscaleIPs":["100.64.0.1"],"Self":{"HostName":"h"}}`)
	ui := &fakeUI{}
	a := newTestApp(t, ui)
	a.refresh()
	a.refresh()
	if updates, _, _, _ := ui.snapshot(); updates != 1 {
		t.Errorf("updates = %d, want 1 after dedup", updates)
	}
}

func TestRefreshStateChangeTriggersUpdate(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, `{"BackendState":"Running","TailscaleIPs":["100.64.0.1"],"Self":{"HostName":"h"}}`)
	ui := &fakeUI{}
	a := newTestApp(t, ui)
	a.refresh()
	ft.setResponse(t, `{"BackendState":"Stopped","TailscaleIPs":["100.64.0.1"],"Self":{"HostName":"h"}}`)
	a.refresh()
	if updates, _, tooltip, _ := ui.snapshot(); updates != 2 || tooltip != "Tailscale: Stopped" {
		t.Errorf("updates=%d tooltip=%q, want 2 / Stopped", updates, tooltip)
	}
}

func TestRefreshErrorLeavesStateUntouched(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setFail(t)
	ui := &fakeUI{}
	a := newTestApp(t, ui)
	a.refresh()
	if updates, _, _, _ := ui.snapshot(); updates != 0 {
		t.Errorf("updates = %d, want 0 on error", updates)
	}
}

func TestRefreshProfilesFailureIgnored(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, `{"BackendState":"Running","TailscaleIPs":["100.64.0.1"],"Self":{"HostName":"h"}}`)
	ui := &fakeUI{}
	a := newTestApp(t, ui)
	a.refresh()
	if updates, _, _, _ := ui.snapshot(); updates != 1 {
		t.Errorf("updates = %d", updates)
	}
}

func TestRegisterLoopImmediate(t *testing.T) {
	ui := &fakeUI{reg: true}
	a := newTestApp(t, ui)
	done := make(chan struct{})
	go func() {
		a.registerLoop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registerLoop blocked with successful registration")
	}
}

func TestRegisterLoopRetriesThenSucceeds(t *testing.T) {
	ui := &fakeUI{reg: false}
	a := newTestApp(t, ui)
	oldDelay := registerRetryDelay
	registerRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { registerRetryDelay = oldDelay })

	// Flip `reg` on from a helper goroutine after a short delay.
	go func() {
		time.Sleep(20 * time.Millisecond)
		ui.setReg(true)
	}()
	done := make(chan struct{})
	go func() {
		a.registerLoop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("registerLoop never returned after registration succeeded")
	}
}

func TestRegisterLoopStopsOnCancel(t *testing.T) {
	ui := &fakeUI{reg: false}
	a := newTestApp(t, ui)
	oldDelay := registerRetryDelay
	registerRetryDelay = 50 * time.Millisecond
	t.Cleanup(func() { registerRetryDelay = oldDelay })

	go a.stop()
	done := make(chan struct{})
	go func() {
		a.registerLoop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("registerLoop ignored ctx cancellation")
	}
}

func TestRunAppRegistersClosesAndExitsOnCancel(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, `{"BackendState":"Running","TailscaleIPs":["100.64.0.1"],"Self":{"HostName":"h"}}`)
	oldPoll := pollInterval
	pollInterval = 10 * time.Millisecond
	t.Cleanup(func() { pollInterval = oldPoll })

	ui := &fakeUI{reg: true}
	ctx, stop := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- runApp(ctx, stop, ui)
	}()
	// let it run a couple of poll ticks, then stop
	time.Sleep(30 * time.Millisecond)
	stop()

	select {
	case err := <-errCh:
		if err == nil {
			t.Error("runApp should return ctx.Err on cancel")
		} else if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runApp did not return after cancel")
	}
	if !ui.closed {
		t.Error("runApp should close the UI connection")
	}
	if updates, _, _, _ := ui.snapshot(); updates == 0 {
		t.Error("runApp never pushed an initial refresh")
	}
}

func TestRunAppPollsContinuously(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setAlternatingResponses(t)

	oldPoll := pollInterval
	pollInterval = 5 * time.Millisecond
	t.Cleanup(func() { pollInterval = oldPoll })

	ui := &fakeUI{reg: true}
	ctx, stop := context.WithCancel(context.Background())
	go runApp(ctx, stop, ui)
	time.Sleep(80 * time.Millisecond)
	stop()
	updates, _, _, _ := ui.snapshot()
	if updates < 2 {
		t.Errorf("updates = %d after 16 poll intervals, want >= 2", updates)
	}
}

// ---- concurrency safety of app state ----

func TestRefreshConcurrentSafe(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, `{"BackendState":"Running","TailscaleIPs":["100.64.0.1"],"Self":{"HostName":"h"}}`)
	ui := &fakeUI{}
	a := newTestApp(t, ui)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				a.requestRefresh()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 5; j++ {
			a.refresh()
		}
	}()
	wg.Wait()
}

// ---- misc ----

func TestNewIDSequence(t *testing.T) {
	start := nextID.Load()
	a := newID()
	b := newID()
	if b != a+1 {
		t.Errorf("ids not sequential: %d then %d", a, b)
	}
	if a <= start {
		t.Errorf("id did not advance: start %d", start)
	}
}

func TestMain(_ *testing.T) {} // placeholder to satisfy editors listing main
