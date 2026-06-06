# shellcheck shell=bash
# Shared cross-build engine: builds libtorrent (+ boost_system from source)
# for one target into _deps/<TARGET>/ using Boost.Build (b2) — no cmake — then
# links the Go binary against it via pkg-config.
#
# libtorrent's own Jamfile builds boost_system straight from the Boost source
# tree when BOOST_ROOT is set, and its `install` target emits a correct
# libtorrent-rasterbar.pc (right Cflags/defines/Libs for the actual build).
# The cgo package (server/lt) consumes it through:
#   #cgo pkg-config: libtorrent-rasterbar
# so we just point PKG_CONFIG_LIBDIR at our per-target tree.
#
# A caller (build/<target>.sh) must export before invoking `cross_build`:
#
#   TARGET                 logical name, e.g. linux-arm64
#   GOOS GOARCH            go cross target (plus GOARM for armv7)
#   CC CXX                 cross C / C++ compilers (full names on PATH)
#   B2_COMPILER            b2 compiler family: gcc | clang        (default gcc)
#   B2_VARIANT             user-config toolset suffix, e.g. arm64
#   B2_TOOLSET_CXX         g++/clang++ b2 should drive (usually == $CXX)
#   B2_FLAGS               extra b2 props, e.g. "architecture=arm address-model=64"
#
# Optional:
#   EXTRA_CGO_LDFLAGS      appended to CGO_LDFLAGS for the final go build
#   BINEXT                 binary suffix, e.g. .exe

# shellcheck source=_common.sh
. "$(dirname "${BASH_SOURCE[0]}")/_common.sh"
fetch_sources() { bash "$(dirname "${BASH_SOURCE[0]}")/_fetch_sources.sh"; }

BOOST_UND=$(echo "$BOOST_VERSION" | tr . _)
BOOST_DIR="$SRC_DIR/boost_${BOOST_UND}"
LT_DIR="$SRC_DIR/libtorrent"

# --- b2 --------------------------------------------------------------
# Bootstrap Boost's b2 once (host tool). It also makes $BOOST_DIR usable as
# BOOST_ROOT, from which libtorrent compiles boost_system for the target.
ensure_b2() {
    if [[ ! -x "$BOOST_DIR/b2" ]]; then
        log "bootstrapping host b2"
        ( cd "$BOOST_DIR" && ./bootstrap.sh >/dev/null )
    fi
}

# --- libtorrent (+ boost_system) via b2 ------------------------------
build_libtorrent() {
    local deps="$1"
    if [[ -f "$deps/lib/pkgconfig/libtorrent-rasterbar.pc" ]]; then
        log "libtorrent already installed for $TARGET"
        return
    fi

    local compiler="${B2_COMPILER:-gcc}"
    local jam="$deps/user-config.jam"
    printf 'using %s : %s : %s ;\n' "$compiler" "$B2_VARIANT" "$B2_TOOLSET_CXX" > "$jam"

    # b2's generate-pkg-config writes libtorrent-rasterbar.pc into the CWD
    # ($LT_DIR). Running two targets from the same CWD races on that file (a
    # parallel windows build can clobber an arm build's .pc). Give each target
    # its own hardlinked copy of the source tree — instant, ~no disk, fully
    # isolated CWD, and parallel builds stay correct.
    local ltwork="$DEPS_ROOT/.lt-src-$TARGET"
    rm -rf "$ltwork"
    cp -al "$LT_DIR" "$ltwork"

    log "building libtorrent for $TARGET (toolset $compiler-$B2_VARIANT, crypto=built-in)"
    # shellcheck disable=SC2086
    ( cd "$ltwork" && BOOST_ROOT="$BOOST_DIR" "$BOOST_DIR/b2" -q -j"$JOBS" \
        --user-config="$jam" \
        --prefix="$deps" \
        --build-dir="$DEPS_ROOT/.lt-build-$TARGET" \
        toolset="$compiler"-"$B2_VARIANT" \
        link=static crypto=built-in \
        cxxstd=17 \
        variant=release threading=multi \
        $B2_FLAGS \
        install )

    # Fix up libtorrent's generated pkg-config for static cgo linking:
    #  1. It lists the static deps it built from source as -llibboost_system.a /
    #     -llibtry_signal.a (the full filename) — no linker resolves that.
    #     Rewrite -llibNAME.a -> -lNAME so -L<libdir> finds them.
    #  2. Those deps live in Libs.private, which pkg-config only emits with
    #     --static. cgo calls pkg-config WITHOUT --static, so fold them into
    #     Libs (after -ltorrent-rasterbar, the order static archives need).
    local pc="$deps/lib/pkgconfig/libtorrent-rasterbar.pc"
    sed -i -E 's/-llib([A-Za-z0-9_]+)\.a/-l\1/g' "$pc"
    local privlibs
    privlibs=$(grep -E '^Libs\.private:' "$pc" | grep -oE -- '-l[A-Za-z0-9_]+' | tr '\n' ' ')
    if [[ -n "$privlibs" ]]; then
        sed -i -E "s#^(Libs:.*)#\1 ${privlibs}#" "$pc"
    fi
}

# --- Go binary -------------------------------------------------------
go_build() {
    local deps="$1"
    local out="$OUT_DIR/TorrServer-LT-${TARGET}${BINEXT:-}"

    # Go caches a cgo package's resolved pkg-config flags keyed on the cgo
    # source + CGO_* env, NOT on the .pc file content. So a regenerated/changed
    # libtorrent-rasterbar.pc is silently ignored on rebuild. Fold a hash of the
    # .pc into CGO_CFLAGS as a harmless unused -D define: the stamp changes with
    # the .pc, busting the cache exactly when needed (and a cache hit otherwise).
    local pc_stamp
    pc_stamp=$( { cat "$deps/lib/pkgconfig/libtorrent-rasterbar.pc"; echo "${EXTRA_CGO_LDFLAGS:-}"; } | sha1sum | cut -c1-12 )

    log "go build ($TARGET)"
    cd "$ROOT/server"
    env \
        CGO_ENABLED=1 \
        GOOS="$GOOS" GOARCH="$GOARCH" ${GOARM:+GOARM="$GOARM"} \
        CC="$CC" CXX="$CXX" \
        PKG_CONFIG_PATH="$deps/lib/pkgconfig" \
        PKG_CONFIG_LIBDIR="$deps/lib/pkgconfig" \
        CGO_CFLAGS="-DTS_PC_STAMP=$pc_stamp" \
        CGO_CXXFLAGS="-DTS_PC_STAMP=$pc_stamp" \
        CGO_LDFLAGS="-L$deps/lib ${EXTRA_CGO_LDFLAGS:-}" \
        go build \
        -ldflags "-s -w -X server/version.Version=${TS_VERSION}" \
        -o "$out" \
        ./cmd

    file "$out"
    du -h "$out"
}

# --- orchestrator ----------------------------------------------------
cross_build() {
    local deps="$DEPS_ROOT/$TARGET"
    mkdir -p "$deps"
    fetch_sources
    ensure_b2
    build_libtorrent "$deps"
    go_build "$deps"
    log "done: $OUT_DIR/TorrServer-LT-${TARGET}${BINEXT:-}"
}
