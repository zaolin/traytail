package main

import (
	"fmt"
	"log"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

// Pixmap is one entry of the SNI IconPixmap property: (width, height, ARGB32).
type Pixmap struct {
	Width  int32
	Height int32
	Bytes  []byte
}

const (
	sniInterface  = "org.kde.StatusNotifierItem"
	sniPath       = "/StatusNotifierItem"
	menuInterface = "com.canonical.dbusmenu"
	menuPath      = "/MenuBar"
	watcherName   = "org.kde.StatusNotifierWatcher"
	busNameBase   = "org.kde.StatusNotifierItem-"
)

// MenuItem is one entry in the DBusMenu tree.
type MenuItem struct {
	ID      int32
	Label   string
	Type    string // "", "separator"
	Toggle  string // "", "checkmark"
	Checked bool
	Submenu []MenuItem
	Enabled bool
	OnClick func()
}

// SNI implements org.kde.StatusNotifierItem + com.canonical.dbusmenu
// for a single tray icon with a dynamic menu.
type SNI struct {
	conn     *dbus.Conn
	busName  string
	menuName string

	mu        sync.Mutex
	revision  uint32
	shownRev  uint32 // revision last reported via AboutToShow
	icon      []byte // ARGB32, 32x32
	tooltip   string
	title     string
	items     []MenuItem
	idToLabel map[int32]string

	props *prop.Properties
}

func NewSNI(pid int) (*SNI, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}
	s := &SNI{
		conn:      conn,
		busName:   fmt.Sprintf("%s%d-1", busNameBase, pid),
		menuName:  "traytail",
		idToLabel: map[int32]string{},
	}
	if err := conn.Export(s, sniPath, sniInterface); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Export(s, menuPath, menuInterface); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Export(s, menuPath, "org.freedesktop.DBus.Properties"); err != nil {
		conn.Close()
		return nil, err
	}

	// static-ish props; we emit PropertiesChanged manually on updates
	s.props, _ = prop.Export(conn, sniPath, map[string]map[string]*prop.Prop{
		sniInterface: {
			"Category":   makeProp("string", "Communications"),
			"Id":         makeProp("string", "traytail"),
			"Title":      makeProp("string", "traytail"),
			"Status":     makeProp("string", "Active"),
			"IconName":   makeProp("string", ""),
			"IconPixmap": makeProp("a(iiay)", []Pixmap{}),
			"ToolTip":    makeProp("(sa(iiay)ss)", []any{"", []Pixmap{}, "", ""}),
			"Menu":       makeProp("o", dbus.ObjectPath(menuPath)),
			"ItemIsMenu": makeProp("b", true),
		},
	})

	reply, err := conn.RequestName(s.busName, dbus.NameFlagDoNotQueue)
	if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		conn.Close()
		if err == nil {
			err = fmt.Errorf("name %s already owned", s.busName)
		}
		return nil, fmt.Errorf("request bus name %s: %v", s.busName, err)
	}
	return s, nil
}

// Close releases the bus name and disconnects.
func (s *SNI) Close() {
	if s.conn == nil {
		return
	}
	s.conn.Close()
	s.conn = nil
}

func makeProp(signature string, v any) *prop.Prop {
	return &prop.Prop{Value: v, Writable: false, Emit: prop.EmitTrue, Callback: nil}
}

// RegisterWithWatcher connects to org.kde.StatusNotifierWatcher.
// ashell implements both kde and freedesktop watcher names.
func (s *SNI) RegisterWithWatcher() bool {
	if s.conn == nil {
		return false
	}
	conn := s.conn
	for _, watcher := range []string{"org.kde.StatusNotifierWatcher", "org.freedesktop.StatusNotifierWatcher"} {
		obj := conn.Object(watcher, "/StatusNotifierWatcher")
		call := obj.Call("org.kde.StatusNotifierWatcher.RegisterStatusNotifierItem", 0, s.busName)
		if call.Err == nil {
			return true
		}
	}
	return false
}

// ---- StatusNotifierItem ----

func (s *SNI) Activate(x, y int32) *dbus.Error                    { return nil }
func (s *SNI) SecondaryActivate(x, y int32) *dbus.Error           { return nil }
func (s *SNI) Scroll(delta int32, orientation string) *dbus.Error { return nil }

func (s *SNI) emitSignal(name string, args ...any) {
	if s.conn == nil {
		return
	}
	s.conn.Emit(sniPath, sniInterface+"."+name, args...)
}

// ---- com.canonical.dbusmenu ----

type layoutNode struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant
}

func propsFor(it MenuItem) map[string]dbus.Variant {
	props := map[string]dbus.Variant{
		"visible": dbus.MakeVariant(true),
		"enabled": dbus.MakeVariant(it.Enabled),
	}
	switch {
	case it.Type == "separator":
		props["type"] = dbus.MakeVariant("separator")
	case it.Submenu != nil:
		props["children-display"] = dbus.MakeVariant("submenu")
		if it.Label != "" {
			props["label"] = dbus.MakeVariant(it.Label)
		}
	default:
		if it.Label != "" {
			props["label"] = dbus.MakeVariant(it.Label)
		}
		if it.Toggle != "" {
			props["toggle-type"] = dbus.MakeVariant(it.Toggle)
			state := int32(0)
			if it.Checked {
				state = 1
			}
			props["toggle-state"] = dbus.MakeVariant(state)
		}
	}
	return props
}

func nodeFromItem(it MenuItem) layoutNode {
	children := make([]dbus.Variant, 0, len(it.Submenu))
	for _, c := range it.Submenu {
		children = append(children, dbus.MakeVariant(nodeFromItem(c)))
	}
	return layoutNode{ID: it.ID, Props: propsFor(it), Children: children}
}

func (s *SNI) findItem(items []MenuItem, id int32) *MenuItem {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
		if sub := s.findItem(items[i].Submenu, id); sub != nil {
			return sub
		}
	}
	return nil
}

// GetLayout returns (revision, layout). ashell calls get_layout(0, -1, []).
func (s *SNI) GetLayout(parentId int32, recursionDepth int32, propertyNames []string) (uint32, layoutNode, *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root := layoutNode{ID: 0, Props: map[string]dbus.Variant{
		"children-display": dbus.MakeVariant("submenu"),
	}}
	for _, it := range s.items {
		root.Children = append(root.Children, dbus.MakeVariant(nodeFromItem(it)))
	}
	return s.revision, root, nil
}

// GetGroupProperties answers per-item property queries in one call;
// unknown ids are simply absent from the result.
func (s *SNI) GetGroupProperties(ids []int32, propertyNames []string) ([]struct {
	ID    int32
	Props map[string]dbus.Variant
}, *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]struct {
		ID    int32
		Props map[string]dbus.Variant
	}, 0, len(ids))
	for _, id := range ids {
		item := s.findItem(s.items, id)
		if item == nil {
			continue
		}
		out = append(out, struct {
			ID    int32
			Props map[string]dbus.Variant
		}{ID: id, Props: propsFor(*item)})
	}
	return out, nil
}

func (s *SNI) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.findItem(s.items, id)
	if item == nil {
		return dbus.MakeVariant(""), dbus.MakeFailedError(fmt.Errorf("no item %d", id))
	}
	switch name {
	case "type":
		if item.Type == "separator" {
			return dbus.MakeVariant("separator"), nil
		}
		return dbus.MakeVariant("standard"), nil
	case "label":
		return dbus.MakeVariant(item.Label), nil
	case "enabled":
		return dbus.MakeVariant(item.Enabled), nil
	case "visible":
		return dbus.MakeVariant(true), nil
	case "toggle-type":
		return dbus.MakeVariant(item.Toggle), nil
	case "toggle-state":
		state := int32(0)
		if item.Checked {
			state = 1
		}
		return dbus.MakeVariant(state), nil
	}
	return dbus.MakeVariant(""), nil
}

// Event handles clicks on menu items. The handler runs on its own
// goroutine so slow actions (tailscale CLI) never block the bus worker.
func (s *SNI) Event(id int32, eventId string, data dbus.Variant, timestamp uint32) *dbus.Error {
	s.mu.Lock()
	item := s.findItem(s.items, id)
	var onClick func()
	if item != nil {
		onClick = item.OnClick
	}
	s.mu.Unlock()
	if onClick != nil && eventId == "clicked" {
		go onClick()
	}
	return nil
}

func (s *SNI) EventGroup(ids []int32, eventId string, data dbus.Variant, timestamp uint32) *dbus.Error {
	return nil
}

// AboutToShow reports whether the host should re-fetch the layout:
// true only when an Update has landed since the last check. The flag
// is consumed on read so hosts don't loop on stale responses.
func (s *SNI) AboutToShow(id int32) (bool, *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirty := s.revision != s.shownRev
	s.shownRev = s.revision
	return dirty, nil
}

func (s *SNI) AboutToShowGroup(ids []int32) ([]int32, *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision != s.shownRev {
		s.shownRev = s.revision
		return ids, nil
	}
	return []int32{}, nil
}

// ---- org.freedesktop.DBus.Properties on menu path (some hosts probe) ----

func (s *SNI) Get(interfaceName, propertyName string) (dbus.Variant, *dbus.Error) {
	return dbus.MakeVariant(""), nil
}
func (s *SNI) GetAll(interfaceName string) (map[string]dbus.Variant, *dbus.Error) {
	return map[string]dbus.Variant{}, nil
}
func (s *SNI) Set(interfaceName, propertyName string, value dbus.Variant) *dbus.Error {
	return dbus.NewError("org.freedesktop.DBus.Error.PropertyNotFound", nil)
}

// ---- updates ----

// Update swaps icon, tooltip and menu atomically and notifies the host.
func (s *SNI) Update(icon []byte, tooltip string, items []MenuItem) {
	s.mu.Lock()
	s.icon = icon
	s.tooltip = tooltip
	s.items = items
	s.revision++
	rev := s.revision
	idToLabel := map[int32]string{}
	labelItems(items, idToLabel)
	s.idToLabel = idToLabel
	s.mu.Unlock()

	if s.conn == nil {
		return
	}
	// SNI property changes: ashell listens for icon-pixmap changes,
	// new_icon signals, and layout_updated on the menu interface.
	s.props.SetMust(sniInterface, "IconPixmap", []Pixmap{{Width: 32, Height: 32, Bytes: icon}})
	s.props.SetMust(sniInterface, "ToolTip", []any{"", []Pixmap{}, tooltip, ""})
	s.emitSignal("NewIcon")
	s.emitSignal("NewToolTip")
	s.conn.Emit(menuPath, menuInterface+".LayoutUpdated", rev, int32(0))
	log.Printf("traytail: updated (rev %d, %d items)", rev, len(items))
}

// labelItems flattens the item tree so GetLayout results can be
// cross-checked cheaply (and hosts can query labels later).
func labelItems(items []MenuItem, out map[int32]string) {
	for _, it := range items {
		out[it.ID] = it.Label
		labelItems(it.Submenu, out)
	}
}