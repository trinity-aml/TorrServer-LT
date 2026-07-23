#!/usr/bin/env bash
# Cross-build for darwin/arm64 (Apple Silicon) from Linux via OSXCross.
#
# ⚠️  LEGAL: OSXCross needs the Apple macOS SDK, whose licence only permits
#     use on Apple-branded hardware. Cross-building macOS binaries on a Linux
#     host is a grey area — for distribution, build on a real Mac / CI macOS
#     runner (see .github/workflows/build-macos.yml). This script exists so
#     "all architectures locally" is reproducible where that SDK is available.
#
# Works with either an OSXCross source build or the crazymax/osxcross image
# tree (needs SDK >= 11.0 for arm64, e.g. crazymax/osxcross:13.1). Point
# OSXCROSS_ROOT at it; see build/_osxcross.sh for the extract recipe.
#   export OSXCROSS_ROOT=/opt/osxcross

# shellcheck source=_osxcross.sh
. "$(dirname "$0")/_osxcross.sh"
osxcross_setup arm64

TARGET=darwin-arm64
GOOS=darwin GOARCH=arm64
CC="$OSX_CC"
CXX="$OSX_CXX"
# Boost.Build's `darwin` toolset (not `clang`) defines the <framework> feature
# libtorrent's Jamfile uses for macOS, plus correct Mach-O link semantics.
B2_COMPILER=darwin
B2_VARIANT=darwin_arm64
B2_TOOLSET_CXX="$OSX_CXX"
B2_FLAGS="target-os=darwin address-model=64 architecture=arm"
# The darwin toolset archives with `libtool -static`, so <archiver> must be the
# Mach-O libtool (not ar — host GNU libtool/ar can't make a Mach-O archive).
B2_USERCONFIG_EXTRA=": <archiver>$OSX_LIBTOOL"
[[ -n "$OSX_RANLIB" ]] && B2_USERCONFIG_EXTRA+=" <ranlib>$OSX_RANLIB"
# libtorrent's pkg-config doesn't emit the macOS frameworks it needs (its
# ip_notifier uses SystemConfiguration/SCDynamicStore). Go already pulls
# CoreFoundation+Security for its runtime; add the libtorrent ones here.
EXTRA_CGO_LDFLAGS="-framework SystemConfiguration -framework CoreFoundation"
# OpenSSL/webrtc deps: Mach-O archives need the osxcross ar/ranlib (host GNU
# ar can't produce them); cmake gets the explicit Darwin cross set.
OPENSSL_AR="$OSX_AR"
OPENSSL_RANLIB="$OSX_RANLIB"
CMAKE_CROSS_ARGS="-DCMAKE_SYSTEM_NAME=Darwin -DCMAKE_C_COMPILER=$OSX_CC -DCMAKE_OSX_ARCHITECTURES=arm64${OSXCROSS_SDKROOT:+ -DCMAKE_OSX_SYSROOT=$OSXCROSS_SDKROOT} -DCMAKE_AR=$(command -v "$OSX_AR") -DCMAKE_RANLIB=$(command -v "$OSX_RANLIB")"

export TARGET GOOS GOARCH CC CXX B2_COMPILER B2_VARIANT B2_TOOLSET_CXX B2_FLAGS \
       B2_USERCONFIG_EXTRA OPENSSL_AR OPENSSL_RANLIB CMAKE_CROSS_ARGS

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
