#!/usr/bin/env bash
# Cross-build for android/arm64 (arm64-v8a) via the Android NDK (r26+; tested r29).
#
# Prereqs: an unpacked NDK and ANDROID_NDK_HOME pointing at it.
#   export ANDROID_NDK_HOME=/path/to/android-ndk-r29   # developer.android.com/ndk
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
# github.com/wlynxg/anet reaches net.zoneCache via //go:linkname; Go 1.23+
# rejects that by default. Its code only compiles on android, so the escape
# hatch is only needed here.
EXTRA_GO_LDFLAGS="-checklinkname=0"
# NDK clang++ links libc++_shared.so by default, but the TorrServe app only
# ships the bare binary — no libc++_shared alongside — so the executable dies
# at load time ("cannot locate symbol ... __ndk1..."). Link libc++ statically.
EXTRA_CGO_LDFLAGS="-static-libstdc++"
# OpenSSL's android-* Configure targets need ANDROID_NDK_ROOT + the toolchain
# on PATH; the API level define matches minSdk above.
ANDROID_NDK_ROOT="$ANDROID_NDK_HOME"
OPENSSL_PATH="$TC/bin"
OPENSSL_EXTRA_ARGS="-D__ANDROID_API__=${API}"
# usrsctp/libjuice: cross via the NDK's own cmake toolchain file.
CMAKE_CROSS_ARGS="-DCMAKE_TOOLCHAIN_FILE=$ANDROID_NDK_HOME/build/cmake/android.toolchain.cmake -DANDROID_ABI=arm64-v8a -DANDROID_PLATFORM=android-${API}"

export TARGET GOOS GOARCH CC CXX B2_COMPILER B2_VARIANT B2_TOOLSET_CXX B2_FLAGS \
       EXTRA_GO_LDFLAGS EXTRA_CGO_LDFLAGS \
       ANDROID_NDK_ROOT OPENSSL_PATH OPENSSL_EXTRA_ARGS CMAKE_CROSS_ARGS

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
