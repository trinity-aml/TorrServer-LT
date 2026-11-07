# TorrServer-LT

> Fork of [YouROK/TorrServer](https://github.com/YouROK/TorrServer) with the BitTorrent core replaced by [libtorrent (arvidn)](https://www.libtorrent.org/).
>
> **Status:** feature parity with upstream **MatriX.150** (including GStreamer HLS transcoding, WAF and MCP) on the libtorrent engine — streaming, preload, and seeking (including back into already-evicted regions) are verified on real torrents. The HTTP API, on-disk databases (`config.db`, JSON, `accs.db`, viewed) and the `torrs://` token format remain compatible with upstream. The cache layout under `TorrentsSavePath/<hash>/<pieceID>` is preserved.
>
> **Platforms:**
> - Linux: `amd64`, `arm64`, `armv7`
> - Windows: `amd64` (MSYS2 / MinGW64 runtime)
> - macOS: `amd64` (Intel), `arm64` (Apple Silicon)
> - Android: `arm64-v8a`, `armeabi-v7a` (Termux or similar shell)
>
> Each platform's binary statically (or near-statically) links libtorrent + Boost; see the per-platform CI workflow under `.github/workflows/` for the exact toolchain.

## Introduction

TorrServer-LT is a program that allows users to view torrents online without the need for preliminary file downloading.
The core functionality includes caching torrents and subsequent data transfer via the HTTP protocol,
allowing the cache size to be adjusted according to the system parameters and the user's internet connection speed.

The difference from upstream is the underlying torrent engine: `arvidn/libtorrent` (C++) is wired in via a thin CGo shim, replacing `anacrolix/torrent` (Go). This is expected to improve peer-protocol behaviour, throughput in real-world conditions, and bring in features absent from the Go-native engine.

## AI Documentation

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/YouROK/TorrServer)

## Features

- Caching
- Streaming
- Local and Remote Server
- Viewing torrents on various devices
- Integration with other apps through API
- Torznab search (Jackett, Prowlarr, and similar indexer managers)
- Cross-browser modern web interface
- Optional DLNA server (with category folders)
- Optional GStreamer HLS transcoding (`-gst` builds; audio AC3/EAC3/DTS → AAC for players without those decoders)
- Native [MCP](server/mcp/README.md) server for AI agents (OpenClaw, Hermes, and other MCP clients)
- HTTP access WAF (IP white/blacklist, Referer/Origin blocking)

## Getting Started

### Installation

Download the application for the required platform in the [releases](https://github.com/trinity-aml/TorrServer-LT/releases) page. After installation, open the link <http://127.0.0.1:8090> in the browser.

Every release ships two flavours per desktop platform:

- `TorrServer-LT-<platform>` — the base build;
- `TorrServer-LT-<platform>-gst` — the same build with **GStreamer HLS
  transcoding** compiled in (linux amd64/arm64, windows amd64, macOS
  amd64/arm64). It loads the system GStreamer libraries dynamically at runtime
  — install [GStreamer](https://gstreamer.freedesktop.org/download/) **1.22+**
  (with the base/good/bad plugin sets) to actually use it; without GStreamer
  the binary still works, the transcoding tab just reports unavailable. The
  base build has the feature stubbed out entirely.

#### Windows

Run `TorrServer-LT-windows-amd64.exe` (or `TorrServer-LT-windows-amd64-gst.exe` for the transcoding variant).

#### Linux

Run in console

```bash
curl -s https://raw.githubusercontent.com/trinity-aml/TorrServer-LT/master/installTorrServerLinux.sh | sudo bash
```

The script supports interactive and non-interactive installation, configuration, updates, and removal. When running the script interactively, you can:

- **Install/Update**: Choose to install or update TorrServer
- **Reconfigure**: If TorrServer is already installed, you'll be prompted to reconfigure settings (port, auth, read-only mode, logging, BBR)
- **Uninstall**: Type `Delete` (or `Удалить` in Russian) to uninstall TorrServer

**Download first and set execute permissions:**

```bash
curl -s https://raw.githubusercontent.com/trinity-aml/TorrServer-LT/master/installTorrServerLinux.sh -o installTorrServerLinux.sh && chmod 755 installTorrServerLinux.sh
```

**Command-line examples:**

- Install a specific version:

  ```bash
  sudo bash ./installTorrServerLinux.sh --install MatriX.150.LT-1.2.3 --silent
  ```

- Update to latest version:

  ```bash
  sudo bash ./installTorrServerLinux.sh --update --silent
  ```

- Reconfigure settings interactively:

  ```bash
  sudo bash ./installTorrServerLinux.sh --reconfigure
  ```

- Check for updates:

  ```bash
  sudo bash ./installTorrServerLinux.sh --check
  ```

- Downgrade to a specific version:

  ```bash
  sudo bash ./installTorrServerLinux.sh --down MatriX.LT-001
  ```

- Remove/uninstall:

  ```bash
  sudo bash ./installTorrServerLinux.sh --remove --silent
  ```

- Change the systemd service user:

  ```bash
  sudo bash ./installTorrServerLinux.sh --change-user root --silent
  ```

- Install the GStreamer transcoding (`-gst`) variant:

  ```bash
  sudo bash ./installTorrServerLinux.sh --install --gst --silent
  ```

**All available commands:**

- `--install [VERSION]` - Install latest or specific version
- `--update` - Update to latest version
- `--reconfigure` - Reconfigure TorrServer settings (port, auth, read-only mode, logging, BBR)
- `--check` - Check for updates (version info only)
- `--down VERSION` - Downgrade to specific version
- `--remove` - Uninstall TorrServer
- `--change-user USER` - Change service user (root|torrserver)
- `--root` - Run service as root user
- `--silent` - Non-interactive mode with defaults
- `--gst` - Install the `-gst` release variant (GStreamer HLS transcoding; amd64/arm64 only, needs a system GStreamer ≥ 1.22 at runtime). Updates keep the installed variant automatically.
- `--no-gst` - Switch an existing `-gst` install back to the base variant
- `--help` - Show help message

#### macOS

Run in Terminal.app

```bash
curl -s https://raw.githubusercontent.com/trinity-aml/TorrServer-LT/master/installTorrServerMac.sh -o installTorrserverMac.sh && chmod 755 installTorrServerMac.sh && bash ./installTorrServerMac.sh
```

Alternative install script for Intel Macs: <https://github.com/dancheskus/TorrServerMacInstaller>

#### IOCage Plugin (Unofficial)

On FreeBSD (TrueNAS/FreeNAS) you can use this plugin: <https://github.com/filka96/iocage-plugin-TorrServer>

#### NAS Systems (Unofficial)

- Several releases are available through this link: <https://github.com/vladlenas>
- **Synology NAS** packages repo source: <https://grigi.lt>

### Server args

- `--port PORT`, `-p PORT` - web server port (default 8090)
- `--ip IP`, `-i IP` - web server bind addr (repeatable; default empty binds to all interfaces)
- `--ssl` - enables https for web server
- `--sslport PORT` -  web server https port (default 8091). If not set, will be taken from db (if stored previously) or the default will be used.
- `--sslcert PATH` -  path to ssl cert file. If not set, will be taken from db (if stored previously) or default self-signed certificate/key will be generated.
- `--sslkey PATH` - path to ssl key file. If not set, will be taken from db (if stored previously) or default self-signed certificate/key will be generated.
- `--force-https` - with `--ssl`, the HTTP port (`--port`) answers with **307 Temporary Redirect** to the same path on HTTPS (`--sslport`). Requires `--ssl`. See [HTTPS](#https).
- `--http-media` - with `--force-https`, keeps media URLs (`/stream`, `/play`, playlists, GStreamer HLS) on plain HTTP for players and TVs that reject the certificate. Stream URLs and Basic auth credentials then travel unencrypted.
- `--https-only` - with `--ssl`, doesn't open the plain HTTP port at all. Players and TVs then need a trusted certificate.
- `--path PATH`, `-d PATH` - database and config dir path
- `--logpath LOGPATH`, `-l LOGPATH` - server log file path
- `--weblogpath WEBLOGPATH`, `-w WEBLOGPATH` - web access log file path
- `--rdb`, `-r` - start in read-only DB mode
- `--httpauth`, `-a` - enable http auth on all requests
- `--dontkill`, `-k` - don't kill server on signal
- `--ui`, `-u` - open torrserver page in browser
- `--torrentsdir TORRENTSDIR`, `-t TORRENTSDIR` - autoload torrents from dir
- `--torrentaddr TORRENTADDR` - Torrent client address (format [IP]:PORT, ex. :32000, 127.0.0.1:32768 etc)
- `--pubipv4 PUBIPV4`, `-4 PUBIPV4` - set public IPv4 addr
- `--pubipv6 PUBIPV6`, `-6 PUBIPV6` - set public IPv6 addr
- `--searchwa`, `-s` - allow search without authentication
- `--maxsize MAXSIZE`, `-m MAXSIZE` - max allowed stream size (in Bytes)
- `--tg TGTOKEN`, `-T TGTOKEN` - [Telegram bot](server/tgbot/README.md) token
- `--fuse FUSEPATH`, `-f FUSEPATH` - fuse mount path
- `--webdav` - enable web dav
- `--proxyurl PROXYURL` - set proxy URL for BitTorrent traffic (http, socks4, socks5, socks5h), example: socks5h://user:password@example.com:2080
- `--proxymode PROXYMODE` - set proxy mode: "tracker" (only HTTP trackers, default), "peers" (only peer connections), or "full" (all traffic)
- `--help`, `-h` - display this help and exit
- `--version` - display version and exit

Example:

```bash
TorrServer-darwin-arm64 [--port PORT] [--ip IP ...] [--path PATH] [--logpath LOGPATH] [--weblogpath WEBLOGPATH] [--rdb] [--httpauth] [--dontkill] [--ui] [--torrentsdir TORRENTSDIR] [--torrentaddr TORRENTADDR] [--pubipv4 PUBIPV4] [--pubipv6 PUBIPV6] [--searchwa] [--maxsize MAXSIZE] [--tg TGTOKEN] [--fuse FUSEPATH] [--webdav] [--ssl] [--sslport PORT] [--sslcert PATH] [--sslkey PATH] [--force-https] [--http-media] [--https-only]
```

### Running in Docker & Docker Compose

Run in console

```bash
docker run --rm -d --name torrserver -p 8090:8090 ghcr.io/trinity-aml/torrserver-lt:latest
```

For running in persistence mode, just mount volume to container by adding `-v ~/ts:/opt/ts`, where `~/ts` folder path is just example, but you could use it anyway... Result example command:

```bash
docker run --rm -d --name torrserver -v ~/ts:/opt/ts -p 8090:8090 ghcr.io/trinity-aml/torrserver-lt:latest
```

#### Environments

- `TS_HTTPAUTH` - 1, and place auth file into `~/ts/config` folder for enabling basic auth (also protects the MCP endpoint `/mcp`)
- `TS_RDB` - if 1, then the enabling `--rdb` flag
- `TS_DONTKILL` - if 1, then the enabling `--dontkill` flag
- `TS_PORT` - for changind default port to **5555** (example), also u need to change `-p 8090:8090` to `-p 5555:5555` (example)
- `TS_CONF_PATH` - for overriding torrserver config path inside container. Example `/opt/tsss`
- `TS_TORR_DIR` - for overriding torrents directory. Example `/opt/torr_files`
- `TS_LOG_PATH` - for overriding log path. Example `/opt/torrserver.log`
- `TS_PROXYURL` - set proxy URL for BitTorrent traffic (http, socks4, socks5, socks5h), example: socks5h://user:password@example.com:2080
- `TS_PROXYMODE` - set proxy mode: "tracker" (only HTTP trackers, default), "peers" (only peer connections), or "full" (all traffic)
- `TS_IP` - web server bind address (`--ip`)
- `TS_TORR_ADDR` - torrent client address (`--torrentaddr`)
- `TS_WEB_LOG_PATH` - web access log path (`--weblogpath`)
- `TS_SSL_ENABLE` - if 1, enables HTTPS (`--ssl`); the old name `TS_EN_SSL` still works
- `TS_SSL_PORT` - HTTPS port (`--sslport`)
- `TS_SSL_CERT_PATH` / `TS_SSL_KEY_PATH` - SSL certificate and key files (`--sslcert` / `--sslkey`)
- `TS_FORCE_HTTPS_ENABLE` - if 1, redirects HTTP to HTTPS (`--force-https`)
- `TS_HTTP_MEDIA_ENABLE` - with `TS_FORCE_HTTPS_ENABLE`, keeps media URLs on plain HTTP (`--http-media`)
- `TS_HTTPS_ONLY_ENABLE` - if 1, serves HTTPS only and does not open the HTTP port (`--https-only`)
- `TS_SEARCH_WA_ENABLE` - if 1, search without auth (`--searchwa`)
- `TS_STREAM_WA_ENABLE` - if 1, stream/play and M3U without auth (`--streamwa`)
- `TS_WEBDAV_ENABLE` - if 1, enables WebDAV (`--webdav`)
- `TS_PUBLIC_IPV4_ADDR` / `TS_PUBLIC_IPV6_ADDR` - public IP addresses (`--pubipv4` / `--pubipv6`)
- `TS_MAX_SIZE` - max allowed stream size in bytes (`--maxsize`)
- `TS_TELEGRAM_TOKEN` - Telegram bot token (`--tgtoken`)
- `TS_FUSE_PATH` - FUSE mount path (`--fusepath`); the container needs `/dev/fuse` and `SYS_ADMIN`

Example with full overrided command (on default values):

```bash
docker run --rm -d -e TS_PORT=5665 -e TS_DONTKILL=1 -e TS_HTTPAUTH=1 -e TS_RDB=1 -e TS_CONF_PATH=/opt/ts/config -e TS_LOG_PATH=/opt/ts/log -e TS_TORR_DIR=/opt/ts/torrents -e TS_PROXYURL=socks5h://user:password@example.com:2080 -e TS_PROXYMODE=tracker --name torrserver -v ~/ts:/opt/ts -p 5665:5665 ghcr.io/trinity-aml/torrserver-lt:latest
```

#### Docker Compose

```yml
# docker-compose.yml

version: '3.3'
services:
    torrserver:
        image: ghcr.io/trinity-aml/torrserver-lt
        container_name: torrserver
        network_mode: host    # to allow DLNA feature
        environment:
            - TS_PORT=5665
            - TS_DONTKILL=1
            - TS_HTTPAUTH=0
            - TS_CONF_PATH=/opt/ts/config
            - TS_TORR_DIR=/opt/ts/torrents
        volumes:
            - './CACHE:/opt/ts/torrents'
            - './CONFIG:/opt/ts/config'
        ports:
            - '5665:5665'
        restart: unless-stopped
        

```

### Smart TV (using Media Station X)

1. Install **Media Station X** on your Smart TV (see [platform support](https://msx.benzac.de/info/?tab=PlatformSupport))

2. Open it and go to: **Settings -> Start Parameter -> Setup**

3. Enter current ip and port of the TorrServe(r), e.g. `127.0.0.1:8090`

## Settings

Most behaviour is configured in the web UI (**Settings**), stored in the config
DB and applied live — saving reconnects the torrent session, so changes take
effect without a restart. The cache and streaming options that matter most for
this libtorrent fork:

| Setting | Default | What it does |
|---------|---------|--------------|
| **Cache size** (`CacheSize`) | 64 MB | Memory (or disk) budget for the piece cache. The streaming cache keeps the reader's forward window + recently-played pieces and evicts the rest; an evicted piece is un-`have`d in libtorrent so a later seek back into it re-downloads instead of stalling. |
| **Readahead cache** (`ReaderReadAHead`) | 95% | Forward streaming window as a percentage of the cache — how far ahead of the play head pieces are prioritised (graded so the play head is fetched first). |
| **Preload before play** (`PreloadCache`) | 50% | Buffer this fraction of the cache at the file head before playback starts (e.g. 64 MB × 50% = 32 MB). |
| **Pad short tail piece** (`PadTailPartial`) | off | **New.** When the file's last piece is a short partial (smaller than a full piece *and* under 5 MB) the cache stops short of its size; pin one extra tail piece to fill it (may then run up to one piece over the configured size). |
| **End-game mode** (`DisableEndGame`) | on | **New.** Request the final buffer pieces from all peers at once for a faster finish/seek; turn off to cut duplicate traffic. |
| **Disk cache** (`UseDisk` + `TorrentsSavePath`) | off | Store pieces on disk under `TorrentsSavePath/<hash>/<pieceID>` instead of RAM. |
| **Remove cache on drop** (`RemoveCacheOnDrop`) | off | Delete the on-disk cache when a torrent is removed. |
| **Upload** (`DisableUpload`) | on | Turn off to run leech-only (never unchoke peers — no seeding). |

The EOF seek index (MP4 `moov` / MKV cues / AVI `idx1`) at the file tail is
buffered **automatically** — one whole piece when pieces exceed 5 MB, otherwise
5 MB — so the player can read its index for instant seek; no setting is needed
(`PadTailPartial` only tops up the cache when that tail piece is a short partial).
`PadTailPartial` lives on the **Main** tab and the end-game toggle on the
**Additional** tab (Additional requires PRO mode). The Additional tab also covers
connection/rate limits (including **Max DHT connections**, `DHTConnectionsLimit` →
libtorrent `dht_max_peers`, default 500), DHT, **PEX** (peer exchange — disabling
it now actually drops the `ut_pex` plugin), LSD/UPnP, encryption, DLNA, HTTPS,
proxy and Torznab search.

A `-gst` build adds a **GStreamer** settings tab (PRO mode; shown only when the
feature is compiled in) with the transcoding options: which codecs to transcode
(H264/H265/AV1/VP9/VP8 video, AAC bitrate/channels for audio), segment length,
parallel task limit and the GStreamer path/version override. Playback goes
through `/gst/{hash}/master.m3u8` (HLS); MKV/WebM containers (and AVI when
`TranscodeAVI` is on) are supported, audio is transcoded to AAC — for players
that can't decode AC3/EAC3/DTS. It can also expose embedded text subtitles as
WebVTT, tone-map HDR to SDR, and use a hardware H.264 encoder when available.
Full detail — runtime installation per OS, configuration fields and the API —
in the [GStreamer](#gstreamer) section below.

## Development

This fork links **libtorrent 2.1.0 (arvidn)** into the Go server through a CGo
shim (`server/lt`). Unlike upstream's pure-Go engine, every build is therefore
**CGo + C++** and needs a libtorrent/Boost toolchain. One shim feature — the
per-piece `we_dont_have` the streaming cache uses to re-download evicted regions
on seek-back — calls libtorrent internals that a **shared** `libtorrent-rasterbar`
(distro / Homebrew) doesn't export. It's compiled in only when the build defines
`TSL_HAVE_LT_INTERNALS`, which the `build/*.sh` scripts do because they link the
**static** libtorrent they build from source. A plain `go build` against a system
`libtorrent-rasterbar-dev` still links (the feature falls back to a no-op), so it
runs for development; seek-back into an already-evicted region just won't
re-download. For the full feature set, build via the scripts below (or pass
`CGO_CXXFLAGS=-DTSL_HAVE_LT_INTERNALS` when linking a static libtorrent yourself).

### Prerequisites

- **Go 1.25+**
- A host C/C++ toolchain (`gcc`/`g++`), `curl`, `git` — to bootstrap Boost's
  `b2` and build libtorrent. No cmake, Docker or QEMU required.
- For the web UI: **Node.js 18+** and **yarn**.

### Local server (linux-amd64)

Build the libtorrent + Boost deps once (cached in `_deps/linux-amd64/`); this
also drops a ready binary in `_out/`:

```bash
build/linux-amd64.sh
```

To iterate on the Go code with `go run` / `go test`, point pkg-config at that
static tree so cgo links the right libtorrent:

```bash
cd server
export PKG_CONFIG_PATH=$PWD/../_deps/linux-amd64/lib/pkgconfig
export PKG_CONFIG_LIBDIR=$PWD/../_deps/linux-amd64/lib/pkgconfig
export CGO_LDFLAGS="-L$PWD/../_deps/linux-amd64/lib"
go run ./cmd        # or: go test ./...
```

Then open <http://127.0.0.1:8090>.

### Cross-compilation (no Docker)

`build/` cross-builds **every** supported target on a Linux host: it builds
libtorrent and `boost_system` from source with Boost.Build (`b2`) into
`_deps/<target>/`, then links the Go binary against it via pkg-config. Output
lands in `_out/TorrServer-LT-<target>`; the GStreamer-capable platforms (linux
amd64/arm64, windows amd64, macOS) also get `_out/TorrServer-LT-<target>-gst`
built with `-tags gst` — the variant is pure Go (GStreamer is dlopen'd at
runtime via purego), so it needs no extra toolchain and reuses the build cache.

```bash
build/all.sh                        # everything the host can build
TARGETS="linux-arm64" build/all.sh  # a subset
```

`all.sh` reports each target `OK` / `FAIL` / `SKIP` (toolchain missing).
Targets and the cross-toolchain each needs:

| Target          | Toolchain to install                                       |
|-----------------|------------------------------------------------------------|
| `linux-amd64`   | host gcc/g++ (nothing extra)                               |
| `linux-arm64`   | `gcc-aarch64-linux-gnu g++-aarch64-linux-gnu`              |
| `linux-armv7`   | `gcc-arm-linux-gnueabihf g++-arm-linux-gnueabihf`          |
| `windows-amd64` | `gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64`                |
| `android-arm64` / `android-armv7` | Android NDK r26+ (`export ANDROID_NDK_HOME=…`) |
| `darwin-arm64` / `darwin-amd64`   | OSXCross + Apple macOS SDK (see below)     |

libtorrent is built with `crypto=openssl` and `webtorrent=on`: each target gets
a static OpenSSL built from source (no system OpenSSL needed) plus the WebRTC
deps (libdatachannel/usrsctp/libjuice), enabling https trackers/web seeds and
WebTorrent (`wss://` trackers, browser peers). `cmake` is required on the build
host for the WebRTC deps. Everything links statically — the only dynamic deps
in the final binary are libc/libstdc++/libgcc (Windows links those static
too). Versions are pinned in `build/_common.sh` (Boost 1.85.0, libtorrent
v2.1.0, OpenSSL 3.5.7) and overridable, e.g.
`LIBTORRENT_TAG=v2.0.13 build/linux-arm64.sh`. Full detail and the per-target
prerequisites table: [`build/README.md`](build/README.md).

### macOS

Three ways to produce macOS binaries (`darwin-amd64` Intel, `darwin-arm64`
Apple Silicon):

1. **On a real Mac** — install Go and the Xcode command-line tools, then run
   `build/darwin-native.sh arm64` (or `amd64`). It builds the pinned libtorrent
   from source via b2 — nothing is taken from Homebrew, whose libtorrent
   version floats — then both the base and `-gst` binaries. This is the
   supported path for anything you distribute.
2. **CI** — `.github/workflows/build-macos.yml` runs that same script for both
   arches on an Apple-Silicon runner (amd64 via Rosetta/`-arch x86_64`).
3. **Cross-build from Linux via OSXCross** — needs the Apple macOS SDK, which
   Apple's licence only permits on Apple hardware, so this is a legal grey area
   and is meant for local reproducibility only. Set `OSXCROSS_ROOT` and run the
   `darwin-arm64.sh` / `darwin-amd64.sh` scripts; the one-time SDK-extraction
   recipe is in [`build/README.md`](build/README.md).

### Web UI

The React app under `web/` is compiled and **embedded** into the Go binary
(`server/web/pages/template`), so a UI change is only visible after rebuilding
the bundle *and* the server:

```bash
cd web
yarn install
NODE_OPTIONS=--openssl-legacy-provider CI=false yarn build   # CRA needs legacy OpenSSL on Node 18+

cd ..
go run gen_web.go     # copies web/build → server tree, regenerates the //go:embed table + routes
```

`gen_web.go` runs `yarn build` for you when `web/build` is missing. The embed
table is keyed on CRA's hashed chunk filenames, so you **must** regenerate it
after a rebuild — copying `web/build` by hand is not enough. For live UI work
without rebuilding the binary, `cd web && yarn start` proxies to a running
server. More info: [`web/README.md`](web/README.md).

### Swagger

`swag` must be installed to (re)build the API docs:

```bash
go install github.com/swaggo/swag/cmd/swag@latest
cd server && swag init -g web/server.go
swag fmt   # lint/format the annotations
```

## API

### API Docs

API documentation is hosted as Swagger format available at path `/swagger/index.html`.

### MCP (AI agents)

TorrServer exposes a native [Model Context Protocol](https://modelcontextprotocol.io/) server at **`/mcp`** on the same HTTP(S) port as the web UI (default `8090`). OpenClaw, Hermes, and other MCP clients can list, add, and manage torrents, and get a play URL for the next unwatched TV episode. The REST API is unchanged.

Endpoint: `http://<host>:8090/mcp` (or `https://` when `--ssl` is enabled).

When HTTP auth is on (`-a` / `TS_HTTPAUTH=1`), MCP uses the same Basic credentials as the rest of the API (`accs.db`). Play links returned by tools are ordinary HTTP URLs for VLC, mpv, or a browser.

**OpenClaw** (`openclaw.json`):

```json
{
  "mcp": {
    "servers": {
      "torrserver": {
        "url": "http://127.0.0.1:8090/mcp",
        "transport": "streamable-http"
      }
    }
  }
}
```

With auth, add `"headers": { "Authorization": "Basic <base64-user-pass>" }`.

**Hermes** (`~/.hermes/config.yaml`):

```yaml
mcp_servers:
  torrserver:
    url: "http://127.0.0.1:8090/mcp"
    headers:
      Authorization: "Basic <base64-user-pass>"
```

See [server/mcp/README.md](server/mcp/README.md) for the tool list and next-unwatched behavior.

## Authentication

The users data file should be located near to the settings. Basic auth, read more in wiki <https://en.wikipedia.org/wiki/Basic_access_authentication>.

`accs.db` in JSON format:

```json
{
    "User1": "Pass1",
    "User2": "Pass2"
}
```

Note: You should enable authentication with -a (--httpauth) TorrServer startup option.

## Retrackers

When adding a torrent, TorrServer can modify its announce trackers according to **Settings → Additional → Retrackers**:

| Mode | Behavior |
|------|----------|
| Don't add | Leave magnet/file trackers unchanged |
| Add (default) | Append the default/remote list |
| Remove | Clear trackers from the torrent |
| Replace | Replace them with the default/remote list |

Related settings (same Web UI section, also via `POST /settings`):

- **`TrackersListURL`** — optional custom remote list URL. Leave it **empty** to use the built-in ngosang `trackers_best_ip.txt` mirrors, tried in order:
  1. `https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best_ip.txt`
  2. `https://ngosang.github.io/trackerslist/trackers_best_ip.txt`
  3. `https://cdn.jsdelivr.net/gh/ngosang/trackerslist@master/trackers_best_ip.txt`
  4. `https://raw.githack.com/ngosang/trackerslist/master/trackers_best_ip.txt`

  A custom URL is tried **first**, then the mirrors. Each fetch times out after 5 s and falls back to the next URL, and to `DefaultTrackers` when all of them fail. The list is fetched in the background at start and refreshed every 12 hours; adding a torrent never waits for it.
- **`DefaultTrackers`** — local announce URLs, one per line (`udp`/`http`/`https`/`wss`; `#` starts a comment). Used alone when every remote fetch fails, otherwise appended after the remote list.

Optional file overlay (always appended when present): put `trackers.txt` in the config directory (`--path` / `-d`), next to `config.db`. Only lines starting with `udp` or `http` are read from that file.

## HTTPS

Start with `--ssl` to serve the web UI and API over HTTPS on `--sslport` (default 8091) alongside plain HTTP on `--port`. Plain HTTP requests sent to the HTTPS port are redirected to `https://`. There are three ways to get a certificate:

| Option | Trusted by browsers | Trusted by players/TVs | Needs |
|---|---|---|---|
| Self-signed (default) | after accepting a warning | no | nothing |
| Let's Encrypt via DNS-01, LAN only | yes | yes (except Android ≤ 7.0) | a domain or free DuckDNS name |
| Reverse proxy (Caddy, nginx) | yes | yes | a domain and a port open to the internet |

### HTTP and HTTPS modes

Four flags decide what the plain HTTP port (`--port`, default 8090) and the HTTPS port (`--sslport`, default 8091) serve:

| Mode | Flags | HTTP port | HTTPS port | Use it when |
|---|---|---|---|---|
| HTTP only (default) | none | everything | not opened | a trusted home network, or behind a [reverse proxy](#reverse-proxy) |
| HTTP and HTTPS | `--ssl` | everything | everything | browsers use HTTPS, players and TVs keep using HTTP |
| HTTPS, media also on HTTP | `--ssl --force-https --http-media` | media only; everything else redirects to HTTPS | everything | the self-signed certificate, with players and TVs that can't use it |
| HTTPS preferred | `--ssl --force-https` | redirects everything to HTTPS | everything | a trusted certificate; clients that type `http://` are sent to HTTPS |
| HTTPS only | `--ssl --https-only` | not opened | everything | a trusted certificate, and nothing should ever travel unencrypted |

- **Media** means `/stream`, `/play`, `/playlist`, `/playlistall` (also used by DLNA) and GStreamer HLS under `/gst/<hash>/`. The web UI, the API and GStreamer control endpoints (`/gst/settings`, `/gst/remove`, `/gst/echo`) are not media.
- **Redirects** are `307 Temporary Redirect` to the same path and query on `https://<host>:<sslport>`. A request still reaches the HTTP port before it's redirected, so its URL and any Basic auth credentials have already been sent unencrypted. `--https-only` removes that path, because the HTTP port is never opened. The same applies to plain HTTP sent to the HTTPS port (below) in every mode, so always configure clients with the `https://` address.
- **Plain HTTP sent to the HTTPS port** (for example `http://host:8091`) is redirected to `https://` in every mode, instead of failing with "Client sent an HTTP request to an HTTPS server".
- **Links handed to players:** playlists and the web UI's external-player and copy-link buttons use the address the page was opened on. The exception is a page opened over the self-signed certificate while the HTTP port serves media: then they point at the HTTP port, because players reject that certificate. DLNA links point at the HTTP port when it serves media, and at the HTTPS port otherwise. With `--https-only`, Bonjour advertises `_torrserver` on the HTTPS port and doesn't advertise `_http`.
- **TorrServer's own requests** (ffprobe, GStreamer) use an internal listener on a random `127.0.0.1` port that is never redirected, so they work in every mode, including when `--ip` excludes loopback.
- **Self-signed certificate with `--force-https` or `--https-only`:** startup logs a warning, because most players and TVs won't play. Use a trusted certificate, or `--force-https --http-media` on a trusted network.
- **An unusable certificate doesn't stop the server:** TorrServer logs what to fix, leaves the files alone and keeps serving plain HTTP with HTTPS off for that run (so redirects, Bonjour records and generated links don't point at a port nobody listens on). This is a TorrServer-LT difference: upstream refuses to start.
- **Invalid combinations stop startup:** `--force-https` or `--https-only` without `--ssl`, `--http-media` without `--force-https`, and `--https-only` with `--http-media`. `--force-https` with `--https-only` is allowed and behaves like `--https-only`.
- **Apps that use the API** (Lampa and other TorrServer clients that add torrents, list them or call `/gst/remove`) must be configured with the `https://` address when `--force-https` or `--https-only` is on: HTTP redirects are unreliable for browser API requests, particularly those requiring CORS preflight. `--http-media` only helps players that receive stream links.
- **Docker:** `TS_SSL_ENABLE`, `TS_FORCE_HTTPS_ENABLE`, `TS_HTTP_MEDIA_ENABLE` and `TS_HTTPS_ONLY_ENABLE` set to `1` enable the matching flags.

HTTPS is only on when TorrServer is started with `--ssl`; the choice isn't saved in the settings. The HTTPS port, certificate and key paths are saved, and reused on later starts with `--ssl`.

Examples:

```bash
# HTTPS only, with a trusted certificate
TorrServer --ssl --https-only --sslcert /opt/torrserver/tls/fullchain.pem --sslkey /opt/torrserver/tls/key.pem

# self-signed certificate for browsers, players and TVs on HTTP
TorrServer --ssl --force-https --http-media
```

### Certificate in the web UI

With `--ssl`, **Settings → Additional → HTTPS** (turn on **PRO mode** to see the Additional tab) shows the HTTPS mode and ports, the certificate in use (names, issuer, expiry, whether a trusted CA issued it) and the certificate and key files it's served from. The certificate can be changed there without a restart. The mode and ports stay startup flags, and without `--ssl` the section isn't shown.

- **Upload** a PEM certificate (full chain) and its unencrypted private key from the computer you're browsing on, e.g. `fullchain.pem` and `privkey.pem`. The pair must match and be currently valid. TorrServer copies them to `ssl/uploaded.crt` and `ssl/uploaded.key` in its config folder (the `--path` folder, e.g. `/opt/torrserver` with the Linux install script or `/opt/ts/config` in Docker; the HTTPS section shows the full path) and serves them within a few seconds. The upload sends the private key, so do it over HTTPS or on the TorrServer machine itself. The copy isn't renewed: upload again after renewing, or use the next option.
- **Use these files** takes the full paths of a certificate and key that are already on the TorrServer machine, as TorrServer sees them (inside the container for Docker); nothing is uploaded. Use it for certificates renewed automatically by acme.sh, certbot or another ACME client: TorrServer notices when the files change and serves the renewed certificate without a restart.
- **Use self-signed** switches back to TorrServer's self-signed certificate (the existing one is reused, so devices that trust it keep working) and deletes an uploaded copy.
- **Regenerate** creates a new self-signed certificate and key.
- **Download certificate** saves the certificate in use (never the key), e.g. to trust the self-signed one on your devices.

The same actions are available in the API under `/ssl/` (see `/swagger`). They are not available with `--rdb`, and the certificate can't be changed here when `--sslcert`/`--sslkey` are given, because those flags are applied again on every start; start without them to manage the certificate from the web UI.

### Self-signed certificate

Without `--sslcert`/`--sslkey`, TorrServer generates a self-signed certificate for `localhost`, the hostname, `hostname.local` and the local IPs, and renews it before it expires or when the host moves to a new IP. Global IPv6 addresses are included but don't cause a renewal when they change, as IPv6 privacy extensions rotate them every few hours. Browsers show a warning you can accept once. Most media players, TVs and DLNA renderers reject it, so give them HTTP links: don't use `--force-https`, or add `--http-media` on a trusted network. When a playlist is requested over the self-signed HTTPS port and HTTP still serves media, its links point to the HTTP port. The self-signed certificate is only ever regenerated if it is one TorrServer created; your own certificate is never touched, even at the default location.

### Trusted certificate on your LAN (Let's Encrypt DNS-01)

Let's Encrypt can issue a certificate for a name that points to a **private** IP. It verifies you own the name through a DNS record, so nothing has to be reachable from the internet. Example with [DuckDNS](https://www.duckdns.org) (free) and [acme.sh](https://github.com/acmesh-official/acme.sh):

1. Create a DuckDNS subdomain, e.g. `mytorr.duckdns.org`, and set its IP to TorrServer's LAN IP (e.g. `192.168.1.10`). Give the host a DHCP reservation so that IP doesn't change.
2. Check it resolves on your LAN: `dig +short mytorr.duckdns.org`. If it returns nothing, your router's DNS rebinding protection blocks public names with private IPs; allow the domain there.
3. Issue the certificate and install it where TorrServer reads it:

   ```bash
   DuckDNS_Token=YOUR_TOKEN acme.sh --issue --dns dns_duckdns -d mytorr.duckdns.org --server letsencrypt --dnssleep 180
   acme.sh --install-cert -d mytorr.duckdns.org --key-file /opt/torrserver/tls/key.pem --fullchain-file /opt/torrserver/tls/fullchain.pem
   ```

4. Start TorrServer with the certificate. Use the **full chain** file: TVs and Android players reject a certificate without its intermediate.

   ```bash
   TorrServer --ssl --sslcert /opt/torrserver/tls/fullchain.pem --sslkey /opt/torrserver/tls/key.pem
   ```

   Or start with just `--ssl` and set the same two paths in the web UI under **Use these files** (see [Certificate in the web UI](#certificate-in-the-web-ui)).

5. Open `https://mytorr.duckdns.org:8091` on any device on the LAN. Use the name, not the IP: the IP isn't in the certificate.
6. Optional: add `--https-only` so TorrServer doesn't open the plain HTTP port at all, or `--force-https` to keep it open but redirect it to HTTPS.

acme.sh renews the certificate automatically every ~60 days and rewrites the files; TorrServer picks up the new files within seconds, without a restart. Any ACME client with DNS-01 support (certbot, lego, Caddy with a DNS plugin) works the same way. Note that certificate names are published in public Certificate Transparency logs.

### TorrServer on an Android TV box

A certificate belongs to a name, not to a device or IP, so it doesn't have to be issued on the box. Two practical setups:

**Issue on a computer, push to the box.** Run the DNS-01 steps above on a Mac, Linux or Windows (WSL) machine, point the DuckDNS name at the **box's** LAN IP, and let acme.sh copy each renewed certificate to the box over ADB (enable network debugging on the box):

```bash
acme.sh --install-cert -d mytorr.duckdns.org --key-file ~/tls/key.pem --fullchain-file ~/tls/fullchain.pem --reloadcmd "adb connect 192.168.1.50 && adb push ~/tls/fullchain.pem ~/tls/key.pem /sdcard/torrserver/tls/"
```

Start TorrServer on the box with `--ssl --sslcert /sdcard/torrserver/tls/fullchain.pem --sslkey /sdcard/torrserver/tls/key.pem`. TorrServer reloads the pushed files within seconds, without a restart. The computer has to be on around renewal time (every ~60 days).

**Let another always-on device handle HTTPS.** If you have a NAS, Raspberry Pi or router that can run [Caddy](https://caddyserver.com) (built with the [DuckDNS plugin](https://github.com/caddy-dns/duckdns)), point the name at that device and forward to the box. Caddy obtains and renews the certificate by itself, and the box runs TorrServer without `--ssl`:

```
mytorr.duckdns.org {
    tls {
        dns duckdns YOUR_TOKEN
    }
    reverse_proxy 192.168.1.50:8090 {
        flush_interval -1
    }
}
```

Running acme.sh on the box itself (e.g. in Termux) also works, but Android's storage restrictions and aggressive background-app killing make renewals unreliable. The box's own Android version doesn't matter, because TorrServer brings its own TLS stack; only the devices that *connect* to it must trust Let's Encrypt.

### Reverse proxy

If TorrServer is exposed to the internet under a domain, the simplest option is a reverse proxy that handles certificates itself. Never expose the plain HTTP port (`--port`) to the internet: Basic auth credentials and stream URLs would travel unencrypted. Run TorrServer without `--ssl`, enable `--httpauth`, and bind it to loopback with `--ip 127.0.0.1` when the proxy runs on the same host. TorrServer honours `X-Forwarded-Proto`/`X-Forwarded-Host`, so playlist links use the public `https://` name. Disable response buffering, or playback will stutter.

Caddy (obtains and renews Let's Encrypt certificates automatically):

```
tv.example.com {
    reverse_proxy 127.0.0.1:8090 {
        flush_interval -1
    }
}
```

nginx (certificate from certbot or similar):

```nginx
location / {
    proxy_pass http://127.0.0.1:8090;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host $host;
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 24h;
    client_max_body_size 50m;
}
```

## External subtitles

TorrServer can find subtitle files that lie next to a video inside a torrent and serve them to clients, converted to WebVTT or in their original format. It works in all builds and needs no external tools.

```
GET /subs/{hash}/{id}                  list subtitle tracks for the video file {id}
GET /subs/{hash}/{id}?track={index}    return one subtitle file (WebVTT by default)
```

Query parameters for `track`: `format` (`vtt` or `original`) and `encoding` (`utf8` or `windows-1251`).

With a `-gst` build the tracks are also listed in the HLS master playlist, so the built-in player offers them without any extra request.

See [server/subs/README.md](server/subs/README.md) for the supported formats and encodings, and for the file layouts inside a torrent that are recognized.

## Web Application Firewall (WAF)

TorrServer includes an HTTP access WAF that filters clients by IP address and by the `Referer` and `Origin` request headers. Configure it from **Settings → WAF** or through the authenticated `/waf` API.

### Configuration

WAF configuration is stored in the top-level **`waf`** object in **`settings.json`**. Each rule is a separate array entry:

```json
{
  "waf": {
    "version": 1,
    "whitelist": [
      "127.0.0.1",
      "::1",
      "10.0.0.0/8"
    ],
    "blacklist": [
      "203.0.113.0/24"
    ],
    "referers": [
      "example.com"
    ]
  }
}
```

On first start, if `settings.json` has **no** `waf` key yet and legacy ACL files **`wip.txt`** (whitelist) / **`bip.txt`** (blacklist) exist in the config directory (same place as `config.db`), TorrServer imports them into `waf` arrays and renames the sources to **`wip.txt.bak`** / **`bip.txt.bak`**. Those backups are not read again. If a `waf` key already exists (even with empty lists), legacy files are left untouched.

Changes saved through the web UI or API are applied immediately. After editing `settings.json` manually, restart TorrServer to load the changes.

### IP rules

Rules:

- If the whitelist is **not empty**, the client IP must match it.
- If the blacklist is **not empty**, a matching client IP is banned even when it is also on the whitelist.
- An empty whitelist or blacklist disables that IP check.
- Invalid entries are skipped and reported as warnings; valid entries remain active.
- Banned responses use HTTP **403** with body `Banned`.
- Client IP is taken from the TCP peer address (`RemoteAddr`). Reverse-proxy headers are not trusted by default.

Supported array-entry formats include IPv4, IPv6, ranges, CIDR blocks, comments, and optional descriptions:

```json
[
  "# comment",
  "127.0.0.1",
  "local:127.0.0.1",
  "127.0.0.0-127.0.0.255",
  "local:127.0.0.0-127.0.0.255",
  "10.0.0.0/8",
  "lan:10.0.0.0/8",
  "2001:db8::1",
  "local:2001:db8::1",
  "2001:db8::/32"
]
```

### Referer and Origin rules

Block HTTP requests that come from unwanted sites (for example mirror pages that embed your TorrServer streams).

- Each entry is a hostname. URLs with only an HTTP/HTTPS scheme and host are also accepted.
- A rule blocks the hostname and all its subdomains.
- Both `Referer` and `Origin` are checked before the IP allowlist, so an IP whitelist match cannot bypass a referer rule.
- Requests without either header are allowed.
- A built-in list of hosts (the same as upstream TorrServer's) is enforced on top of your own entries. The web UI shows it, and **Settings → WAF → Built-in referer blocklist** or `"disable_default_referers": true` in the `waf` object turns it off.

```json
{
  "referers": [
    "example.com",
    "evil.example.org",
    "# comment"
  ]
}
```

### API

`GET /waf` returns the active editable lists, the built-in referer list (`default_referers`) and whether it is enforced (`default_referers_enabled`), status flags, and parse warnings. `POST /waf` atomically replaces all three editable lists and hot-reloads the WAF. The API uses newline-delimited strings for compatibility with the web text editors; the three lists are required and an empty string clears one. `default_referers_enabled` is optional; leaving it out keeps the current state.

```shell
curl -u USER:PASSWORD http://127.0.0.1:8090/waf

curl -u USER:PASSWORD \
  -H 'Content-Type: application/json' \
  -d '{"whitelist":"127.0.0.1\n::1\n10.0.0.0/8","blacklist":"","referers":"example.com"}' \
  http://127.0.0.1:8090/waf
```

In read-only mode, `GET /waf` remains available but `POST /waf` returns HTTP **403**.

> **Note:** BitTorrent peer IP filtering uses a separate PeerGuardian-style file named `blocklist` in the config directory. That list is not managed by Settings → WAF / `/waf`.

## Torznab

TorrServer can talk to **Torznab** indexers so you can search for torrents from tools like **Jackett** and **Prowlarr**, including searching several configured indexers at once.

Configure it in the web UI: **Settings → Torznab**.

### Indexer parameters

Each Torznab indexer needs:

- **Host URL**: Torznab base URL. TorrServer always calls the `/api` endpoint and accepts the host with or without a trailing `/api` or `/api/` (it will not be doubled).
  - Jackett example (either form works):

  ```shell
  http://192.168.1.10:9117/api/v2.0/indexers/all/results/torznab/
  http://192.168.1.10:9117/api/v2.0/indexers/all/results/torznab/api
  ```

  - Prowlarr example:

  ```shell
  http://localhost:9696/1
  ```

  - Other indexers often document the full endpoint with `/api` (e.g. `http://127.0.0.1:9117/torznab/api`). You can paste that as-is.

- **API Key**: the key from your Torznab indexer manager.
- **Categories** (PRO mode): which Torznab categories this indexer is searched with.
  - **Default** — movies and TV (`5000,2000`). This is also what indexers added before the setting existed use.
  - **Manual** — the comma-separated list you enter (e.g. `5000,2000,3000`); an empty list searches everything.
  - **All** — no category is sent, the indexer decides.

  A search request may still override this: `GET /torznab/search?cat=...` searches those
  categories, `cat=` (empty) searches all of them, and omitting `cat` uses each indexer's
  own setting (the Telegram bot and the MCP server always do the latter).

### Enabling Torznab search

1. Open **Settings**.
2. Open the **Torznab** tab.
3. Turn on **Enable Torznab Search**.
4. Enter **Host URL** and **API Key** (and, in PRO mode, pick the **Categories**), then **Add Server** for each indexer.
5. **Save** settings.

## GStreamer

GStreamer enables **HLS transcoding** for Matroska/WebM torrents (and AVI when `TranscodeAVI` is on) when the client cannot play the original video or audio codec directly (typically audio: AC3/EAC3/DTS → AAC; video H.264/H.265/AV1/VP9/VP8 passes through, or is re-encoded when the matching `Transcode*` option is on). It can also convert embedded text subtitles to segmented WebVTT (`Subtitles`), tone-map HDR video to SDR (`HDRToSDR`), and encode on the GPU with a hardware H.264 encoder, falling back to x264 (`HardwareAcceleration`, `UseGPU`).

### The `-gst` binary (main requirement)

The **only** way to enable GStreamer in TorrServer-LT is to run a build compiled with the `gst` tag. There is no on/off switch in the settings.

| Binary | GStreamer |
| --- | --- |
| `TorrServer-LT-<platform>-gst` | **Yes** — `/gst/*` routes and transcoding |
| `TorrServer-LT-<platform>` (standard) | **No** — `GET /gst/settings` returns `built_in: false`; no other `/gst/*` routes are registered and pipelines do not run |

Download `TorrServer-LT-*-gst` from [releases](https://github.com/trinity-aml/TorrServer-LT/releases) (Windows amd64 / Linux amd64+arm64 / macOS amd64+arm64), let the install scripts do it (`--gst`, see [Installation](#installation)), or build it yourself — the `build/*.sh` scripts produce both variants automatically, and for a manual build it is just the tag on top of the usual cgo environment:

```bash
cd server
go build -tags gst -o TorrServer-LT-gst ./cmd
```

Per-codec behavior is configured on the **GStreamer** settings tab (`TranscodeH264`, `TranscodeH265`, `TranscodeAV1`, `TranscodeVP9`, `TranscodeVP8`, `TranscodeAVI`), along with the subtitle (`Subtitles`), HDR→SDR (`HDRToSDR`) and hardware-encoder (`HardwareAcceleration`, `UseGPU`) toggles.

### Bundled vs dynamically loaded GStreamer

TorrServer-LT stays a single Go binary. GStreamer is **not** linked into it at compile time — the libraries are loaded at runtime via `dlopen` / `LoadLibrary` (purego). What changes is **where** the GStreamer libraries and plugins come from:

| Mode | How it works | Typical use |
| --- | --- | --- |
| **Bundled (portable)** | Place a `gst-lib/` directory next to the TorrServer executable (same layout as an extracted GStreamer runtime) | Portable installs, custom deployments |
| **Dynamic (system)** | TorrServer loads `libgstreamer` / DLLs from OS packages or a system install path (`GSTPath`, `/opt/gstreamer`, framework path, etc.) | Linux, macOS, Windows with the [official GStreamer installer](https://gstreamer.freedesktop.org/download/) |
| **Bundled (embedded)** | GStreamer runtime packed inside the binary and extracted to cache on first run — needs a custom Windows build with the extra `embed_gstlib` tag; release binaries don't include it | Self-contained Windows deployments |

Auto-detection order: `GSTPath` from settings → `gst-lib/` beside the binary → common system paths → `LD_LIBRARY_PATH` / `PATH`.

Verify whichever mode you use with `GET /gst/echo` or the status lines on the **GStreamer** settings tab.

### Installing GStreamer for dynamic loading

Needed for the `-gst` builds unless you ship a portable `gst-lib/`. Minimum GStreamer version: **1.22**.

**Debian / Ubuntu**

```bash
sudo apt update
sudo apt install -y \
  gstreamer1.0-tools \
  gstreamer1.0-plugins-base \
  gstreamer1.0-plugins-good \
  gstreamer1.0-plugins-bad \
  gstreamer1.0-plugins-ugly \
  gstreamer1.0-libav
```

**Fedora / RHEL / Rocky / AlmaLinux**

```bash
sudo dnf install -y \
  gstreamer1-tools \
  gstreamer1-plugins-base \
  gstreamer1-plugins-good \
  gstreamer1-plugins-bad-free \
  gstreamer1-plugins-ugly-free \
  gstreamer1-libav
```

`x264enc` (used when transcoding video to H.264) may require [RPM Fusion](https://rpmfusion.org/) on Fedora.

**Arch Linux**

```bash
sudo pacman -S --needed \
  gst-plugins-base \
  gst-plugins-good \
  gst-plugins-bad \
  gst-plugins-ugly \
  gst-libav
```

**macOS**

[Official framework](https://gstreamer.freedesktop.org/download/) or Homebrew:

```bash
brew install gstreamer gst-plugins-base gst-plugins-good gst-plugins-bad gst-plugins-ugly gst-libav
```

Set `GSTPath` if needed (e.g. `/Library/Frameworks/GStreamer.framework/Versions/1.0`).

**Windows (dynamic)**

Install the MSVC 64-bit runtime from [gstreamer.freedesktop.org](https://gstreamer.freedesktop.org/download/) (default: `C:\Program Files\gstreamer\1.0\mingw_x86_64`), or use a `gst-lib/` folder next to the exe.

### Web UI configuration

1. Open **Settings**.
2. Enable **PRO mode**.
3. Open the **GStreamer** tab (shown only on `-gst` builds).
4. Adjust options and click **Save GStreamer Settings**.

GStreamer settings are stored separately from the main BitTorrent settings and take effect immediately for new streams.

### Configuration fields

| Field | Description |
| --- | --- |
| `GSTVersion` | Installed GStreamer version (minimum `1.22`, e.g. `1.22`, `1.28`). Used for pipeline feature selection. |
| `GSTPath` | Path to GStreamer installation. Empty = auto-detection. |
| `Source` | Input URL mode: `stream` (`/stream/...`) or `play` (`/play/...`). |
| `MaxTasks` | Parallel transcode task limit (`0` = default). |
| `InactiveMinutes` | Freeze a pipeline after this many minutes without playback. |
| `AACBitrateKbps` | Audio transcoding bitrate in kbps. |
| `SegmentSeconds` | HLS segment length in seconds. |
| `appsinkBuffers` | Number of buffers in the appsink queue. |
| `TranscodeH264` / `TranscodeH265` / `TranscodeAV1` / `TranscodeVP9` | Re-encode that video codec instead of passing it through. |
| `VideoBitrate` | Target video bitrate in kbps when transcoding video. |
| `tempfs` | Use memory-backed tempfs for segments (Linux). |
| `tempfs_ring` | Extra tempfs ring blocks (`0` = default). |
| `PlaylistHLS` | **Fork extension.** Generated M3U playlists point MKV/WebM entries at `/gst/{hash}/master.m3u8` instead of the direct `/stream` link, so any HLS-capable player gets the AAC-transcoded audio straight from the playlist; other containers keep direct links. No effect on base builds. |

### API

**Settings** (requires authentication when `--httpauth` is enabled):

- `GET /gst/settings` — on `-gst` builds: `built_in`, current config, and platform defaults; on standard builds: `{ "built_in": false }` only
- `POST /gst/settings` — update or reset config (`404` on standard builds)

  ```json
  { "action": "set", "config": { "GSTVersion": 1.22, "Source": "stream" } }
  ```

  Reset to defaults:

  ```json
  { "action": "def" }
  ```

**Streaming** (available on `-gst` builds only):

| Endpoint | Description |
| --- | --- |
| `GET /gst/echo` | GStreamer / gst-discoverer health check |
| `GET /gst/:hash/probe` | Probe torrent file codecs (`index`, `id`, or `fileID` query) |
| `GET /gst/:hash/master.m3u8` | HLS master playlist |
| `GET /gst/:hash/init.mp4` | Initialization segment |
| `GET /gst/:hash/seg/*segment` | Media segment |
| `GET /gst/:hash/heartbeat` | Keep-alive for the active transcode task |
| `GET /gst/remove` | Stop a transcode task (`hash` or `id` query) |

Standard binaries serve a filtered Swagger spec at runtime (only `/gst/settings`); `-gst` builds document all `/gst/*` endpoints.

## Donate

- [YooMoney](https://yoomoney.ru/to/410013733697114/200)
- [Boosty](https://boosty.to/yourok)
- [TBank](https://www.tbank.ru/cf/742qEMhKhKn)

## Thanks to everyone who tested and helped

- [anacrolix](https://github.com/anacrolix) Matt Joiner
- [tsynik](https://github.com/tsynik) Nikk Gitanes
- [dancheskus](https://github.com/dancheskus) for react web GUI and PWA code
- [kolsys](https://github.com/kolsys) for initial Media Station X support
- [damiva](https://github.com/damiva) for Media Station X code updates
- [vladlenas](https://github.com/vladlenas) for NAS builds
- [pavelpikta](https://github.com/pavelpikta) Pavel Pikta for linux install script and more
- [Nemiroff](https://github.com/Nemiroff) Tw1cker
- [spawnlmg](https://github.com/spawnlmg) SpAwN_LMG for testing
- [TopperBG](https://github.com/TopperBG) Dimitar Maznekov for Bulgarian web translation
- [FaintGhost](https://github.com/FaintGhost) Zhang Yaowei for Simplified Chinese web translation
- [Anton111111](https://github.com/Anton111111) Anton Potekhin for sleep on Windows fixes
- [lieranderl](https://github.com/lieranderl) Evgeni for adding SSL support code
- [cocool97](https://github.com/cocool97) for openapi API documentation and torrent categories
- [shadeov](https://github.com/shadeov) for README improvements
- [butaford](https://github.com/butaford) Pavel for make docker file and scripts
- [filimonic](https://github.com/filimonic) Alexey D. Filimonov
- [leporel](https://github.com/leporel) Viacheslav Evseev
- and others
