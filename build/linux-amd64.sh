#!/usr/bin/env bash
# Build for linux/amd64 with libtorrent statically linked in — same b2 engine
# as every other target, using the host's native gcc as the b2 toolset. No
# dependency on a host libtorrent-rasterbar-dev package; the binary is
# self-contained (only libc/libstdc++/libgcc are dynamic).

TARGET=linux-amd64
GOOS=linux GOARCH=amd64
CC=gcc
CXX=g++
B2_VARIANT=amd64
B2_TOOLSET_CXX=g++
B2_FLAGS="architecture=x86 address-model=64"

export TARGET GOOS GOARCH CC CXX B2_VARIANT B2_TOOLSET_CXX B2_FLAGS

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
