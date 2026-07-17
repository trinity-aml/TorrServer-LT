#!/usr/bin/env bash
# NATIVE macOS build (run on a real Mac / CI macos runner) for arm64 or amd64.
# Usage: build/darwin-native.sh <arm64|amd64>
#
# Builds the PINNED libtorrent ($LIBTORRENT_TAG in _common.sh) from source via
# b2, exactly like every cross target — instead of linking against Homebrew's
# libtorrent, whose version floats: brew's bump to 2.1.0 broke the CI macOS
# jobs (our disk_io shim targets the 2.0.x disk_interface API), which is why
# this script exists. amd64 on an Apple-Silicon host builds via `-arch x86_64`
# (cgo + b2 both), producing a real Intel binary.
#
# The osxcross variants (darwin-{arm64,amd64}.sh) stay for Linux-host local
# builds; this one needs no SDK juggling — just Xcode CLT and Go.

set -euo pipefail

ARCH="${1:-arm64}"
case "$ARCH" in
    arm64)
        B2_ARCH=arm
        ARCHFLAG=""
        ;;
    amd64)
        B2_ARCH=x86
        ARCHFLAG="-arch x86_64"
        ;;
    *)
        echo "usage: $0 <arm64|amd64>" >&2
        exit 1
        ;;
esac

TARGET="darwin-$ARCH"
GOOS=darwin GOARCH="$ARCH"
CC="clang $ARCHFLAG"
CXX="clang++ $ARCHFLAG"
# Boost.Build's `darwin` toolset: correct Mach-O archiving (libtool -static)
# and the <framework> feature libtorrent's Jamfile uses — all native here, no
# archiver override needed.
B2_COMPILER=darwin
B2_VARIANT="native_$ARCH"
B2_TOOLSET_CXX="clang++ $ARCHFLAG"
B2_FLAGS="target-os=darwin address-model=64 architecture=$B2_ARCH"
# libtorrent's ip_notifier needs SystemConfiguration (its .pc doesn't say so).
EXTRA_CGO_LDFLAGS="-framework SystemConfiguration -framework CoreFoundation"

export TARGET GOOS GOARCH CC CXX B2_COMPILER B2_VARIANT B2_TOOLSET_CXX B2_FLAGS \
       EXTRA_CGO_LDFLAGS

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
