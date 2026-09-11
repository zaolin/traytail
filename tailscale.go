package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// interfaceAddrs is a seam for tests; production code uses net.InterfaceAddrs.
var interfaceAddrs = func() ([]net.Addr, error) { return net.InterfaceAddrs() }

// Status is the subset of `tailscale status --json` we care about.
type Status struct {
	BackendState string          `json:"BackendState"`
	AuthURL      string          `json:"AuthURL"`
	TailscaleIPs []string        `json:"TailscaleIPs"`
	Self         Peer            `json:"Self"`
	Peer         map[string]Peer `json:"Peer"`
}

type Peer struct {
	ID             string   `json:"ID"`
	HostName       string   `json:"HostName"`
	DNSName        string   `json:"DNSName"`
	Online         bool     `json:"Online"`
	ExitNodeOption bool     `json:"ExitNodeOption"`
	ExitNode       bool     `json:"ExitNode"`
	Tags           []string `json:"Tags"`
	TailscaleIPs   []string `json:"TailscaleIPs"`
	PrimaryRoutes  []string `json:"PrimaryRoutes"`
}

type Profile struct {
	ID       string `json:"id"`
	Nickname string `json:"nickname"`
	Tailnet  string `json:"tailnet"`
	Account  string `json:"account"`
	Selected bool   `json:"selected"`
}

func run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tailscale", args...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func GetStatus(ctx context.Context) (*Status, error) {
	out, err := run(ctx, "status", "--json")
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w", err)
	}
	var st Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return nil, fmt.Errorf("parse status: %w", err)
	}
	return &st, nil
}

func GetProfiles(ctx context.Context) ([]Profile, error) {
	out, err := run(ctx, "switch", "--list", "--json")
	if err != nil {
		return nil, fmt.Errorf("tailscale switch --list: %w", err)
	}
	var ps []Profile
	if err := json.Unmarshal([]byte(out), &ps); err != nil {
		return nil, fmt.Errorf("parse profiles: %w", err)
	}
	return ps, nil
}

func SwitchProfile(ctx context.Context, id string) error {
	_, err := run(ctx, "switch", id)
	return err
}

func Connect(ctx context.Context) error {
	_, err := run(ctx, "up")
	return err
}

func Disconnect(ctx context.Context) error {
	_, err := run(ctx, "down")
	return err
}

// SetExitNode selects an exit node by base DNS name, or "" to disable.
func SetExitNode(ctx context.Context, baseName string) error {
	_, err := run(ctx, "set", "--exit-node="+baseName)
	return err
}

// ExitNodeInfo is one row of `tailscale exit-node list`.
type ExitNodeInfo struct {
	IP       string
	Hostname string // full DNS name, e.g. de-ber-wg-001.mullvad.ts.net.
	Country  string // full country name; empty for own nodes
	City     string // full city name; empty for own nodes / "Any" dupes kept
	Selected bool
}

// debugPrefs is the subset of `tailscale debug prefs` we care about.
// AutoExitNode is "any" when auto:any is active and absent/empty for
// manual selections — the only reliable way to tell auto mode from a
// resolved concrete node.
type debugPrefs struct {
	AutoExitNode string `json:"AutoExitNode"`
}

// AutoExitNodeActive reports whether auto:any exit-node mode is on.
func AutoExitNodeActive(ctx context.Context) bool {
	out, err := run(ctx, "debug", "prefs")
	if err != nil {
		return false
	}
	var p debugPrefs
	if json.Unmarshal([]byte(out), &p) != nil {
		return false
	}
	return p.AutoExitNode != ""
}

// GetExitNodes parses the tabwriter output of `tailscale exit-node list`.
// Columns are separated by two or more spaces so multi-word cities
// ("Buenos Aires") survive. Own exit nodes have "-" for country/city.
func GetExitNodes(ctx context.Context) ([]ExitNodeInfo, error) {
	out, err := run(ctx, "exit-node", "list")
	if err != nil {
		return nil, fmt.Errorf("tailscale exit-node list: %w", err)
	}
	var nodes []ExitNodeInfo
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "  ") || strings.HasPrefix(line, " IP ") {
			continue // header or no data
		}
		fields := strings.Split(strings.TrimSpace(line), "  ")
		fields = nonEmpty(fields)
		if len(fields) < 5 {
			continue
		}
		n := ExitNodeInfo{
			IP:       strings.TrimSpace(fields[0]),
			Hostname: strings.TrimSpace(fields[1]),
			Country:  dashToEmpty(fields[2]),
			City:     dashToEmpty(fields[3]),
		}
		n.Selected = strings.TrimSpace(fields[4]) == "selected"
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("parse exit-node list: no rows in output")
	}
	return nodes, nil
}

func dashToEmpty(s string) string {
	s = strings.TrimSpace(s)
	if s == "-" {
		return ""
	}
	return s
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func AdvertiseExitNode(ctx context.Context, enable bool) error {
	_, err := run(ctx, "set", fmt.Sprintf("--advertise-exit-node=%v", enable))
	return err
}

// SelfIP returns the primary IPv4 of this node, or "".
func (s *Status) SelfIP() string {
	for _, ip := range s.TailscaleIPs {
		if strings.Contains(ip, ":") {
			continue
		}
		return ip
	}
	if len(s.TailscaleIPs) > 0 {
		return s.TailscaleIPs[0]
	}
	return ""
}

// Running reports whether tailscaled is up and connected.
func (s *Status) Running() bool {
	return s.BackendState == "Running"
}

func (p Peer) IsMullvad() bool {
	for _, t := range p.Tags {
		if strings.HasPrefix(t, "tag:mullvad-") || t == "tag:mullvad" {
			return true
		}
	}
	return strings.Contains(p.DNSName, "mullvad.ts.net")
}

// BaseName strips the trailing dot from a DNS name.
func (p Peer) BaseName() string {
	return strings.TrimSuffix(p.DNSName, ".")
}

// ExitNodePeer returns the peer currently used as exit node, or nil.
func (s *Status) ExitNodePeer() *Peer {
	for id := range s.Peer {
		if s.Peer[id].ExitNode {
			p := s.Peer[id]
			return &p
		}
	}
	return nil
}

// SortPeers returns the peers sorted by hostname.
func (s *Status) SortPeers() []Peer {
	ps := make([]Peer, 0, len(s.Peer))
	for _, p := range s.Peer {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].HostName < ps[j].HostName })
	return ps
}

// ---- smart auto: home/away detection + last-used Mullvad ----

// OwnExitNodeLANs returns the subnets advertised (PrimaryRoutes) by
// online own exit nodes — the "home networks" behind them.
func OwnExitNodeLANs(st *Status) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range st.Peer {
		if !p.ExitNodeOption || !p.Online || p.IsMullvad() {
			continue
		}
		for _, r := range p.PrimaryRoutes {
			if pre, err := netip.ParsePrefix(r); err == nil {
				out = append(out, pre)
			}
		}
	}
	return out
}

// AtHome reports whether any local interface address falls inside one
// of the prefixes (own exit nodes' advertised LANs).
func AtHome(prefixes []netip.Prefix) bool {
	if len(prefixes) == 0 {
		return false
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipnet.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		for _, pre := range prefixes {
			if pre.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// smartStatePath returns the file storing the last-used Mullvad node.
func smartStatePath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "traytail", "last-mullvad"), nil
}

// LoadLastMullvad reads the last-used Mullvad hostname; "" when absent.
func LoadLastMullvad() string {
	p, err := smartStatePath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// StoreLastMullvad persists the last-used Mullvad hostname.
func StoreLastMullvad(hostname string) error {
	p, err := smartStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(hostname+"\n"), 0o644)
}

// PickMullvadNode chooses the Mullvad node to use: the last-used one
// if still an online option, otherwise the current "selected" row of
// the exit-node list, otherwise the first online Mullvad peer.
func PickMullvadNode(st *Status, nodes []ExitNodeInfo, lastUsed string) string {
	online := map[string]bool{}
	bareName := map[string]string{} // every accepted spelling -> peer HostName
	for _, p := range st.Peer {
		if p.IsMullvad() && p.ExitNodeOption && p.Online {
			online[p.HostName] = true
			online[p.BaseName()] = true
			bareName[p.HostName] = p.HostName
			bareName[p.BaseName()] = p.HostName
		}
	}
	if lastUsed != "" && online[lastUsed] {
		return bareName[lastUsed]
	}
	for _, n := range nodes {
		if n.Selected && n.Country != "" {
			if bare, ok := bareName[hostBase(n.Hostname)]; ok {
				return bare
			}
		}
	}
	for _, p := range st.SortPeers() {
		if p.IsMullvad() && p.ExitNodeOption && p.Online {
			return p.HostName
		}
	}
	return ""
}
