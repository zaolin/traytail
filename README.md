# traytail

[![CI](https://github.com/zaolin/traytail/actions/workflows/ci.yml/badge.svg)](https://github.com/zaolin/traytail/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A minimal Tailscale tray icon for Wayland bars that host the
[StatusNotifierItem](https://www.freedesktop.org/wiki/Specifications/status-notifier-item/)
protocol — built and tested against [ashell](https://github.com/MalpenZibo/ashell)
(see [ashell](#ashell) for configuration notes).

No GUI toolkit. Pure Go, one dependency (`github.com/godbus/dbus/v5`), ~900 lines.

## Features

- Status icon: connected (filled) / offline (hollow) / exit node in use (green ring) / login required (amber)
- **Multiple profiles** — switch accounts (`tailscale switch`) from the Profile submenu
- **Exit nodes**, cleanly split:
  - *Own exit nodes* — regular tailnet peers with `ExitNodeOption`
  - *Mullvad VPN* — grouped by country (`tag:mullvad-exit-node-*`)
- Connect / Disconnect, copy Tailscale IP, open admin console
- Left-click on the bar icon opens the menu; updates are pushed via
  `NewIcon` / `LayoutUpdated` signals (5s poll, deduplicated)

## Requirements

- `tailscale` CLI in `$PATH`, `tailscaled` running
- You must be the Tailscale "operator" once:
  ```
  sudo tailscale set --operator=$USER
  ```

## Build & run

```
go build .
./traytail
```

## Menu

```
Haruhi — zaolin.github          (click = admin console)
Copy IP: 100.121.168.35
────────────
Profile: zaolin@github ▸        (if >1 account; parent shows active)
  ● zaolin@github               (active: click does nothing)
  ○ philipp@binarly.io          (click = switch, refreshes menu)
Exit node: Berlin ▸             (parent shows full city of the active node)
  ● Auto (smart: away→own, home→Mullvad)
  ○ Off
  ──
  ○ zds-nabara                  (own nodes, first DNS label)
  ──
  Mullvad ▸
     ● Germany ▸                (active country hoisted to top)
     Albania ▸  Argentina ▸ …   (full country names, sorted)
        ● Berlin                (city labels; ● marks the active one)
Disconnect
────────────
Admin console
Quit
```

**Exit node selection:** entries are labelled with full city/country
names parsed from `tailscale exit-node list` (hostname fallback when
that fails). The active country is hoisted to the top of the Mullvad
list, and every item carries a `●`/`○` radio marker — including an
explicit `Off` entry, so there is no click-active-to-disable trick.
State is shown as a text glyph instead of checkmark properties because
SNI hosts like ashell render `toggle-type: checkmark` items as switches.

**Smart auto** replaces tailscale's native `auto:any` with a policy
traytail manages itself: when your machine is *away* (no local
interface address inside the LAN routes advertised by your own exit
node) it routes through the own exit node; when *at home* it switches
to the last-used Mullvad node (persisted across restarts). The policy
re-applies only when the home/away state flips — manual selections and
Off stay untouched while the state is stable, and any manual pick or
Off disables smart auto.

**Deselecting the own exit node** (clicking its `●` item) applies the
last-used Mullvad node instead — regardless of whether smart auto is
on. Active nodes are matched by Tailscale IP, so a user-renamed device
(`Nabara` vs `zds-nabara.tailb4e47d.ts.net`) still shows as selected.

## Notes

- Exit node selection uses `tailscale set --exit-node=<hostname>`;
  labels and grouping come from `tailscale exit-node list`.
- Works with any SNI host (ashell, waybar, KDE Plasma), not just ashell.

## ashell

traytail is developed against [ashell](https://github.com/MalpenZibo/ashell)'s
tray module and works out of the box with its defaults — the module only
appears once a tray icon exists, so no configuration is needed. Notes for
ashell setups:

- **Menu opens on left click** by default (ashell's `right_click` is unset
  → left click opens the context menu). If you set `right_click = "Menu"`,
  left click switches to activating the app instead — traytail has no
  window, so keep the default or use `right_click = "Open"`.
- **ashell renders checkmark items as switches**, which is why traytail
  carries all selection state as `●`/`○` text glyphs on plain items —
  every menu entry renders as a normal button and closes the menu on click.
- Submenus render inline with `▸` toggles; the exit-node menu nests
  `Mullvad ▸ country ▸ city`, which ashell displays fine (indentation per level).
- traytail registers under the name `traytail`; add it to ashell's
  `[tray] blocklist` only if you *don't* want it shown.
- Other SNI hosts (waybar with SNI support, KDE Plasma) should work too,
  but are untested — ashell is the reference host.

## Development

```
make build    # go build
make test     # unit tests with -race
make cover    # unit + integration coverage (~93%)
make integration  # DBus integration tests under a session bus
```

## Install

```
sudo make install        # build + install to /usr/local/bin
sudo make uninstall      # remove it again
```

Custom location (no root needed):

```
make install PREFIX=$HOME/.local/bin
```

## License

[MIT](LICENSE)
