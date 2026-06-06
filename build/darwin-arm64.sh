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
B2_COMPILER=clang
B2_VARIANT=darwin_arm64
B2_TOOLSET_CXX="$OSX_CXX"
B2_FLAGS="target-os=darwin address-model=64 architecture=arm"
# Use the Mach-O archiver/ranlib for the static lib (host GNU ar/ranlib produce
# an index ld64 can't use). Appended as the 4th `using` toolset field.
[[ -n "$OSX_AR" ]] && B2_USERCONFIG_EXTRA=": <archiver>$OSX_AR <ranlib>${OSX_RANLIB:-$OSX_AR}"

export TARGET GOOS GOARCH CC CXX B2_COMPILER B2_VARIANT B2_TOOLSET_CXX B2_FLAGS \
       B2_USERCONFIG_EXTRA

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
