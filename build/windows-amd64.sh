#!/usr/bin/env bash
# Cross-build for windows/amd64 via MinGW-w64.
# Toolchain: gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64
#   sudo apt install gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64
#
# Produces a statically-linked .exe; the MinGW runtime is linked in so the
# binary needs no libstdc++/libgcc DLLs alongside it.

TARGET=windows-amd64
BINEXT=.exe
GOOS=windows GOARCH=amd64
CC=x86_64-w64-mingw32-gcc
CXX=x86_64-w64-mingw32-g++
B2_VARIANT=mingw64
B2_TOOLSET_CXX=x86_64-w64-mingw32-g++
B2_FLAGS="target-os=windows address-model=64 architecture=x86"
# libtorrent statically pulls in winsock/iphlpapi/crypto; list them for cgo,
# and link the gcc/stdc++ runtime statically so no MinGW DLLs are required.
# bcrypt/advapi32/user32/shell32/gdi32: static OpenSSL 3 + libdatachannel
# (see the win32 lib list in libdatachannel's Jamfile).
EXTRA_CGO_LDFLAGS="-static -static-libgcc -static-libstdc++ -lws2_32 -liphlpapi -lmswsock -lsecur32 -lcrypt32 -lbcrypt -ladvapi32 -luser32 -lshell32 -lgdi32"
# OpenSSL: the prefix makes Configure pick the whole mingw binutils family
# (gcc/ar/ranlib/windres), not just $CC.
OPENSSL_CROSS_PREFIX=x86_64-w64-mingw32-

export TARGET BINEXT GOOS GOARCH CC CXX B2_VARIANT B2_TOOLSET_CXX B2_FLAGS \
       EXTRA_CGO_LDFLAGS OPENSSL_CROSS_PREFIX

# shellcheck source=_deps.sh
. "$(dirname "$0")/_deps.sh"
cross_build
