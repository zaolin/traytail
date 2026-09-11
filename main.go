package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var pollInterval = 5 * time.Second

var registerRetryDelay = 3 * time.Second

// execCommand is a seam for tests; production code always uses exec.Command.
var execCommand = exec.Command

// trayUI is the subset of SNI the app drives; tests replace it with a fake.
type trayUI interface {
	Update(icon []byte, tooltip string, items []MenuItem)
	RegisterWithWatcher() bool
	Close()
}

type app struct {
	ctx       context.Context
	stop      context.CancelFunc // cancels ctx on Quit
	sni       trayUI
	last      string // dedup key
	refreshCh chan struct{}

	// smart-auto state
	smart       *smartAuto
	execCommand func(string, ...string) *exec.Cmd // nil = global execCommand
}

// smartAuto tracks the home/away state machine. It applies the policy
// exit node only when the home/away state flips, leaving manual picks
// alone while the state is stable.
type smartAuto struct {
	mu      sync.Mutex
	atHome  bool // last observed home state
	primed  bool // have we observed any state yet?
	lastMul string
}

func main() {
	log.SetFlags(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	sni, err := NewSNI(os.Getpid())
	if err != nil {
		log.Fatalf("traytail: %v", err)
	}
	if err := runApp(ctx, stop, sni); err != nil {
		log.Fatalf("traytail: %v", err)
	}
}

// runApp runs the poll loop until ctx is cancelled (signal or Quit item).
// It owns the SNI connection for the duration.
func runApp(ctx context.Context, stop context.CancelFunc, sni trayUI) error {
	defer sni.Close()
	a := &app{ctx: ctx, stop: stop, sni: sni, refreshCh: make(chan struct{}, 1), smart: &smartAuto{}}
	a.registerLoop()
	a.refresh() // initial state

	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			a.refresh()
		case <-a.refreshCh:
			a.refresh()
		}
	}
}

// registerLoop registers with whatever watcher is running (ashell provides
// one), retrying until it shows up or the app context is cancelled.
func (a *app) registerLoop() {
	for {
		if a.sni.RegisterWithWatcher() {
			log.Println("traytail: registered with StatusNotifierWatcher")
			return
		}
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(registerRetryDelay):
		}
	}
}

func (a *app) refresh() {
	st, err := GetStatus(a.ctx)
	if err != nil {
		log.Printf("traytail: %v", err)
		return
	}
	profiles, _ := GetProfiles(a.ctx)

	a.tickSmartAuto(st)

	icon, tooltip, items := a.buildUI(st, profiles)
	key := dedupKey(st, profiles, items)
	if key == a.last {
		return
	}
	a.last = key
	a.sni.Update(icon, tooltip, items)
}

// tickSmartAuto runs the home/away state machine. desiredTarget
// returns the exit node hostname to apply when the state flips.
func (a *app) tickSmartAuto(st *Status) {
	s := a.smart
	if s == nil || !st.Running() {
		return
	}
	lans := OwnExitNodeLANs(st)
	home := AtHome(lans)

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.primed {
		s.primed = true
		s.atHome = home
		s.lastMul = LoadLastMullvad()
		log.Printf("traytail: smart auto primed (home=%v, lastMullvad=%q)", home, s.lastMul)
		return // first observation never applies anything
	}
	if home == s.atHome {
		return // stable: manual choices stand
	}
	s.atHome = home

	// Flipped. Determine the desired target.
	var target string
	if home {
		nodes, _ := GetExitNodes(context.Background())
		target = PickMullvadNode(st, nodes, s.lastMul)
		if target != "" {
			s.lastMul = target
			StoreLastMullvad(target)
		}
	} else {
		target = a.pickOwnNode(st)
	}
	if target == "" {
		log.Printf("traytail: smart auto: no target for home=%v, keeping current", home)
		return
	}
	log.Printf("traytail: smart auto: home=%v -> applying exit node %q", home, target)
	if err := SetExitNode(context.Background(), target); err != nil {
		log.Printf("traytail: smart auto: set exit node: %v", err)
	}
}

// pickOwnNode returns the hostname of the first online own exit node
// advertising a LAN, or "" when none is available.
func (a *app) pickOwnNode(st *Status) string {
	for _, p := range st.SortPeers() {
		if p.ExitNodeOption && p.Online && !p.IsMullvad() && len(p.PrimaryRoutes) > 0 {
			return p.HostName
		}
	}
	return ""
}

// requestRefresh asks the main loop for an immediate refresh.
func (a *app) requestRefresh() {
	select {
	case a.refreshCh <- struct{}{}:
	default:
	}
}

// buildUI picks icon, tooltip and the full menu tree for the current state.
func (a *app) buildUI(st *Status, profiles []Profile) ([]byte, string, []MenuItem) {
	switch {
	case !st.Running() && (st.BackendState == "NeedsLogin" || st.AuthURL != ""):
		return iconWarning(), "Tailscale: login required", a.menuNeedsLogin(st)
	case !st.Running():
		return iconOffline(), "Tailscale: "+st.BackendState, a.menuOffline(st)
	}

	exit := st.ExitNodePeer()
	icon := iconConnected()
	exitLabel := "None"
	if exit != nil {
		icon = iconExitNode()
		exitLabel = exit.HostName
	}
	tooltip := fmt.Sprintf("Tailscale: connected\nIP: %s\nExit node: %s", st.SelfIP(), exitLabel)
	return icon, tooltip, a.menuOnline(st, exit, profiles)
}

// ---- menu construction ----

var nextID atomic.Int32

func newID() int32 {
	return nextID.Add(1)
}

func mk(label string, onClick func()) MenuItem {
	return MenuItem{ID: newID(), Label: label, Enabled: true, OnClick: onClick}
}

// radioGlyph prefixes a menu item with an active/inactive marker.
// ashell renders checkmark items as switches, so state is carried in
// the label instead ("● active" / "○ inactive").
func radioGlyph(active bool) string {
	if active {
		return "● "
	}
	return "○ "
}

func mkRadio(label string, active bool, onClick func()) MenuItem {
	return mk(radioGlyph(active)+label, onClick)
}

func mkSub(label string, children []MenuItem) MenuItem {
	return MenuItem{ID: newID(), Label: label, Enabled: true, Submenu: children}
}

func mkSep() MenuItem {
	return MenuItem{ID: newID(), Type: "separator"}
}

func (a *app) menuOnline(st *Status, exit *Peer, profiles []Profile) []MenuItem {
	tailnet := ""
	for _, p := range profiles {
		if p.Selected {
			tailnet = p.Tailnet
			break
		}
	}
	info := st.Self.HostName
	if tailnet != "" {
		info += " — " + tailnet
	}
	items := []MenuItem{
		// info row doubling as admin-console affordance
		mk(info, func() { openURL("https://login.tailscale.com/admin/machines") }),
		mk(fmt.Sprintf("Copy IP: %s", st.SelfIP()), func() {
			copyToClipboard(st.SelfIP())
		}),
		mkSep(),
	}

	// Profiles submenu: parent shows the active account; only one can
	// be active at a time, so clicking the selected one is a no-op.
	if len(profiles) > 1 {
		active := ""
		for _, p := range profiles {
			if p.Selected {
				active = p.Nickname
				break
			}
		}
		var profItems []MenuItem
		for _, p := range profiles {
			p := p
			if p.Selected {
				profItems = append(profItems, mkRadio(p.Nickname, true, nil))
			} else {
				profItems = append(profItems, mkRadio(p.Nickname, false, func() {
					if err := SwitchProfile(context.Background(), p.ID); err != nil {
						log.Printf("traytail: switch profile: %v", err)
					}
					a.requestRefresh()
				}))
			}
		}
		items = append(items, mkSub("Profile: "+active, profItems))
	}

	// Exit nodes submenu, split into own and Mullvad (per country).
	items = append(items, mkSub("Exit node"+exitSuffix(exit, st), a.exitNodeMenu(st, exit)))

	// Connect / Disconnect
	items = append(items, mk("Disconnect", func() {
		if err := Disconnect(context.Background()); err != nil {
			log.Printf("traytail: disconnect: %v", err)
		}
		a.requestRefresh()
	}))
	items = append(items, mkSep())
	items = append(items, mk("Admin console", func() {
		openURL("https://login.tailscale.com/admin/machines")
	}))
	items = append(items, mk("Quit", a.stop))
	return items
}

// exitSuffix labels the Exit node submenu parent with the current
// selection. Prefers the full city/country from `exit-node list`
// ("Exit node: Berlin"), falling back to the hostname.
func exitSuffix(exit *Peer, st *Status) string {
	if exit == nil {
		return ": off"
	}
	if nodes, err := GetExitNodes(context.Background()); err == nil {
		for _, n := range nodes {
			if n.Selected {
				return ": " + exitNodeLabel(n)
			}
		}
	}
	return ": " + exitHostName(*exit)
}

// smartPrimed reports whether the smart-auto state machine has been
// enabled (has observed a home/away state at least once).
func (a *app) smartPrimed() bool {
	s := a.smart
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.primed
}

// enableSmartAuto turns the smart policy on: evaluates the current
// home state and immediately applies the matching node.
func (a *app) enableSmartAuto() {
	if a.smart == nil {
		a.smart = &smartAuto{}
	}
	st, err := GetStatus(context.Background())
	if err != nil {
		log.Printf("traytail: smart auto enable: %v", err)
		return
	}
	home := AtHome(OwnExitNodeLANs(st))
	a.smart.mu.Lock()
	a.smart.atHome = home
	a.smart.primed = true
	if home {
		a.smart.lastMul = LoadLastMullvad()
	}
	a.smart.mu.Unlock()

	var target string
	if home {
		nodes, _ := GetExitNodes(context.Background())
		target = PickMullvadNode(st, nodes, a.smart.lastMul)
		if target != "" {
			a.smart.lastMul = target
			StoreLastMullvad(target)
		}
	} else {
		target = a.pickOwnNode(st)
	}
	if target == "" {
		log.Printf("traytail: smart auto enabled (home=%v): no target yet, will apply on next flip", home)
		return
	}
	log.Printf("traytail: smart auto enabled (home=%v) -> applying exit node %q", home, target)
	if err := SetExitNode(context.Background(), target); err != nil {
		log.Printf("traytail: smart auto: set exit node: %v", err)
	}
}

// disableSmartAuto turns the smart policy off.
func (a *app) disableSmartAuto() {
	a.smart = nil
}

// exitNodeLabel picks the friendliest name for an exit node row:
// city, then hostname-derived code, then hostname.
func exitNodeLabel(n ExitNodeInfo) string {
	if n.City != "" {
		return n.City
	}
	return exitHostName(Peer{HostName: n.Hostname})
}

// exitHostName renders a peer's hostname, stripping Mullvad hostname
// codes down to the city part when parseable.
func exitHostName(p Peer) string {
	name := p.HostName
	if p.IsMullvad() {
		if _, city := splitMullvad(name); city != "" {
			return city
		}
	}
	return name
}

// exitNodeMenu builds the exit node submenu: Off, Auto (smart), own
// nodes, and Mullvad nodes grouped by country (full names from
// `tailscale exit-node list`). The active country is hoisted to the
// top of the Mullvad list so the current selection is easy to find.
// Auto is traytail's smart policy: away -> own exit node (home LAN
// reachability), home -> last-used Mullvad node.
func (a *app) exitNodeMenu(st *Status, exit *Peer) []MenuItem {
	nodes, _ := GetExitNodes(context.Background())
	auto := exit != nil && a.smartPrimed()

	// In smart-auto mode traytail itself applied the concrete node, so
	// the city IS marked active — the label shows what carries traffic.
	activeHost := ""
	if exit != nil {
		activeHost = exit.HostName
	}

	items := []MenuItem{
		mkRadio("Off", exit == nil, func() {
			a.disableSmartAuto()
			if err := SetExitNode(context.Background(), ""); err != nil {
				log.Printf("traytail: unset exit node: %v", err)
			}
			a.requestRefresh()
		}),
		mkRadio("Auto (smart: away→own, home→Mullvad)", auto, func() {
			a.enableSmartAuto()
			a.requestRefresh()
		}),
	}

	var own []MenuItem
	mullvad := map[string][]ExitNodeInfo{} // country name -> nodes
	var countries []string
	seen := map[string]bool{} // hostname dedupe, prefer specific city over "Any"

	for _, n := range nodes {
		if n.Hostname == "" {
			continue
		}
		// "Any" rows duplicate a specific city row for the same host:
		// keep the first (specific) occurrence, skip the rest.
		base := hostBase(n.Hostname)
		if seen[base] {
			continue
		}
		seen[base] = true
		if !peerExitAvailableIP(st, n) {
			continue
		}
		if n.Country == "" {
			own = append(own, ownExitItem(n, listRowActive(n, exit), a))
			continue
		}
		if _, ok := mullvad[n.Country]; !ok {
			countries = append(countries, n.Country)
		}
		mullvad[n.Country] = append(mullvad[n.Country], n)
	}

	if len(own) > 0 {
		items = append(items, mkSep())
		items = append(items, own...)
	}
	if len(countries) > 0 {
		sort.Strings(countries)
		items = append(items, mkSep(), mkSub("Mullvad", mullvadSubmenu(countries, mullvad, activeHost, a)))
	}
	if len(items) == 1 {
		items = append(items, mkSep(), mk("No exit nodes available", nil))
	}
	return items
}

// listRowActive reports whether an exit-node list row is the currently
// active exit node. Matches by Tailscale IP first (rename-proof: the
// status peer's HostName may be user-renamed and unrelated to the DNS
// name in the list), then falls back to hostname heuristics.
func listRowActive(n ExitNodeInfo, exit *Peer) bool {
	if exit == nil {
		return false
	}
	if n.IP != "" {
		for _, ip := range exit.TailscaleIPs {
			if strings.TrimSuffix(ip, "/32") == n.IP {
				return true
			}
		}
	}
	return hostMatches(n.Hostname, exit.HostName) || hostMatches(n.Hostname, exit.BaseName())
}

// ownExitItem builds the radio item for a self-hosted exit node.
// Clicking the active item deselects it: traytail applies the last-used
// Mullvad node instead (explicit user choice, disables smart auto).
func ownExitItem(n ExitNodeInfo, active bool, a *app) MenuItem {
	label := n.Hostname
	if i := strings.Index(label, "."); i > 0 {
		label = label[:i] // strip domain: zds-nabara.tail... -> zds-nabara
	}
	return mkRadio(label, active, func() {
		a.disableSmartAuto()
		if active {
			a.applyLastMullvad()
			a.requestRefresh()
			return
		}
		if err := SetExitNode(context.Background(), n.Hostname); err != nil {
			log.Printf("traytail: set exit node: %v", err)
		}
		a.requestRefresh()
	})
}

// applyLastMullvad resolves and applies the last-used Mullvad node,
// persisting it. Returns the applied hostname ("" when none found).
func (a *app) applyLastMullvad() string {
	st, err := GetStatus(context.Background())
	if err != nil {
		log.Printf("traytail: last-used mullvad: %v", err)
		return ""
	}
	nodes, _ := GetExitNodes(context.Background())
	target := PickMullvadNode(st, nodes, LoadLastMullvad())
	if target == "" {
		log.Printf("traytail: last-used mullvad: no online Mullvad node available")
		return ""
	}
	StoreLastMullvad(target)
	log.Printf("traytail: own node deselected -> applying last-used Mullvad %q", target)
	if err := SetExitNode(context.Background(), target); err != nil {
		log.Printf("traytail: set exit node: %v", err)
		return ""
	}
	a.requestRefresh()
	return target
}

// mullvadSubmenu builds the country submenus, hoisting the active
// country to the top with a ● marker.
func mullvadSubmenu(countries []string, mullvad map[string][]ExitNodeInfo, activeHost string, a *app) []MenuItem {
	activeCountry := ""
	for _, c := range countries {
		for _, n := range mullvad[c] {
			if hostMatches(n.Hostname, activeHost) {
				activeCountry = c
				break
			}
		}
	}

	ordered := countries
	if activeCountry != "" {
		ordered = append([]string{activeCountry}, without(countries, activeCountry)...)
	}

	var out []MenuItem
	for _, c := range ordered {
		active := c == activeCountry
		label := c
		if active {
			label = "● " + c
		}
		out = append(out, mkSub(label, cityItems(mullvad[c], activeHost, a)))
	}
	return out
}

// cityItems renders the nodes of one country as city-labelled radio
// items, active first.
func cityItems(nodes []ExitNodeInfo, activeHost string, a *app) []MenuItem {
	var out []MenuItem
	for _, n := range nodes {
		n := n
		active := hostMatches(n.Hostname, activeHost)
		out = append(out, mkRadio(exitNodeLabel(n), active, func() {
			a.disableSmartAuto()
			if err := SetExitNode(context.Background(), n.Hostname); err != nil {
				log.Printf("traytail: set exit node: %v", err)
			}
			a.requestRefresh()
		}))
	}
	return out
}

// hostBase strips the trailing dot from a DNS name.
func hostBase(host string) string {
	return strings.TrimSuffix(host, ".")
}

// hostMatches reports whether an exit-node list hostname refers to
// the same node as a status peer name. The list carries full DNS
// names ("de-ber-wg-001.mullvad.ts.net") while status peers use the
// first label ("de-ber-wg-001"), so compare first labels too.
func hostMatches(listHost, peerHost string) bool {
	if listHost == peerHost || hostBase(listHost) == peerHost {
		return true
	}
	first := listHost
	if i := strings.IndexAny(first, "."); i > 0 {
		first = first[:i]
	}
	return first == peerHost
}

// peerExitAvailable reports whether the status peer list still shows
// this hostname as an online exit-node option.
func peerExitAvailable(st *Status, hostname string) bool {
	for _, p := range st.Peer {
		if !p.ExitNodeOption || !p.Online {
			continue
		}
		if hostMatches(hostname, p.HostName) || hostMatches(hostname, p.BaseName()) {
			return true
		}
	}
	return false
}

// peerExitAvailableIP additionally matches by Tailscale IP for
// renamed devices whose HostName shares nothing with the DNS name.
func peerExitAvailableIP(st *Status, n ExitNodeInfo) bool {
	if peerExitAvailable(st, n.Hostname) {
		return true
	}
	if n.IP == "" {
		return false
	}
	for _, p := range st.Peer {
		if !p.ExitNodeOption || !p.Online {
			continue
		}
		for _, ip := range p.TailscaleIPs {
			if strings.TrimSuffix(ip, "/32") == n.IP {
				return true
			}
		}
	}
	return false
}

func without(list []string, s string) []string {
	out := make([]string, 0, len(list))
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// splitMullvad parses "de-fra-wg-001" into ("de", "fra").
func splitMullvad(host string) (cc, city string) {
	parts := strings.SplitN(host, "-", 3)
	if len(parts) >= 2 {
		return parts[0], parts[1]
	}
	return host, ""
}

func (a *app) menuOffline(st *Status) []MenuItem {
	items := []MenuItem{
		mk("Tailscale: "+st.BackendState, nil),
		mkSep(),
		mk("Connect", func() {
			if err := Connect(context.Background()); err != nil {
				log.Printf("traytail: connect: %v", err)
			}
			a.requestRefresh()
		}),
	}
	if st.AuthURL != "" {
		items = append(items, mk("Log in", func() { openURL(st.AuthURL) }))
	}
	items = append(items, mkSep())
	items = append(items, mk("Quit", a.stop))
	return items
}

func (a *app) menuNeedsLogin(st *Status) []MenuItem {
	items := []MenuItem{mk("Login required", nil), mkSep()}
	if st.AuthURL != "" {
		items = append(items, mk("Open login page", func() { openURL(st.AuthURL) }), mkSep())
	} else {
		items = append(items, mk("Run 'tailscale up' to log in", nil), mkSep())
	}
	items = append(items, mk("Quit", a.stop))
	return items
}

// ---- helpers ----

func copyToClipboard(s string) {
	for _, tool := range [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
		cmd := execCommand(tool[0], tool[1:]...)
		cmd.Stdin = strings.NewReader(s)
		if err := cmd.Run(); err == nil {
			return
		}
	}
}

func openURL(url string) {
	execCommand("xdg-open", url).Start()
}

// dedupKey builds a string that changes whenever anything UI-relevant changes.
func dedupKey(st *Status, profiles []Profile, items []MenuItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|", st.BackendState, st.SelfIP(), st.AuthURL)
	if e := st.ExitNodePeer(); e != nil {
		b.WriteString(e.HostName)
	}
	b.WriteByte('|')
	for _, p := range st.SortPeers() {
		if p.ExitNodeOption {
			fmt.Fprintf(&b, "%s:%v,", p.HostName, p.Online)
		}
	}
	b.WriteByte('|')
	for _, p := range profiles {
		fmt.Fprintf(&b, "%s:%v,", p.ID, p.Selected)
	}
	return b.String()
}