package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

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
