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
	a := &app{ctx: ctx, stop: stop, sni: sni, refreshCh: make(chan struct{}, 1)}
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

	icon, tooltip, items := a.buildUI(st, profiles)
	key := dedupKey(st, profiles, items)
	if key == a.last {
		return
	}
	a.last = key
	a.sni.Update(icon, tooltip, items)
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

func mkCheck(label string, checked bool, onClick func()) MenuItem {
	return MenuItem{ID: newID(), Label: label, Enabled: true, Toggle: "checkmark", Checked: checked, OnClick: onClick}
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
				profItems = append(profItems, mkCheck(p.Nickname, true, nil))
			} else {
				profItems = append(profItems, mkCheck(p.Nickname, false, func() {
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
	items = append(items, mkSub("Exit node"+exitSuffix(exit), a.exitNodeMenu(st)))

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
// selection, e.g. "Exit node: fra" or "Exit node: off".
func exitSuffix(exit *Peer) string {
	if exit == nil {
		return ": off"
	}
	name := exit.HostName
	if exit.IsMullvad() {
		if _, city := splitMullvad(name); city != "" {
			return ": " + city
		}
	}
	return ": " + name
}

// exitNodeToggle returns a checkmark entry that selects the peer, or
// deselects it when it is already active (radio behavior: unchecking
// the sole active node means "no exit node").
func (a *app) exitNodeToggle(p Peer, active bool) MenuItem {
	if active {
		return mkCheck(p.HostName, true, func() {
			if err := SetExitNode(context.Background(), ""); err != nil {
				log.Printf("traytail: unset exit node: %v", err)
			}
			a.requestRefresh()
		})
	}
	return mkCheck(p.HostName, false, func() {
		if err := SetExitNode(context.Background(), p.BaseName()); err != nil {
			log.Printf("traytail: set exit node: %v", err)
		}
		a.requestRefresh()
	})
}

// exitNodeMenu builds the exit node submenu: Auto (best), own nodes,
// and Mullvad nodes grouped by country behind a single "Mullvad" node.
// Clicking the active entry turns exit routing off — there is no
// separate "None" entry.
func (a *app) exitNodeMenu(st *Status) []MenuItem {
	exit := st.ExitNodePeer()
	peers := st.SortPeers()

	items := []MenuItem{a.autoToggle(exit)}

	var own []MenuItem
	mullvad := map[string][]MenuItem{} // country code -> nodes
	var countries []string

	for _, p := range peers {
		if !p.ExitNodeOption || !p.Online {
			continue
		}
		active := exit != nil && exit.HostName == p.HostName
		if p.IsMullvad() {
			cc, _ := splitMullvad(p.HostName)
			if _, ok := mullvad[cc]; !ok {
				countries = append(countries, cc)
			}
			mullvad[cc] = append(mullvad[cc], a.exitNodeToggle(p, active))
		} else {
			own = append(own, a.exitNodeToggle(p, active))
		}
	}

	if len(own) > 0 {
		items = append(items, mkSep())
		items = append(items, own...)
	}
	if len(countries) > 0 {
		sort.Strings(countries)
		var countryNodes []MenuItem
		for _, cc := range countries {
			countryNodes = append(countryNodes, mkSub(strings.ToUpper(cc), mullvad[cc]))
		}
		items = append(items, mkSep(), mkSub("Mullvad", countryNodes))
	}
	if exit == nil && len(items) == 1 {
		items = append(items, mkSep(), mk("No exit nodes available", nil))
	}
	return items
}

// autoToggle is the "Auto (best)" entry backed by tailscale's auto:any
// exit node. Active while no concrete peer is selected.
func (a *app) autoToggle(exit *Peer) MenuItem {
	if exit == nil {
		return mkCheck("Auto (best)", true, func() {
			if err := SetExitNode(context.Background(), ""); err != nil {
				log.Printf("traytail: unset exit node: %v", err)
			}
			a.requestRefresh()
		})
	}
	return mkCheck("Auto (best)", false, func() {
		if err := SetExitNode(context.Background(), "auto:any"); err != nil {
			log.Printf("traytail: set auto exit node: %v", err)
		}
		a.requestRefresh()
	})
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