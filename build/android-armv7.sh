#!/usr/bin/env bash
# Cross-build for android/arm (armeabi-v7a) via the Android NDK (r26+; tested r29).
#
# Prereqs: an unpacked NDK and ANDROID_NDK_HOME pointing at it.
#   export ANDROID_NDK_HOME=/path/to/android-ndk-r29
#
# Note the NDK clang triple for 32-bit arm is armv7a-linux-androideabi.

: "${ANDROID_NDK_HOME:?set ANDROID_NDK_HOME to an unpacked Android NDK}"
API=21
TC="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64"

TARGET=android-armv7
GOOS=android GOARCH=arm GOARM=7
CC="$TC/bin/armv7a-linux-androideabi${API}-clang"
CXX="$TC/bin/armv7a-linux-androideabi${API}-clang++"
B2_COMPILER=clang
B2_VARIANT=android32
B2_TOOLSET_CXX="$CXX"
B2_FLAGS="target-os=android address-model=32 architecture=arm"
# See android-arm64.sh: anet's //go:linkname needs Go's checklinkname off.
EXTRA_GO_LDFLAGS="-checklinkname=0"
# See android-arm64.sh: link libc++ statically, the app ships no libc++_shared.
EXTRA_CGO_LDFLAGS="-static-libstdc++"

export TARGET GOOS GOARCH GOARM CC CXX B2_COMPILER B2_VARIANT B2_TOOLSET_CXX \
       B2_FLAGS EXTRA_GO_LDFLAGS EXTRA_CGO_LDFLAGS

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
