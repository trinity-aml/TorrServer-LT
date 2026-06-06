# Local cross-build (no Docker)

These scripts build **TorrServer-LT** for every supported target straight on a
Linux host — no Docker, no QEMU, no cmake. Each one builds libtorrent (and
`boost_system` straight from the Boost source tree) with Boost.Build (`b2`)
into `_deps/<target>/`, then links the Go binary against it through pkg-config.
Final binaries land in `_out/TorrServer-LT-<target>`.

libtorrent's own `Jamfile` emits a correct `libtorrent-rasterbar.pc` from its
`install` target, so the per-target Cflags/defines/Libs flow straight into cgo
(`server/lt` uses `#cgo pkg-config: libtorrent-rasterbar`). libtorrent is built
with `crypto=built-in`, so there is no OpenSSL cross-dependency.

```
build/
  _common.sh         paths + pinned versions (Boost 1.85.0, libtorrent v2.0.10)
  _fetch_sources.sh  download Boost + clone libtorrent into _src/  (idempotent)
  _deps.sh           shared engine: b2 install libtorrent → go_build
  all.sh             build every target whose toolchain is present
  <target>.sh        per-target toolchain setup + cross_build
```

`_src/`, `_deps/`, `_out/` are git-ignored.

## One command

```bash
build/all.sh                       # everything the host can build
TARGETS="linux-arm64" build/all.sh # a subset
```

`all.sh` never silently drops a target: each is reported `OK` / `FAIL` /
`SKIP` (toolchain missing).

## Prerequisites per target

The host needs `go`, `curl`, `git` and a host C++ compiler (to bootstrap
Boost's `b2`). No cmake.

| Target          | Install                                                                 |
|-----------------|-------------------------------------------------------------------------|
| `linux-amd64`   | nothing extra — uses the host gcc/g++ (libtorrent built static)         |
| `linux-arm64`   | `sudo apt install gcc-aarch64-linux-gnu g++-aarch64-linux-gnu`           |
| `linux-armv7`   | `sudo apt install gcc-arm-linux-gnueabihf g++-arm-linux-gnueabihf`       |
| `windows-amd64` | `sudo apt install gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64`             |
| `android-arm64` | Android NDK r26d, `export ANDROID_NDK_HOME=/opt/android-ndk-r26d`        |
| `android-armv7` | same as android-arm64                                                   |
| `darwin-arm64`  | OSXCross, `export OSXCROSS_ROOT=/opt/osxcross` (see ⚠️ below)            |
| `darwin-amd64`  | same as darwin-arm64                                                     |

One apt line for the three apt-installable cross targets:

```bash
sudo apt install \
  gcc-aarch64-linux-gnu g++-aarch64-linux-gnu \
  gcc-arm-linux-gnueabihf g++-arm-linux-gnueabihf \
  gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64
```

> ⚠️ **macOS targets** need the Apple macOS SDK via OSXCross. Apple's licence
> only permits that SDK on Apple hardware, so cross-building macOS binaries on
> Linux is a legal grey area. For anything distributed, build on a real Mac or
> the macOS CI runner (`.github/workflows/build-macos.yml`). The scripts exist
> so "all architectures locally" is reproducible where the SDK is available.

## Notes

- Every target — `linux-amd64` included — builds libtorrent from source and
  links it statically; the only dynamic deps are libc/libstdc++/libgcc (and on
  Windows nothing extra, those are linked static too).
- Boost/libtorrent are built once per target and cached in `_deps/<target>/`;
  re-running a script is a fast no-op for the deps and only relinks Go.
- Versions are pinned in `_common.sh`; override with e.g.
  `LIBTORRENT_TAG=v2.0.11 build/linux-arm64.sh`.
