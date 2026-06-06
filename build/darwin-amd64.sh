#!/usr/bin/env bash
# Cross-build for darwin/amd64 (Intel Mac) from Linux via OSXCross.
#
# ⚠️  LEGAL: see build/darwin-arm64.sh — the Apple SDK licence restricts this
#     to Apple hardware; for distribution prefer a macOS CI runner.
#
# Prereqs: a built OSXCross tree and OSXCROSS_ROOT pointing at it.
#   export OSXCROSS_ROOT=/opt/osxcross

: "${OSXCROSS_ROOT:?set OSXCROSS_ROOT to a built OSXCross tree}"
export PATH="$OSXCROSS_ROOT/target/bin:$PATH"

TARGET=darwin-amd64
GOOS=darwin GOARCH=amd64
CC=o64-clang
CXX=o64-clang++
B2_COMPILER=clang
B2_VARIANT=darwin_amd64
B2_TOOLSET_CXX=o64-clang++
B2_FLAGS="target-os=darwin address-model=64 architecture=x86"

export TARGET GOOS GOARCH CC CXX B2_COMPILER B2_VARIANT B2_TOOLSET_CXX B2_FLAGS

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
