# traytail

[![CI](https://github.com/zaolin/traytail/actions/workflows/ci.yml/badge.svg)](https://github.com/zaolin/traytail/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A minimal Tailscale tray icon for Wayland bars that host the
[StatusNotifierItem](https://www.freedesktop.org/wiki/Specifications/status-notifier-item/)
protocol — built and tested against [ashell](https://github.com/MalpenZibo/ashell).

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
  ● Off                         (explicit off switch; active when no exit node)
  ○ Auto (best)                 (tailscale auto:any)
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

## Notes

- Exit node selection uses `tailscale set --exit-node=<hostname>`;
  labels and grouping come from `tailscale exit-node list`.
- Works with any SNI host (ashell, waybar, KDE Plasma), not just ashell.

## Development

```
make build    # go build
make test     # unit tests with -race
make cover    # unit + integration coverage (~93%)
make integration  # DBus integration tests under a session bus
```

## License

[MIT](LICENSE)
