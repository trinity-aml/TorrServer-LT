#!/usr/bin/env bash
# Cross-build for linux/arm64 (aarch64).
# Toolchain: gcc-aarch64-linux-gnu g++-aarch64-linux-gnu
#   sudo apt install gcc-aarch64-linux-gnu g++-aarch64-linux-gnu

TARGET=linux-arm64
GOOS=linux GOARCH=arm64
CC=aarch64-linux-gnu-gcc
CXX=aarch64-linux-gnu-g++
B2_VARIANT=arm64
B2_TOOLSET_CXX=aarch64-linux-gnu-g++
B2_FLAGS="architecture=arm address-model=64"

export TARGET GOOS GOARCH CC CXX B2_VARIANT B2_TOOLSET_CXX B2_FLAGS

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
