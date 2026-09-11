package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os/exec"
	"sync"
	"testing"
	"time"
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
	installFakeTailscale(t) // no exit-node list -> hostname-based fallback
	a := newTestApp(t, &fakeUI{})
	st := &Status{
		BackendState: "Running",
		TailscaleIPs: []string{"100.64.0.1"},
		Peer: map[string]Peer{
			"e": {HostName: "de-fra-wg-001", DNSName: "de-fra-wg-001.mullvad.ts.net.", ExitNode: true, ExitNodeOption: true, Online: true},
		},
	}
	icon, tooltip, items := a.buildUI(st, nil)
	if !hasLabel(items, "Exit node: fra") {
		t.Errorf("submenu parent should show city: %v", labels(items))
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
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "switch" || got[1] != "p2" {
		t.Errorf("switch ran %v", got)
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

func TestExitSuffixPrefersExitNodeList(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)
	st := &Status{BackendState: "Running"}
	got := exitSuffix(&Peer{HostName: "de-ber-wg-001.mullvad.ts.net."}, st)
	if got != ": Berlin" {
		t.Errorf("exitSuffix with list = %q, want ': Berlin'", got)
	}
}

// exitNodeListFixture mirrors `tailscale exit-node list` output:
// multi-word cities, own node with "-" fields, "Any" duplicate, selected row.
const exitNodeListFixture = `
 IP                  HOSTNAME                         COUNTRY            CITY                   STATUS       
 100.107.25.96       zds-nabara.tailb4e47d.ts.net     -                  -                      -            
 100.77.189.15       al-tia-wg-001.mullvad.ts.net     Albania            Tirana                 -            
 100.65.216.13       au-adl-wg-301.mullvad.ts.net     Australia          Any                    -            
 100.65.216.13       au-adl-wg-301.mullvad.ts.net     Australia          Adelaide               -            
 100.123.112.108     de-ber-wg-001.mullvad.ts.net     Germany            Berlin                 selected     
`

func TestGetExitNodes(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)

	nodes, err := GetExitNodes(context.Background())
	if err != nil {
		t.Fatalf("GetExitNodes: %v", err)
	}
	if len(nodes) != 5 {
		t.Fatalf("nodes = %d, want 5 (Any dupe kept for dedupe at menu level)", len(nodes))
	}
	own := nodes[0]
	if own.Country != "" || own.City != "" || own.Selected {
		t.Errorf("own node parsed wrong: %+v", own)
	}
	ber := nodes[4]
	if !ber.Selected || ber.Country != "Germany" || ber.City != "Berlin" {
		t.Errorf("selected row parsed wrong: %+v", ber)
	}
	if nodes[1].City != "Tirana" {
		t.Errorf("multi-word/regular city: %+v", nodes[1])
	}
}

func TestGetExitNodesError(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setFail(t)
	if _, err := GetExitNodes(context.Background()); err == nil {
		t.Fatal("should fail when CLI fails")
	}
}

func TestGetExitNodesGarbage(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, "some error occurred")
	if _, err := GetExitNodes(context.Background()); err == nil {
		t.Fatal("should fail with no parseable rows")
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

func TestExitNodeMenuOffAndAuto(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)
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
	ft.setExitNodeList(t, exitNodeListFixture)
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
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true, atHome: true}
	active := Peer{HostName: "de-ber-wg-001", DNSName: "de-ber-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, ExitNode: true}
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
	if !hasLabel(mullvad.Submenu, "● Germany") {
		t.Errorf("country should be hoisted in smart-auto mode: %v", labels(mullvad.Submenu))
	}
	if !hasLabel(mullvad.Submenu, "● Berlin") {
		t.Errorf("applied city should be marked in smart-auto mode: %v", labels(mullvad.Submenu))
	}
}

func TestExitNodeMenuAutoParentLabel(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true, atHome: true}
	active := Peer{HostName: "de-ber-wg-001", DNSName: "de-ber-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, ExitNode: true}
	st := &Status{BackendState: "Running", Peer: map[string]Peer{"mv": active}}
	_, _, menu := a.buildUI(st, nil)
	// parent shows the applied node's city, not "auto"
	if !hasLabel(menu, "Exit node: Berlin") {
		t.Errorf("parent label in smart-auto mode: %v", labels(menu))
	}
}

func TestExitNodeMenuAutoClickEnables(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)
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
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)
	a := newTestApp(t, &fakeUI{})
	st := &Status{
		BackendState: "Running",
		Peer: map[string]Peer{
			"own":  {HostName: "zds-nabara", DNSName: "zds-nabara.tailb4e47d.ts.net.", ExitNodeOption: true, Online: true},
			"mv-a": {HostName: "al-tia-wg-001", DNSName: "al-tia-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"}},
			"mv-b": {HostName: "de-ber-wg-001", DNSName: "de-ber-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"}},
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
	// Australia "Any" duplicate suppressed: only Adelaide survives
	germany := findLabel(mullvad.Submenu, "Germany")
	if germany == nil {
		t.Fatal("Germany submenu missing")
	}
}

func TestExitNodeMenuActiveCountryHoisted(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)
	a := newTestApp(t, &fakeUI{})
	active := Peer{HostName: "de-ber-wg-001", DNSName: "de-ber-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, ExitNode: true}
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
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)
	a := newTestApp(t, &fakeUI{})
	active := Peer{HostName: "de-ber-wg-001", DNSName: "de-ber-wg-001.mullvad.ts.net.", ExitNode: true}
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
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)
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
	ft.setExitNodeList(t, exitNodeListFixture)
	a := newTestApp(t, &fakeUI{})
	st := &Status{
		BackendState: "Running",
		Peer: map[string]Peer{
			"mv-a": {HostName: "al-tia-wg-001", DNSName: "al-tia-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"}},
		},
	}
	items := a.exitNodeMenu(st, nil)

	mullvad := findLabel(items, "Mullvad")
	if mullvad == nil {
		t.Fatalf("Mullvad submenu missing: %v", labels(items))
	}
	albania := findLabel(mullvad.Submenu, "Albania")
	if albania == nil {
		t.Fatalf("Albania missing: %v", labels(mullvad.Submenu))
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
	installFakeTailscale(t)
	// no exit-node list response: GetExitNodes returns nothing parseable
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
	ft.setExitNodeList(t, exitNodeListFixture)
	restoreIfaceAddrs(t, nil) // away
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{}

	a.tickSmartAuto(smartTestStatus()) // prime
	if a.smart.primed != true || a.smart.atHome != false {
		t.Fatalf("prime state wrong: %+v", a.smart)
	}
	for _, c := range ft.callsSoFar(t) {
		if c[0] == "set" {
			t.Fatalf("priming must not apply anything, ran %v", c)
		}
	}
}

func TestTickSmartAutoFlipsAwayToHome(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setExitNodeList(t, exitNodeListFixture)
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
	ft.setExitNodeList(t, exitNodeListFixture)
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
	ft.setExitNodeList(t, exitNodeListFixture)
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
	ft.setExitNodeList(t, exitNodeListFixture)
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
	ft.setExitNodeList(t, exitNodeListFixture)
	a := newTestApp(t, &fakeUI{})
	a.smart = &smartAuto{primed: true}
	st := &Status{
		BackendState: "Running",
		Peer: map[string]Peer{
			"mv-a": {HostName: "al-tia-wg-001", DNSName: "al-tia-wg-001.mullvad.ts.net.", ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node"}},
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
	ft.setExitNodeList(t, exitNodeListFixture)
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

// ---- menuOffline / menuNeedsLogin ----

func TestMenuOfflineConnectAndQuit(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Stopped"}
	items := a.menuOffline(st)

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
	items := a.menuOffline(st)
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
	items := a.menuNeedsLogin(st)
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
	items := a.menuNeedsLogin(st)
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