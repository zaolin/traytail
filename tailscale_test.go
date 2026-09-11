package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeTailscale installs a shell-script `tailscale` on a temp PATH that
// records its arguments into a file and prints canned responses.
type fakeTailscale struct {
	dir     string
	logPath string

	mu     sync.Mutex
	calls  [][]string
	stdout map[string]string // keyed by args joined with spaces
	fails  map[string]bool   // args -> exit 1
}

func installFakeTailscale(t *testing.T) *fakeTailscale {
	t.Helper()
	ft := &fakeTailscale{
		stdout: map[string]string{},
		fails:  map[string]bool{},
	}
	ft.dir = t.TempDir()
	ft.logPath = filepath.Join(ft.dir, "calls.log")

	script := "#!/bin/sh\necho \"$@\" >> " + ft.logPath + "\nif [ -f " + filepath.Join(ft.dir, "fail") + " ]; then echo 'simulated cli failure' >&2; exit 1; fi\ncat " + filepath.Join(ft.dir, "resp.txt") + " 2>/dev/null || true\n"
	binDir := filepath.Join(ft.dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "tailscale"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() { os.Remove(ft.logPath) })
	return ft
}

func (ft *fakeTailscale) callsSoFar(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(ft.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		calls = append(calls, strings.Fields(line))
	}
	return calls
}

func (ft *fakeTailscale) setFail(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ft.dir, "fail"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setAlternatingResponses rewrites the fake CLI so that every
// `status --json` invocation alternates between two payloads (state
// flips escape the dedup key), while any other subcommand returns an
// empty JSON array without touching the toggle.
func (ft *fakeTailscale) setAlternatingResponses(t *testing.T) {
	t.Helper()
	cli := filepath.Join(ft.dir, "bin", "tailscale")
	r1 := `{"BackendState":"Running","TailscaleIPs":["100.64.0.1"],"Self":{"HostName":"h"}}`
	r2 := `{"BackendState":"Running","TailscaleIPs":["100.64.0.2"],"Self":{"HostName":"h"}}`
	alt := "#!/bin/sh\necho \"$@\" >> " + ft.logPath + "\n" +
		"if [ \"$1\" != \"status\" ]; then echo '[]'; exit 0; fi\n" +
		"if [ -f " + ft.dir + "/toggle ]; then rm " + ft.dir + "/toggle; echo '" + r2 + "'; else echo x > " + ft.dir + "/toggle; echo '" + r1 + "'; fi\n"
	if err := os.WriteFile(cli, []byte(alt), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (ft *fakeTailscale) setResponse(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ft.dir, "resp.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (ft *fakeTailscale) lastCall(t *testing.T) []string {
	t.Helper()
	calls := ft.callsSoFar(t)
	if len(calls) == 0 {
		t.Fatal("no tailscale CLI calls recorded")
	}
	return calls[len(calls)-1]
}

func TestGetStatus(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, `{"BackendState":"Running","TailscaleIPs":["100.64.0.1","fd7a::1"]}`)

	st, err := GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.BackendState != "Running" {
		t.Errorf("BackendState = %q, want Running", st.BackendState)
	}
	if got := ft.lastCall(t); len(got) != 2 || got[0] != "status" || got[1] != "--json" {
		t.Errorf("call = %v, want [status --json]", got)
	}
}

func TestGetStatusCLIError(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setFail(t)
	if _, err := GetStatus(context.Background()); err == nil {
		t.Fatal("GetStatus should fail when CLI fails")
	}
}

func TestGetStatusBadJSON(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, "not json")
	if _, err := GetStatus(context.Background()); err == nil {
		t.Fatal("GetStatus should fail on invalid JSON")
	}
}

func TestGetProfiles(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, `[{"id":"a","nickname":"a@x","tailnet":"x.io","account":"a@x","selected":true}]`)

	ps, err := GetProfiles(context.Background())
	if err != nil {
		t.Fatalf("GetProfiles: %v", err)
	}
	if len(ps) != 1 || !ps[0].Selected || ps[0].Tailnet != "x.io" {
		t.Fatalf("profiles = %+v", ps)
	}
	if got := ft.lastCall(t); len(got) != 3 || got[0] != "switch" || got[1] != "--list" {
		t.Errorf("call = %v", got)
	}
}

func TestGetProfilesBadJSON(t *testing.T) {
	ft := installFakeTailscale(t)
	ft.setResponse(t, "nope")
	if _, err := GetProfiles(context.Background()); err == nil {
		t.Fatal("GetProfiles should fail on invalid JSON")
	}
}

func TestSwitchProfile(t *testing.T) {
	ft := installFakeTailscale(t)
	if err := SwitchProfile(context.Background(), "prof-1"); err != nil {
		t.Fatalf("SwitchProfile: %v", err)
	}
	if got := ft.lastCall(t); len(got) != 2 || got[1] != "prof-1" {
		t.Errorf("call = %v, want [switch prof-1]", got)
	}
}

func TestConnectDisconnect(t *testing.T) {
	ft := installFakeTailscale(t)
	if err := Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := Disconnect(context.Background()); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	calls := ft.callsSoFar(t)
	if len(calls) != 2 || calls[0][0] != "up" || calls[1][0] != "down" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestSetExitNode(t *testing.T) {
	ft := installFakeTailscale(t)
	if err := SetExitNode(context.Background(), "de-fra-wg-001.example.com"); err != nil {
		t.Fatalf("SetExitNode: %v", err)
	}
	if err := SetExitNode(context.Background(), ""); err != nil {
		t.Fatalf("SetExitNode(empty): %v", err)
	}
	calls := ft.callsSoFar(t)
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	if calls[0][1] != "--exit-node=de-fra-wg-001.example.com" {
		t.Errorf("call 1 = %v", calls[0])
	}
	if calls[1][1] != "--exit-node=" {
		t.Errorf("call 2 = %v, want empty exit-node", calls[1])
	}
}

func TestAdvertiseExitNode(t *testing.T) {
	ft := installFakeTailscale(t)
	if err := AdvertiseExitNode(context.Background(), true); err != nil {
		t.Fatalf("AdvertiseExitNode: %v", err)
	}
	if got := ft.lastCall(t); got[1] != "--advertise-exit-node=true" {
		t.Errorf("call = %v", got)
	}
	if err := AdvertiseExitNode(context.Background(), false); err != nil {
		t.Fatalf("AdvertiseExitNode(false): %v", err)
	}
	if got := ft.lastCall(t); got[1] != "--advertise-exit-node=false" {
		t.Errorf("call = %v", got)
	}
}

func TestSelfIP(t *testing.T) {
	tests := []struct {
		ips  []string
		want string
	}{
		{[]string{"100.64.0.1", "fd7a::1"}, "100.64.0.1"},
		{[]string{"fd7a::1", "100.64.0.1"}, "100.64.0.1"},
		{[]string{"fd7a::1"}, "fd7a::1"},
		{nil, ""},
	}
	for _, tt := range tests {
		st := &Status{TailscaleIPs: tt.ips}
		if got := st.SelfIP(); got != tt.want {
			t.Errorf("SelfIP(%v) = %q, want %q", tt.ips, got, tt.want)
		}
	}
}

func TestRunning(t *testing.T) {
	if (&Status{BackendState: "Running"}).Running() != true {
		t.Error("Running state should report true")
	}
	if (&Status{BackendState: "Stopped"}).Running() != false {
		t.Error("Stopped state should report false")
	}
}

func TestIsMullvad(t *testing.T) {
	tests := []struct {
		name string
		p    Peer
		want bool
	}{
		{"tag prefix", Peer{Tags: []string{"tag:mullvad-exit-node-de"}}, true},
		{"bare tag", Peer{Tags: []string{"tag:mullvad"}}, true},
		{"dns name", Peer{DNSName: "de-fra-wg-001.mullvad.ts.net."}, true},
		{"other tag", Peer{Tags: []string{"tag:server"}}, false},
		{"no markers", Peer{HostName: "homeserver", DNSName: "homeserver.tail.net."}, false},
	}
	for _, tt := range tests {
		if got := tt.p.IsMullvad(); got != tt.want {
			t.Errorf("%s: IsMullvad = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestBaseName(t *testing.T) {
	p := Peer{DNSName: "host.tailnet.ts.net."}
	if got := p.BaseName(); got != "host.tailnet.ts.net" {
		t.Errorf("BaseName = %q", got)
	}
	p2 := Peer{DNSName: "no-dot"}
	if got := p2.BaseName(); got != "no-dot" {
		t.Errorf("BaseName(no dot) = %q", got)
	}
}

func TestExitNodePeer(t *testing.T) {
	st := &Status{Peer: map[string]Peer{
		"a": {HostName: "a"},
		"b": {HostName: "b", ExitNode: true},
	}}
	e := st.ExitNodePeer()
	if e == nil || e.HostName != "b" {
		t.Fatalf("ExitNodePeer = %+v, want b", e)
	}
	if (&Status{}).ExitNodePeer() != nil {
		t.Error("empty status should have nil exit node")
	}
}

func TestSortPeers(t *testing.T) {
	st := &Status{Peer: map[string]Peer{
		"z": {HostName: "zebra"},
		"a": {HostName: "apple"},
		"m": {HostName: "mango"},
	}}
	got := st.SortPeers()
	if len(got) != 3 || got[0].HostName != "apple" || got[1].HostName != "mango" || got[2].HostName != "zebra" {
		t.Fatalf("SortPeers = %+v", got)
	}
}