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
Haruhi — binarly.io             (click = admin console)
Copy IP: 100.65.70.92
────────────
Profile: binarly.io ▸           (if >1 account; parent shows active)
  ✓ philipp@binarly.io          (active: click does nothing)
  ─ zaolin@github               (click = switch, refreshes menu)
Exit node: fra ▸                (parent shows current selection)
  ✓ Auto (best)                 (tailscale auto:any; active = click off)
  ──
  homeserver                    (own nodes direct, radio behavior)
  ──
  Mullvad ▸                     (grouped by country: DE ▸, NL ▸ …)
Disconnect
────────────
Admin console
Quit
```

**Exit nodes behave like radio buttons:** only one can be active at a
time; clicking the active entry turns exit routing off — there is no
separate "None" entry. `Auto (best)` uses Tailscale's `auto:any` (best
available node); once connected, the parent label shows the resolved
city (e.g. `Exit node: ams`).

## Notes

- Exit node selection uses `tailscale set --exit-node=<base-name>`
  (works identically for own nodes and Mullvad; empty = off).
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
