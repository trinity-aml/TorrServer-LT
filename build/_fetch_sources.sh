#!/usr/bin/env bash
# Download + extract Boost and OpenSSL and clone libtorrent once, into _src/.
# Idempotent: re-runs are no-ops once the trees are present.

# shellcheck source=_common.sh
. "$(dirname "$0")/_common.sh"

# --- Boost -----------------------------------------------------------
BOOST_UND=$(echo "$BOOST_VERSION" | tr . _)
BOOST_DIR="$SRC_DIR/boost_${BOOST_UND}"
BOOST_TAR="$SRC_DIR/boost_${BOOST_UND}.tar.bz2"

if [[ ! -d "$BOOST_DIR" ]]; then
    if [[ ! -f "$BOOST_TAR" ]]; then
        log "downloading Boost ${BOOST_VERSION}"
        curl -fL --retry 3 -o "$BOOST_TAR" \
            "https://archives.boost.io/release/${BOOST_VERSION}/source/boost_${BOOST_UND}.tar.bz2"
    fi
    log "extracting Boost"
    tar -xjf "$BOOST_TAR" -C "$SRC_DIR"
else
    log "Boost ${BOOST_VERSION} already in $BOOST_DIR"
fi

# --- OpenSSL ---------------------------------------------------------
# Static per-target OpenSSL for crypto=openssl + webtorrent (built by
# build_openssl in _deps.sh from this shared source tree).
SSL_DIR="$SRC_DIR/openssl-${OPENSSL_VERSION}"
SSL_TAR="$SRC_DIR/openssl-${OPENSSL_VERSION}.tar.gz"

if [[ ! -d "$SSL_DIR" ]]; then
    if [[ ! -f "$SSL_TAR" ]]; then
        log "downloading OpenSSL ${OPENSSL_VERSION}"
        curl -fL --retry 3 -o "$SSL_TAR" \
            "https://github.com/openssl/openssl/releases/download/openssl-${OPENSSL_VERSION}/openssl-${OPENSSL_VERSION}.tar.gz"
    fi
    log "extracting OpenSSL"
    tar -xzf "$SSL_TAR" -C "$SRC_DIR"
else
    log "OpenSSL ${OPENSSL_VERSION} already in $SSL_DIR"
fi

# --- libtorrent ------------------------------------------------------
# libtorrent vendors its deps (try_signal, libdatachannel, ...) as submodules
# under deps/; the b2 build needs them present, including libdatachannel's own
# nested submodules (usrsctp, libjuice, plog) for webtorrent. --shallow keeps
# the recursive clone from dragging in full histories.
LT_DIR="$SRC_DIR/libtorrent"
if [[ ! -d "$LT_DIR" ]]; then
    log "cloning libtorrent $LIBTORRENT_TAG"
    git clone --branch "$LIBTORRENT_TAG" --depth 1 --recurse-submodules \
        --shallow-submodules \
        https://github.com/arvidn/libtorrent.git "$LT_DIR"
else
    # An existing clone may sit on an older pin (LIBTORRENT_TAG was bumped):
    # verify HEAD matches the tag and switch if it doesn't, so a version bump
    # in _common.sh actually takes effect on cached/local trees.
    have=$(git -C "$LT_DIR" describe --tags --exact-match 2>/dev/null || echo none)
    if [[ "$have" == "$LIBTORRENT_TAG" ]]; then
        log "libtorrent already at $LIBTORRENT_TAG in $LT_DIR"
    else
        log "switching libtorrent $have -> $LIBTORRENT_TAG"
        git -C "$LT_DIR" fetch --depth 1 origin "refs/tags/$LIBTORRENT_TAG:refs/tags/$LIBTORRENT_TAG"
        git -C "$LT_DIR" checkout -f "$LIBTORRENT_TAG"
        git -C "$LT_DIR" submodule update --init --recursive --depth 1
    fi
fi
# Always make sure submodules are checked out (a bare clone won't have them).
# Check both try_signal and libdatachannel's NESTED submodules — a tree cloned
# before the 2.1 bump (or with non-recursive init) has the former but not the
# latter, and the webtorrent build needs usrsctp/libjuice/plog present.
if [[ ! -f "$LT_DIR/deps/try_signal/Jamfile" ]] \
    || { [[ -f "$LT_DIR/deps/libdatachannel/Jamfile" ]] \
         && [[ ! -f "$LT_DIR/deps/libdatachannel/deps/libjuice/CMakeLists.txt" ]]; }; then
    log "initialising libtorrent submodules"
    git -C "$LT_DIR" submodule update --init --recursive --depth 1
fi

log "sources ready in $SRC_DIR"
