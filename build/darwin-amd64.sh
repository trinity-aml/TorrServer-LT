#!/usr/bin/env bash
# Cross-build for darwin/amd64 (Intel Mac) from Linux via OSXCross.
#
# ⚠️  LEGAL: see build/darwin-arm64.sh — the Apple SDK licence restricts this
#     to Apple hardware; for distribution prefer a macOS CI runner.
#
# Works with either an OSXCross source build or the crazymax/osxcross image
# tree. Point OSXCROSS_ROOT at it; see build/_osxcross.sh for the extract recipe.
#   export OSXCROSS_ROOT=/opt/osxcross

# shellcheck source=_osxcross.sh
. "$(dirname "$0")/_osxcross.sh"
osxcross_setup x86_64

TARGET=darwin-amd64
GOOS=darwin GOARCH=amd64
CC="$OSX_CC"
CXX="$OSX_CXX"
B2_COMPILER=clang
B2_VARIANT=darwin_amd64
B2_TOOLSET_CXX="$OSX_CXX"
B2_FLAGS="target-os=darwin address-model=64 architecture=x86"
# Use the Mach-O archiver/ranlib for the static lib (host GNU ar/ranlib produce
# an index ld64 can't use). Appended as the 4th `using` toolset field.
[[ -n "$OSX_AR" ]] && B2_USERCONFIG_EXTRA=": <archiver>$OSX_AR <ranlib>${OSX_RANLIB:-$OSX_AR}"

export TARGET GOOS GOARCH CC CXX B2_COMPILER B2_VARIANT B2_TOOLSET_CXX B2_FLAGS \
       B2_USERCONFIG_EXTRA

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
