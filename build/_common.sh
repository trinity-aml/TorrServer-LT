# shellcheck shell=bash
# Common bits sourced by every build/<target>.sh script.
#
# Layout:
#   _src/    — third-party source trees (Boost, libtorrent)
#   _deps/   — per-target cross-built install trees
#   _out/    — final TorrServer-LT-<target> binaries
#
# Versions are pinned here so all targets stay consistent.

set -euo pipefail

# --- pinned versions -------------------------------------------------
BOOST_VERSION=${BOOST_VERSION:-1.85.0}
LIBTORRENT_TAG=${LIBTORRENT_TAG:-v2.1.0}
# Static OpenSSL per target: required by webtorrent=on (DTLS + wss:// trackers)
# and gives libtorrent https tracker/web-seed support (crypto=openssl).
# 3.5 is the LTS branch (EOL 2030-04).
OPENSSL_VERSION=${OPENSSL_VERSION:-3.5.7}
# First number must stay >= 132: the Android client gates its settings UI on the
# leading digit run (>131 shows PreloadCache, hides the legacy PreloadBuffer).
TS_VERSION=${TS_VERSION:-MatriX.142.LT-1.1.8}

# --- paths -----------------------------------------------------------
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC_DIR="$ROOT/_src"
DEPS_ROOT="$ROOT/_deps"
OUT_DIR="$ROOT/_out"

mkdir -p "$SRC_DIR" "$DEPS_ROOT" "$OUT_DIR"

# --- helpers ---------------------------------------------------------
log() { printf '\033[1;36m[%s]\033[0m %s\n' "$(basename "${0:-build}")" "$*"; }
die() { printf '\033[1;31m[fatal]\033[0m %s\n' "$*" >&2; exit 1; }

# nproc fallback for systems where it isn't on PATH
JOBS=${JOBS:-$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)}
