#!/usr/bin/env bash
# Cross-build for android/arm64 (arm64-v8a) via the Android NDK (r26d).
#
# Prereqs: an unpacked NDK and ANDROID_NDK_HOME pointing at it.
#   export ANDROID_NDK_HOME=/opt/android-ndk-r26d   # developer.android.com/ndk
#
# minSdk = 21 (Android 5.0), matching the original TorrServer Android target.

: "${ANDROID_NDK_HOME:?set ANDROID_NDK_HOME to an unpacked Android NDK}"
API=21
TC="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64"

TARGET=android-arm64
GOOS=android GOARCH=arm64
CC="$TC/bin/aarch64-linux-android${API}-clang"
CXX="$TC/bin/aarch64-linux-android${API}-clang++"
B2_COMPILER=clang
B2_VARIANT=android64
B2_TOOLSET_CXX="$CXX"
B2_FLAGS="target-os=android address-model=64 architecture=arm"

export TARGET GOOS GOARCH CC CXX B2_COMPILER B2_VARIANT B2_TOOLSET_CXX B2_FLAGS

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
