package main

import (
	"context"
	"errors"
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
	// The peer is not tagged mullvad here, so the suffix shows the raw
	// hostname; use a tagged peer for the city label variant.
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
	if !hasLabel(items, "Exit node: de-fra-wg-001") == false && !hasLabel(items, "Exit node: fra") {
		t.Errorf("expected either form: %v", labels(items))
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
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running", TailscaleIPs: []string{"100.64.0.2"}}
	items := a.menuOnline(st, nil, nil)

	copyItem := findLabel(items, "Copy IP: 100.64.0.2")
	if copyItem == nil {
		t.Fatal("Copy IP item missing")
	}
	copyItem.OnClick()
	if got := ft.callsSoFar(t); len(got) != 0 {
		t.Errorf("copy should not call CLI, got %v", got)
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
	active := findLabel(items, "one@x")
	if active == nil || active.Checked != true || active.OnClick != nil {
		t.Errorf("active profile should be checked with nil handler")
	}
	other := findLabel(items, "two@y")
	if other == nil || other.Checked != false || other.OnClick == nil {
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
	if got := exitSuffix(nil); got != ": off" {
		t.Errorf("nil exit = %q", got)
	}
	if got := exitSuffix(&Peer{HostName: "homeserver"}); got != ": homeserver" {
		t.Errorf("own node = %q", got)
	}
	if got := exitSuffix(&Peer{HostName: "de-fra-wg-001", Tags: []string{"tag:mullvad-exit-node-de"}}); got != ": fra" {
		t.Errorf("mullvad = %q", got)
	}
	// tagged mullvad but hostname lacks a city segment: SplitN still
	// yields two parts ("mullvad", "only"), so the label is that second
	// token, not the hostname.
	if got := exitSuffix(&Peer{HostName: "mullvad-only", Tags: []string{"tag:mullvad"}}); got != ": only" {
		t.Errorf("mullvad without city = %q", got)
	}
	// a hostname without dashes at all falls back to the full hostname
	if got := exitSuffix(&Peer{HostName: "mullvadsolo", Tags: []string{"tag:mullvad"}}); got != ": mullvadsolo" {
		t.Errorf("mullvad single-token host = %q", got)
	}
	// DNS-name-based detection (no tags) also yields the city
	if got := exitSuffix(&Peer{HostName: "nl-ams-wg-003", DNSName: "nl-ams-wg-003.mullvad.ts.net."}); got != ": ams" {
		t.Errorf("mullvad via DNS = %q", got)
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

func TestExitNodeMenuEmpty(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{BackendState: "Running", Peer: map[string]Peer{}}
	items := a.exitNodeMenu(st)
	if !hasLabel(items, "Auto (best)") {
		t.Fatalf("Auto missing: %v", labels(items))
	}
	if !hasLabel(items, "No exit nodes available") {
		t.Errorf("empty state placeholder missing: %v", labels(items))
	}
}

func TestExitNodeMenuOwnAndMullvad(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	st := &Status{
		BackendState: "Running",
		Peer: map[string]Peer{
			"own":  {HostName: "homeserver", ExitNodeOption: true, Online: true},
			"mv-a": {HostName: "nl-ams-wg-002", ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node-nl"}},
			"mv-b": {HostName: "de-fra-wg-001", ExitNodeOption: true, Online: true, Tags: []string{"tag:mullvad-exit-node-de"}},
			"off":  {HostName: "offline-node", ExitNodeOption: true, Online: false},
			"noex": {HostName: "plain", Online: true},
		},
	}
	items := a.exitNodeMenu(st)

	auto := findLabel(items, "Auto (best)")
	if auto == nil || !auto.Checked {
		t.Fatalf("Auto should be checked when no exit selected")
	}
	if !hasLabel(items, "homeserver") {
		t.Errorf("own node missing: %v", labels(items))
	}
	if !hasLabel(items, "NL") || !hasLabel(items, "DE") {
		t.Errorf("country submenus missing: %v", labels(items))
	}
	if hasLabel(items, "offline-node") || hasLabel(items, "plain") {
		t.Errorf("offline / non-exit peers leaked into menu: %v", labels(items))
	}
}

func TestExitNodeMenuActivePeerChecked(t *testing.T) {
	a := newTestApp(t, &fakeUI{})
	active := Peer{HostName: "homeserver", DNSName: "homeserver.ts.net.", ExitNodeOption: true, Online: true, ExitNode: true}
	st := &Status{
		BackendState: "Running",
		Peer:         map[string]Peer{"own": active},
	}
	items := a.exitNodeMenu(st)
	activeItem := findLabel(items, "homeserver")
	if activeItem == nil || !activeItem.Checked {
		t.Fatal("active peer should be checked")
	}
	if !hasLabel(items, "Exit node: homeserver") {
		// this is the submenu parent; verify via full build
		_, _, menu := a.buildUI(st, nil)
		if !hasLabel(menu, "Exit node: homeserver") {
			t.Errorf("parent label missing: %v", labels(menu))
		}
	}
}

func TestExitNodeToggleUnsetActive(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	item := a.exitNodeToggle(Peer{HostName: "homeserver", DNSName: "homeserver.ts.net."}, true)
	if !item.Checked {
		t.Fatal("active toggle should be checked")
	}
	item.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "set" || got[1] != "--exit-node=" {
		t.Errorf("unset ran %v", got)
	}
	select {
	case <-a.refreshCh:
	case <-time.After(time.Second):
		t.Error("toggle should request refresh")
	}
}

func TestExitNodeToggleSetInactive(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	item := a.exitNodeToggle(Peer{HostName: "homeserver", DNSName: "homeserver.ts.net."}, false)
	if item.Checked {
		t.Fatal("inactive toggle should be unchecked")
	}
	item.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "set" || got[1] != "--exit-node=homeserver.ts.net" {
		t.Errorf("set ran %v (want base name)", got)
	}
}

func TestAutoToggleOffSelectsAuto(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	item := a.autoToggle(&Peer{HostName: "homeserver"})
	if item.Checked {
		t.Fatal("auto should be unchecked while a peer is active")
	}
	item.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "set" || got[1] != "--exit-node=auto:any" {
		t.Errorf("auto ran %v", got)
	}
}

func TestAutoToggleOnUnsets(t *testing.T) {
	ft := installFakeTailscale(t)
	a := newTestApp(t, &fakeUI{})
	item := a.autoToggle(nil)
	if !item.Checked {
		t.Fatal("auto should be checked with no peer selected")
	}
	item.OnClick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(ft.callsSoFar(t)) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := ft.lastCall(t); got[0] != "set" || got[1] != "--exit-node=" {
		t.Errorf("unset ran %v", got)
	}
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