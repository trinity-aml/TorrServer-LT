# TorrServer-LT

> Fork of [YouROK/TorrServer](https://github.com/YouROK/TorrServer) with the BitTorrent core replaced by [libtorrent (arvidn)](https://www.libtorrent.org/).
>
> **Status:** feature parity with upstream **MatriX.142.2** (including GStreamer HLS transcoding) on the libtorrent engine — streaming, preload, and seeking (including back into already-evicted regions) are verified on real torrents. The HTTP API, on-disk databases (`config.db`, JSON, `accs.db`, viewed) and the `torrs://` token format remain compatible with upstream. The cache layout under `TorrentsSavePath/<hash>/<pieceID>` is preserved.
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
  sudo bash ./installTorrServerLinux.sh --install MatriX.142.LT-1.1.5 --silent
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
- `--force-https` - with `--ssl`, the HTTP listener (`--port`) answers only with **307 Temporary Redirect** to the same path on HTTPS (`--sslport`). The web UI and API are served on HTTPS only; nothing is served on HTTP except redirects. Requires `--ssl` (startup fails if `--force-https` is set without `--ssl`). Default is off so plain HTTP still works when SSL is disabled.
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
TorrServer-darwin-arm64 [--port PORT] [--ip IP ...] [--path PATH] [--logpath LOGPATH] [--weblogpath WEBLOGPATH] [--rdb] [--httpauth] [--dontkill] [--ui] [--torrentsdir TORRENTSDIR] [--torrentaddr TORRENTADDR] [--pubipv4 PUBIPV4] [--pubipv6 PUBIPV6] [--searchwa] [--maxsize MAXSIZE] [--tg TGTOKEN] [--fuse FUSEPATH] [--webdav] [--ssl] [--sslport PORT] [--sslcert PATH] [--sslkey PATH] [--force-https]
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

- `TS_HTTPAUTH` - 1, and place auth file into `~/ts/config` folder for enabling basic auth
- `TS_RDB` - if 1, then the enabling `--rdb` flag
- `TS_DONTKILL` - if 1, then the enabling `--dontkill` flag
- `TS_PORT` - for changind default port to **5555** (example), also u need to change `-p 8090:8090` to `-p 5555:5555` (example)
- `TS_CONF_PATH` - for overriding torrserver config path inside container. Example `/opt/tsss`
- `TS_TORR_DIR` - for overriding torrents directory. Example `/opt/torr_files`
- `TS_LOG_PATH` - for overriding log path. Example `/opt/torrserver.log`
- `TS_PROXYURL` - set proxy URL for BitTorrent traffic (http, socks4, socks5, socks5h), example: socks5h://user:password@example.com:2080
- `TS_PROXYMODE` - set proxy mode: "tracker" (only HTTP trackers, default), "peers" (only peer connections), or "full" (all traffic)

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

## Whitelist/Blacklist IP

The lists file should be located in the same directory with config.db.

- Whitelist file name: `wip.txt`
- Blacklist file name: `bip.txt`

Whitelist has priority over everything else.

Example:

```text
local:127.0.0.0-127.0.0.255
127.0.0.0-127.0.0.255
local:127.0.0.1
127.0.0.1
# at the beginning of the line, comment
```

## Torznab

TorrServer can talk to **Torznab** indexers so you can search for torrents from tools like **Jackett** and **Prowlarr**, including searching several configured indexers at once.

Configure it in the web UI: **Settings → Torznab**.

### Indexer parameters

Each Torznab indexer needs:

- **Host URL**: full URL to the Torznab API endpoint.
  - Jackett example:

  ```shell
  http://192.168.1.10:9117/api/v2.0/indexers/all/results/torznab/
  ```

  - Prowlarr example:
  
  ```shell
  http://localhost:9696/1
  ```
  
  - Make sure to include the correct trailing slash (`/`) in your indexer's URL,
  as required by your Torznab provider. TorrServer will try to properly format the path,
  but matching your indexer's expected format is best to avoid connection issues.
  
- **API Key**: the key from your Torznab indexer manager.

### Enabling Torznab search

1. Open **Settings**.
2. Open the **Torznab** tab.
3. Turn on **Enable Torznab Search**.
4. Enter **Host URL** and **API Key**, then **Add Server** for each indexer.
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
