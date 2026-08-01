# Beacon

A lightweight, **self-updating DLNA / UPnP-AV media server** for low-power ARM
NAS hardware. Built for the **ASUSTOR AS1102TL** (Realtek RTD1619B, quad-core
ARM64, 1 GB RAM, ADM OS), but it runs anywhere Linux does.

Think MiniDLNA, but with a library that stays correct on its own, real metadata
and subtitle handling, thumbnails, and a built-in web dashboard — all in a
single ~12 MB static binary that sips RAM.

## Why

MiniDLNA's `inotify` misses events for some file types, only sees changes while
it's running, and has no periodic rescan — the usual fix is a cron job that
**destroys and rebuilds the whole database** every night. Beacon replaces that
with a three-tier update model that is **incremental and non-destructive**:

1. **Real-time** — an `inotify`/fsnotify watcher indexes adds/changes/removals
   the moment they happen, with write-settle debouncing so half-copied files are
   never served.
2. **Reconcile** — a periodic delta scan (default every 15 min) catches anything
   the watcher missed (events lost while stopped, watch-limit overflow, etc.).
3. **Integrity** — a slower full pass (default daily) as a backstop.

Existing entries keep their identity across all of this; nothing is ever wiped
and rebuilt.

## Features

- **Standards-compliant UPnP-AV MediaServer** — SSDP discovery, ContentDirectory
  browsing, ConnectionManager, and HTTP streaming with byte-range seeking.
- **Self-updating library** — the three-tier engine above.
- **Metadata** — duration and resolution via `ffprobe`, shown to clients.
- **Subtitles** — sidecar `.srt`/`.ass`/`.vtt`/… detection (incl. `Movie.en.srt`),
  advertised three ways for broad smart-TV compatibility and served over HTTP.
- **Thumbnails & artwork** — lazy, disk-cached video thumbnails via `ffmpeg`,
  plus `poster.jpg`/`folder.jpg`/same-name image detection.
- **Web admin dashboard** — status, live folder management, manual rescan, and a
  recent-activity log, on the same port as the media server.
- **Tiny footprint** — one static, cgo-free binary; pure-Go SQLite; bounded
  worker pools tuned for 1 GB RAM. Uses the NAS's system `ffmpeg`/`ffprobe`.

## Requirements

- A Linux host (the target NAS). `ffmpeg` and `ffprobe` on `PATH` enable
  metadata and thumbnails; without them, browsing/streaming/subtitles still work.
- To build: **Go 1.25+** (see the `go` directive in `go.mod`).

IPv4 only — there is no IPv6 support.

## Install as an ADM app (ASUSTOR App Central)

Build the package (produces `dist/Beacon_<version>_arm64.apk` — ASUSTOR calls
these "APK files"; the format is APKG 2.0):

```powershell
./scripts/package.ps1
```

Or, on any platform (Linux, macOS, or Git Bash on Windows):

```bash
./scripts/build.sh
```

Both read the version from the `VERSION` file.

On the NAS:

1. **App Central → Manual Install** (top-right tab).
2. **Browse** to the `.apk` → **Upload** → Install (accept the unverified-app
   warning).
3. A **Beacon** icon appears on the ADM desktop and opens the dashboard.

The package installs to `/usr/local/AppCentral/Beacon/`, seeds a default config
on first launch, and starts automatically on boot.

> Packaging notes: `config.json` `firmware` must be two-component `major.minor`
> (e.g. `4.0`), and the architecture is `arm64`, matching your installed apps.
> The package is assembled by `cmd/mkapkg` using the Go standard library — no
> ASUSTOR `apkg-tools` required.

## Run directly (development / non-ADM hosts)

Cross-compile and copy over SSH:

```powershell
./scripts/deploy.ps1 -NasHost <ip> -NasUser <user> -Run
```

This builds a `linux/arm64` binary, installs a small control script
(`beaconctl.sh` — `start|stop|restart|status`), seeds a config, and starts it.
Or run the binary yourself:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o beacon ./cmd/beacon
./beacon -config beacon.toml
```

## Configuration

Copy `beacon.example.toml` to `beacon.toml` and edit. Every field is optional —
omitted fields use built-in defaults, and a stable server UUID is generated on
first run. Key settings: watched folders, HTTP port (default `8322`; `0`
auto-picks a free one), worker count, scan intervals, and `ffprobe`/`ffmpeg`
paths (auto-detected by default). See the example file for everything.

## Web dashboard

Browse to `http://<nas-ip>:8322/`:

- library size and engine health (watcher / ffprobe / ffmpeg badges),
- add or remove watched folders live (no restart),
- trigger a rescan,
- view recent log lines.

It shares the media-server port and has **no authentication** — intended for a
trusted LAN. Two limits keep that from being a blank cheque:

- Write endpoints refuse cross-origin requests and require `application/json`,
  so a web page you happen to visit cannot drive the dashboard behind your back.
- Folders can only be added under `library.allowed_parents` (defaulting to the
  parents of the folders you already configured), so nobody can point Beacon at
  `/` and stream the whole NAS.

Anyone on the LAN can still *read* status and recent log lines.

## Troubleshooting

**A TV can't find the server.** Discovery is SSDP on UDP/1900:

- Something else may hold port 1900 (the NAS's own media server). Beacon logs
  this and keeps serving over HTTP — only auto-discovery is lost. Stop the other
  server, or point the client at `http://<nas-ip>:8322/rootDesc.xml` directly.
- Beacon joins the multicast group on *every* up, multicast-capable interface and
  replies with the address of the one the query arrived on, so link aggregation,
  VLANs and Docker bridges are handled. Set `[server] interface` to pin it to one.
- After a DHCP address change Beacon re-announces with a new `BOOTID`, which tells
  clients to discard the address they cached. Some TVs still need their media-source
  list refreshed.

**The port conflicts.** `http_port = 0` picks any free port.

**Where are the logs?** The ADM package writes `beacon.log` next to the binary in
`/usr/local/AppCentral/Beacon/`. It is truncated at each start and is not rotated,
so avoid leaving `log.level = "debug"` on permanently.

**Durations and resolutions are blank.** `ffprobe` was not found at index time.
Install it and restart — items whose probe failed are automatically re-queued.

## Building & testing

```sh
go build ./...     # build everything (host)
go test ./...      # run the test suite
go vet ./...
```

The module is named `beacon`; if you publish it, you can rename the module path
in `go.mod`.

## Project layout

```
cmd/
  beacon/        server entrypoint
  mkapkg/        assembles the ASUSTOR .apk (Go stdlib; no apkg-tools)
  mkicon/        generates the app icon
internal/
  config/        TOML config + validation
  logging/       structured logging + in-memory ring buffer for the dashboard
  netutil/       LAN IP detection
  store/         pure-Go SQLite persistence (modernc.org/sqlite)
  content/       content model + object-ID scheme + media-type tables
  library/       indexer, DB-backed browse backend, watcher, metadata enricher
  meta/          ffprobe wrapper + subtitle detection
  thumbs/        ffmpeg thumbnails + poster detection (lazy, cached)
  upnp/          SSDP, SOAP, DIDL-Lite, ContentDirectory, ConnectionManager, HTTP
  ssdp/          SSDP advertiser/responder
  admin/         embedded web dashboard + JSON API
  server/        wires it all together
packaging/apkg/  ASUSTOR package control files (config.json, start-stop.sh, …)
scripts/         deploy.ps1, package.ps1, beaconctl.sh
```

## How it works (brief)

Media files are indexed into a single SQLite table keyed by absolute path
(object IDs are the base64url of the path). The UPnP `ContentDirectory` serves
browse requests straight from that index, so listings are fast and the
auto-update engine and dashboard can share one source of truth. Metadata and
thumbnails are produced by bounded background worker pools and cached, so the
NAS is never overwhelmed. See the package doc comments for details.

## Roadmap (optional)

- GENA push eventing so clients refresh a folder on their own, rather than on
  re-navigation. `SystemUpdateID` is maintained correctly today, but there is no
  subscription mechanism to tell anyone about it.
- ContentDirectory `Search`, for Samsung's "Recently added" and Kodi/BubbleUPnP
  search boxes.
- Per-client (Samsung/LG/Sony) quirk profiles.
- Optional dashboard authentication.
- IPv6.

## License

Licensed under the Apache License, Version 2.0 — see [`LICENSE`](LICENSE).

Copyright 2026 cicalooo
