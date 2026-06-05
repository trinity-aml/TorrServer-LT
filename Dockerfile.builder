# syntax=docker/dockerfile:1.7
#
# TorrServer-LT builder: produces a fully-static linux binary with libtorrent
# (arvidn) linked into the Go binary via cgo.
#
# Stages:
#   lt-build  — compiles libtorrent-rasterbar 2.x as a static archive
#   go-build  — compiles the Go binary with cgo, statically linking libtorrent
#               and all C/C++ deps (boost, openssl, zlib, libstdc++, musl)
#   final     — minimal scratch image with just the binary
#
# Stage 1 milestone: only proves the toolchain works end-to-end (lt.Version()).
# Real wiring of session/torrent/storage lands in later milestones.

ARG LT_TAG=v2.0.10
ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.20

############################
# Stage 1: build libtorrent
############################
FROM alpine:${ALPINE_VERSION} AS lt-build
ARG LT_TAG

RUN apk add --no-cache \
        build-base cmake git linux-headers \
        boost-dev boost-static \
        openssl-dev openssl-libs-static \
        zlib-dev zlib-static

WORKDIR /src
RUN git clone --branch ${LT_TAG} --depth 1 --recurse-submodules \
        https://github.com/arvidn/libtorrent.git

WORKDIR /src/libtorrent/build
RUN cmake .. \
        -DCMAKE_BUILD_TYPE=Release \
        -DBUILD_SHARED_LIBS=OFF \
        -Dstatic_runtime=ON \
        -Ddeprecated-functions=OFF \
        -Dbuild_examples=OFF \
        -Dbuild_tests=OFF \
        -Dpython-bindings=OFF \
        -DCMAKE_INSTALL_PREFIX=/opt/lt \
 && cmake --build . -j"$(nproc)" \
 && cmake --install .

############################
# Stage 2: build TorrServer-LT
############################
FROM golang:${GO_VERSION}-alpine AS go-build

RUN apk add --no-cache \
        build-base musl-dev pkgconfig git \
        boost-dev boost-static \
        openssl-dev openssl-libs-static \
        zlib-dev zlib-static

# libtorrent artifacts from stage 1
COPY --from=lt-build /opt/lt /opt/lt
ENV PKG_CONFIG_PATH=/opt/lt/lib/pkgconfig

WORKDIR /src
COPY . .

WORKDIR /src/server

ARG TS_VERSION=MatriX.LT-001

# CGO_ENABLED=1 + fully-static via -extldflags '-static'.
# pkg-config in lt.go resolves CXXFLAGS/LDFLAGS for libtorrent-rasterbar.
ENV CGO_ENABLED=1

# Gate the static binary build on the lt + torrstor test suites so a
# broken shim or piece-cache never ships. libtorrent is static
# (.a only under /opt/lt/lib) so the test binaries are fully
# self-contained.
RUN go test -count=1 -timeout 180s ./lt/ ./torr/ ./torr/storage/torrstor/

RUN go build \
      -tags 'osusergo netgo' \
      -ldflags "-s -w -X server/version.Version=${TS_VERSION} -linkmode external -extldflags '-static'" \
      -o /out/TorrServer-LT \
      ./cmd

############################
# Stage 3: final
############################
FROM alpine:${ALPINE_VERSION} AS final
COPY --from=go-build /out/TorrServer-LT /usr/local/bin/TorrServer-LT
ENTRYPOINT ["TorrServer-LT"]
