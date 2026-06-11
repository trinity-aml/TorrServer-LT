#!/usr/bin/env bash
# Download + extract Boost and clone libtorrent once, into _src/.
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

# --- libtorrent ------------------------------------------------------
# libtorrent vendors its deps (try_signal, libsimulator, ...) as submodules
# under deps/; the b2 build needs them present.
LT_DIR="$SRC_DIR/libtorrent"
if [[ ! -d "$LT_DIR" ]]; then
    log "cloning libtorrent $LIBTORRENT_TAG"
    git clone --branch "$LIBTORRENT_TAG" --depth 1 --recurse-submodules \
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
if [[ ! -f "$LT_DIR/deps/try_signal/Jamfile" ]]; then
    log "initialising libtorrent submodules"
    git -C "$LT_DIR" submodule update --init --recursive --depth 1
fi

log "sources ready in $SRC_DIR"
