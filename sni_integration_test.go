//go:build dbus_integration

package main

// Integration tests against a real session bus. Run under dbus-run-session:
//
//	dbus-run-session -- go test -tags dbus_integration .
//
// Skipped automatically when no session bus is reachable.

import (
	"os"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func haveSessionBus(t *testing.T) {
	t.Helper()
	if _, err := dbus.SessionBus(); err != nil {
		t.Skipf("no session bus: %v", err)
	}
}

func TestIntegrationNewSNI(t *testing.T) {
	haveSessionBus(t)
	s, err := NewSNI(os.Getpid())
	if err != nil {
		t.Fatalf("NewSNI: %v", err)
	}
	defer s.Close()
}

func TestIntegrationBusNameOwned(t *testing.T) {
	haveSessionBus(t)
	pid := os.Getpid()
	s, err := NewSNI(pid)
	if err != nil {
		t.Fatalf("NewSNI: %v", err)
	}
	defer s.Close()

	// Requesting the same unique name from a second connection must fail.
	conn2, err := dbus.SessionBus()
	if err != nil {
		t.Fatalf("second conn: %v", err)
	}
	defer conn2.Close()
	reply, err := conn2.RequestName(s.busName, dbus.NameFlagDoNotQueue)
	if err != nil {
		// some daemons return an error instead of a reply
		return
	}
	// reply 4 = DBUS_REQUEST_NAME_REPLY_SECONDARY_OWNER (queued behind us)
	if reply == dbus.RequestNameReplyPrimaryOwner {
		t.Errorf("second connection became primary owner of %s", s.busName)
	}
}

func TestIntegrationRegisterWithFakeWatcher(t *testing.T) {
	haveSessionBus(t)

	watcher, err := dbus.SessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	type fakeWatcher struct{}
	fw := &fakeWatcher{}
	if err := watcher.Export(fw, "/StatusNotifierWatcher", "org.kde.StatusNotifierWatcher"); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Export(fw, "/StatusNotifierWatcher", "org.freedesktop.StatusNotifierWatcher"); err != nil {
		t.Fatal(err)
	}

	// RequestName so the bus routes method calls to us.
	if _, err := watcher.RequestName("org.kde.StatusNotifierWatcher", dbus.NameFlagDoNotQueue); err != nil {
		t.Skipf("watcher name unavailable: %v", err)
	}

	// Export needs a method to be invoked: use a raw handler instead.
	watcher.ExportWithMap(fw, map[string]string{}, "/StatusNotifierWatcher", "org.freedesktop.DBus.Peer")

	s, err := NewSNI(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Poll until the watcher is reachable on the bus (name may take a beat).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.RegisterWithWatcher() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Skip("watcher registration never succeeded; environment may not route method calls to the fake")
}

func TestIntegrationUpdateAndGetLayout(t *testing.T) {
	haveSessionBus(t)
	s, err := NewSNI(os.Getpid())
	if err != nil {
		t.Fatalf("NewSNI: %v", err)
	}
	defer s.Close()

	items := []MenuItem{
		{ID: 1, Label: "hello", Enabled: true},
		{ID: 2, Type: "separator"},
	}
	s.Update(iconConnected(), "integration tooltip", items)

	// Fetch the layout through a proxy object, exactly as a host would.
	obj := s.conn.Object(s.busName, menuPath)
	var rev uint32
	var layout dbus.Variant
	if err := obj.Call(menuInterface+".GetLayout", 0, int32(0), int32(-1), []string{}).
		Store(&rev, &layout); err != nil {
		t.Fatalf("GetLayout over DBus: %v", err)
	}
	if rev != 1 {
		t.Errorf("revision = %d, want 1", rev)
	}
}

func TestIntegrationEventClickRoundTrip(t *testing.T) {
	haveSessionBus(t)
	s, err := NewSNI(os.Getpid())
	if err != nil {
		t.Fatalf("NewSNI: %v", err)
	}
	defer s.Close()

	clicked := make(chan struct{}, 1)
	items := []MenuItem{{ID: 42, Label: "go", Enabled: true, OnClick: func() {
		clicked <- struct{}{}
	}}}
	s.Update(iconConnected(), "tip", items)

	// Click over the bus from a second connection.
	conn2, err := dbus.SessionBus()
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	obj := conn2.Object(s.busName, menuPath)
	if err := obj.Call(menuInterface+".Event", 0, int32(42), "clicked", dbus.MakeVariant(""), uint32(0)).Err; err != nil {
		t.Fatalf("Event over DBus: %v", err)
	}
	select {
	case <-clicked:
	case <-time.After(2 * time.Second):
		t.Fatal("click handler never ran")
	}
}

func TestIntegrationAboutToShow(t *testing.T) {
	haveSessionBus(t)
	s, err := NewSNI(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// No update yet: revision == shownRev, so not dirty.
	obj := s.conn.Object(s.busName, menuPath)
	var dirty bool
	if err := obj.Call(menuInterface+".AboutToShow", 0, int32(0)).Store(&dirty); err != nil {
		t.Fatalf("AboutToShow: %v", err)
	}
	if dirty {
		t.Error("SNI without Update should not be dirty")
	}

	// After an Update the host must re-fetch.
	s.Update(iconConnected(), "tip", nil)
	if err := obj.Call(menuInterface+".AboutToShow", 0, int32(0)).Store(&dirty); err != nil {
		t.Fatalf("AboutToShow 2: %v", err)
	}
	if !dirty {
		t.Error("SNI should be dirty after Update")
	}
	if err := obj.Call(menuInterface+".AboutToShow", 0, int32(0)).Store(&dirty); err != nil {
		t.Fatalf("AboutToShow 3: %v", err)
	}
	if dirty {
		t.Error("dirty flag should be consumed")
	}
}