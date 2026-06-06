#!/usr/bin/env bash
# Cross-build for darwin/arm64 (Apple Silicon) from Linux via OSXCross.
#
# ⚠️  LEGAL: OSXCross needs the Apple macOS SDK, whose licence only permits
#     use on Apple-branded hardware. Cross-building macOS binaries on a Linux
#     host is a grey area — for distribution, build on a real Mac / CI macOS
#     runner (see .github/workflows/build-macos.yml). This script exists so
#     "all architectures locally" is reproducible where that SDK is available.
#
# Prereqs: a built OSXCross tree and OSXCROSS_ROOT pointing at it.
#   export OSXCROSS_ROOT=/opt/osxcross

: "${OSXCROSS_ROOT:?set OSXCROSS_ROOT to a built OSXCross tree}"
export PATH="$OSXCROSS_ROOT/target/bin:$PATH"

TARGET=darwin-arm64
GOOS=darwin GOARCH=arm64
CC=oa64-clang
CXX=oa64-clang++
B2_COMPILER=clang
B2_VARIANT=darwin_arm64
B2_TOOLSET_CXX=oa64-clang++
B2_FLAGS="target-os=darwin address-model=64 architecture=arm"

export TARGET GOOS GOARCH CC CXX B2_COMPILER B2_VARIANT B2_TOOLSET_CXX B2_FLAGS

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
