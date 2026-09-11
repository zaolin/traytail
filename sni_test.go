package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func mkItems() []MenuItem {
	return []MenuItem{
		{ID: 1, Label: "one", Enabled: true, OnClick: func() {}},
		{ID: 2, Type: "separator"},
		{ID: 3, Label: "check", Toggle: "checkmark", Checked: true, Enabled: true},
		{ID: 4, Label: "sub", Enabled: true, Submenu: []MenuItem{
			{ID: 5, Label: "inner", Enabled: true},
		}},
	}
}

func TestNodeFromItem(t *testing.T) {
	n := nodeFromItem(MenuItem{ID: 7, Label: "hi", Enabled: true})
	if n.ID != 7 {
		t.Errorf("ID = %d", n.ID)
	}
	if v, ok := n.Props["label"]; !ok || v.Value().(string) != "hi" {
		t.Errorf("label prop = %v", n.Props["label"])
	}
	if v := n.Props["enabled"].Value().(bool); !v {
		t.Error("enabled should be true")
	}
	if len(n.Children) != 0 {
		t.Error("leaf node should have no children")
	}
}

func TestNodeFromItemSeparator(t *testing.T) {
	n := nodeFromItem(MenuItem{ID: 2, Type: "separator"})
	if v := n.Props["type"].Value().(string); v != "separator" {
		t.Errorf("type = %q", v)
	}
	if _, ok := n.Props["label"]; ok {
		t.Error("separator should not carry a label prop")
	}
}

func TestNodeFromItemSubmenu(t *testing.T) {
	n := nodeFromItem(MenuItem{ID: 4, Label: "sub", Submenu: []MenuItem{
		{ID: 5, Label: "inner"},
	}})
	if v := n.Props["children-display"].Value().(string); v != "submenu" {
		t.Errorf("children-display = %q", v)
	}
	if len(n.Children) != 1 {
		t.Fatalf("children = %d", len(n.Children))
	}
	child := n.Children[0].Value().(layoutNode)
	if child.ID != 5 || child.Props["label"].Value().(string) != "inner" {
		t.Errorf("child = %+v", child)
	}
}

func TestNodeFromItemToggle(t *testing.T) {
	on := nodeFromItem(MenuItem{ID: 3, Label: "c", Toggle: "checkmark", Checked: true})
	if v := on.Props["toggle-type"].Value().(string); v != "checkmark" {
		t.Errorf("toggle-type = %q", v)
	}
	if v := on.Props["toggle-state"].Value().(int32); v != 1 {
		t.Errorf("toggle-state = %d, want 1", v)
	}
	off := nodeFromItem(MenuItem{ID: 3, Label: "c", Toggle: "checkmark", Checked: false})
	if v := off.Props["toggle-state"].Value().(int32); v != 0 {
		t.Errorf("toggle-state = %d, want 0", v)
	}
}

func TestNodeFromItemDisabled(t *testing.T) {
	n := nodeFromItem(MenuItem{ID: 9, Label: "x", Enabled: false})
	if v := n.Props["enabled"].Value().(bool); v {
		t.Error("enabled should be false")
	}
	if v := n.Props["visible"].Value().(bool); !v {
		t.Error("visible should be true")
	}
}

func TestNodeFromItemEmptyLabelOmitted(t *testing.T) {
	n := nodeFromItem(MenuItem{ID: 10})
	if _, ok := n.Props["label"]; ok {
		t.Error("empty label should be omitted")
	}
}

func newTestSNI(t *testing.T) *SNI {
	t.Helper()
	s := &SNI{busName: "test", idToLabel: map[int32]string{}}
	s.Update(iconConnected(), "tooltip", mkItems())
	return s
}

func TestFindItem(t *testing.T) {
	s := newTestSNI(t)
	if it := s.findItem(s.items, 1); it == nil || it.Label != "one" {
		t.Errorf("find top-level: %+v", it)
	}
	if it := s.findItem(s.items, 5); it == nil || it.Label != "inner" {
		t.Errorf("find nested: %+v", it)
	}
	if it := s.findItem(s.items, 99); it != nil {
		t.Errorf("unknown id should return nil, got %+v", it)
	}
}

func TestGetLayout(t *testing.T) {
	s := newTestSNI(t)
	rev, root, derr := s.GetLayout(0, -1, nil)
	if derr != nil {
		t.Fatalf("GetLayout error: %v", derr)
	}
	if rev != 1 {
		t.Errorf("revision = %d, want 1", rev)
	}
	if root.ID != 0 || len(root.Children) != 4 {
		t.Fatalf("root = ID %d, %d children", root.ID, len(root.Children))
	}
	first := root.Children[0].Value().(layoutNode)
	if first.ID != 1 {
		t.Errorf("first child ID = %d", first.ID)
	}
}

func TestGetLayoutIncrement(t *testing.T) {
	s := newTestSNI(t)
	s.Update(iconOffline(), "t2", mkItems())
	rev, _, _ := s.GetLayout(0, -1, nil)
	if rev != 2 {
		t.Errorf("revision = %d, want 2", rev)
	}
}

func TestGetProperty(t *testing.T) {
	s := newTestSNI(t)
	cases := []struct {
		id   int32
		name string
		want any
	}{
		{1, "label", "one"},
		{1, "enabled", true},
		{1, "visible", true},
		{1, "type", "standard"},
		{2, "type", "separator"},
		{3, "toggle-type", "checkmark"},
		{3, "toggle-state", int32(1)},
		{1, "unknown-prop", ""},
	}
	for _, tc := range cases {
		v, derr := s.GetProperty(tc.id, tc.name)
		if derr != nil {
			t.Fatalf("GetProperty(%d, %s): %v", tc.id, tc.name, derr)
		}
		if got := v.Value(); got != tc.want {
			t.Errorf("GetProperty(%d, %s) = %v (%T), want %v (%T)", tc.id, tc.name, got, got, tc.want, tc.want)
		}
	}
}

func TestGetPropertyUnknownItem(t *testing.T) {
	s := newTestSNI(t)
	_, derr := s.GetProperty(999, "label")
	if derr == nil {
		t.Fatal("expected error for unknown item")
	}
	if derr.Name != "org.freedesktop.DBus.Error.Failed" {
		t.Errorf("error name = %q", derr.Name)
	}
}

func TestGetPropertyUntoggledItem(t *testing.T) {
	s := newTestSNI(t)
	// item 1 has no toggle: toggle-type must be empty string
	v, _ := s.GetProperty(1, "toggle-type")
	if got := v.Value().(string); got != "" {
		t.Errorf("toggle-type = %q, want empty", got)
	}
	v, _ = s.GetProperty(1, "toggle-state")
	if got := v.Value().(int32); got != 0 {
		t.Errorf("toggle-state = %d, want 0", got)
	}
}

func TestEventClicks(t *testing.T) {
	var clicked atomic.Int32
	items := []MenuItem{
		{ID: 1, Label: "ok", Enabled: true, OnClick: func() { clicked.Add(1) }},
		{ID: 2, Label: "no-op"},
	}
	s := &SNI{busName: "test", idToLabel: map[int32]string{}}
	s.Update(iconConnected(), "tip", items)

	if derr := s.Event(1, "clicked", dbus.Variant{}, 0); derr != nil {
		t.Fatalf("Event: %v", derr)
	}
	// click handler runs async
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && clicked.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if clicked.Load() != 1 {
		t.Error("click handler did not run")
	}

	// non-clicked events should not fire
	if derr := s.Event(1, "hovered", dbus.Variant{}, 0); derr != nil {
		t.Fatalf("Event(hovered): %v", derr)
	}
	time.Sleep(20 * time.Millisecond)
	if got := clicked.Load(); got != 1 {
		t.Errorf("clicked = %d after hover, want 1", got)
	}

	// unknown id: no panic, no error
	if derr := s.Event(999, "clicked", dbus.Variant{}, 0); derr != nil {
		t.Fatalf("Event(unknown): %v", derr)
	}
}

func TestEventNoHandler(t *testing.T) {
	s := newTestSNI(t)
	if derr := s.Event(3, "clicked", dbus.Variant{}, 0); derr != nil {
		t.Fatalf("Event with nil OnClick: %v", derr)
	}
}

func TestEventGroup(t *testing.T) {
	s := newTestSNI(t)
	if derr := s.EventGroup([]int32{1, 2}, "clicked", dbus.Variant{}, 0); derr != nil {
		t.Errorf("EventGroup: %v", derr)
	}
}

func TestGetGroupProperties(t *testing.T) {
	s := newTestSNI(t)
	got, derr := s.GetGroupProperties([]int32{1, 2, 999}, nil)
	if derr != nil {
		t.Fatalf("GetGroupProperties: %v", derr)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (unknown id skipped)", len(got))
	}
	if got[0].ID != 1 || got[0].Props["label"].Value().(string) != "one" {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[1].ID != 2 || got[1].Props["type"].Value().(string) != "separator" {
		t.Errorf("entry 1 = %+v", got[1])
	}
}

func TestGetGroupPropertiesEmpty(t *testing.T) {
	s := newTestSNI(t)
	got, _ := s.GetGroupProperties(nil, nil)
	if len(got) != 0 {
		t.Errorf("got = %v", got)
	}
}

func TestAboutToShowCycle(t *testing.T) {
	s := newTestSNI(t)

	dirty, _ := s.AboutToShow(0)
	if !dirty {
		t.Error("fresh Update should be dirty")
	}
	dirtyAgain, _ := s.AboutToShow(0)
	if dirtyAgain {
		t.Error("AboutToShow must consume the dirty flag")
	}

	s.Update(iconOffline(), "tip", mkItems())
	ids, _ := s.AboutToShowGroup([]int32{1})
	if len(ids) != 1 {
		t.Errorf("AboutToShowGroup dirty = %v", ids)
	}
	ids, _ = s.AboutToShowGroup([]int32{1})
	if len(ids) != 0 {
		t.Errorf("AboutToShowGroup must consume the dirty flag, got %v", ids)
	}

	// consume: AboutToShow should flip to false until next Update
	dirty, _ = s.AboutToShow(0)
	if dirty {
		t.Error("AboutToShow should be false after being consumed")
	}

	s.Update(iconOffline(), "tip", mkItems())
	dirty, _ = s.AboutToShow(0)
	if !dirty {
		t.Error("Update should mark dirty again")
	}
}

func TestAboutToShowGroupClean(t *testing.T) {
	s := newTestSNI(t)
	if _, err := s.AboutToShow(0); err != nil {
		t.Fatal(err)
	} // consume dirty flag
	ids, _ := s.AboutToShowGroup([]int32{5, 6})
	if len(ids) != 0 {
		t.Errorf("clean state should return empty ids, got %v", ids)
	}
}

func TestPropertiesStubs(t *testing.T) {
	s := newTestSNI(t)
	if v, _ := s.Get("any", "thing"); v.String() != `""` {
		t.Errorf("Get = %v", v)
	}
	all, _ := s.GetAll("any")
	if len(all) != 0 {
		t.Errorf("GetAll = %v", all)
	}
	if derr := s.Set("any", "thing", dbus.Variant{}); derr == nil ||
		derr.Name != "org.freedesktop.DBus.Error.PropertyNotFound" {
		t.Errorf("Set = %v", derr)
	}
}

func TestSNIActivateStubs(t *testing.T) {
	s := newTestSNI(t)
	if derr := s.Activate(0, 0); derr != nil {
		t.Error("Activate")
	}
	if derr := s.SecondaryActivate(0, 0); derr != nil {
		t.Error("SecondaryActivate")
	}
	if derr := s.Scroll(1, "vertical"); derr != nil {
		t.Error("Scroll")
	}
}

func TestLabelItemsFlatten(t *testing.T) {
	out := map[int32]string{}
	labelItems(mkItems(), out)
	if len(out) != 5 {
		t.Fatalf("flattened %d labels, want 5: %v", len(out), out)
	}
	if out[5] != "inner" {
		t.Errorf("out[5] = %q", out[5])
	}
}

func TestUpdatePopulatesIDMap(t *testing.T) {
	s := newTestSNI(t)
	s.mu.Lock()
	n := len(s.idToLabel)
	_, ok := s.idToLabel[5]
	s.mu.Unlock()
	if n != 5 || !ok {
		t.Errorf("idToLabel = %d entries, missing 5: %v", n, s.idToLabel)
	}
}

func TestUpdateNilConn(t *testing.T) {
	s := &SNI{busName: "test", idToLabel: map[int32]string{}}
	s.conn = nil // simulate closed connection: must not panic
	done := make(chan struct{})
	go func() {
		s.Update(iconConnected(), "tip", mkItems())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Update with nil conn hung")
	}
}

func TestEmitSignalNilConn(t *testing.T) {
	s := &SNI{busName: "test"}
	done := make(chan struct{})
	go func() {
		s.emitSignal("NewIcon")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emitSignal with nil conn hung")
	}
}

func TestRegisterWithWatcherNilConn(t *testing.T) {
	s := &SNI{busName: "test", idToLabel: map[int32]string{}}
	if s.RegisterWithWatcher() {
		t.Error("nil conn should report registration failure")
	}
}

func TestMenuItemStaysConsistentUnderConcurrency(t *testing.T) {
	s := newTestSNI(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.GetLayout(0, -1, nil)
			s.GetProperty(1, "label")
			s.GetGroupProperties([]int32{1, 2}, nil)
			s.AboutToShow(0)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			s.Update(iconOffline(), "tip", mkItems())
		}
	}()
	wg.Wait()
}