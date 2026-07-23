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
SSL_SRC_DIR="$SRC_DIR/openssl-${OPENSSL_VERSION}"

# --- b2 --------------------------------------------------------------
# Bootstrap Boost's b2 once (host tool). It also makes $BOOST_DIR usable as
# BOOST_ROOT, from which libtorrent compiles boost_system for the target.
# The run-check (not just -x) matters in CI: the _src cache is shared between
# the ubuntu matrix and the macOS runners, so a restored tree can carry a b2
# binary for the OTHER OS — executable, but not runnable here. Re-bootstrap
# whenever the cached b2 can't actually run on this host.
ensure_b2() {
    # NB: `b2 --version` prints the banner but exits NON-zero (observed with
    # B2 5.1.0), so judge runnability by the banner text, not the exit code.
    if [[ ! -x "$BOOST_DIR/b2" ]] \
        || ! "$BOOST_DIR/b2" --version 2>/dev/null | grep -q '^B2 '; then
        log "bootstrapping host b2"
        ( cd "$BOOST_DIR" && ./bootstrap.sh >/dev/null )
    fi
}

# --- OpenSSL (static, per target) ------------------------------------
# Needed by crypto=openssl (https trackers/web seeds) and webtorrent (DTLS +
# wss:// signalling). Built out-of-tree from the shared _src/openssl tree.
#
# A target script may override the Configure target via OPENSSL_CONFIG_TARGET
# and/or set OPENSSL_CROSS_PREFIX (e.g. "x86_64-w64-mingw32-") so Configure
# picks the whole cross binutils family, not just $CC.
openssl_config_target() {
    if [[ -n "${OPENSSL_CONFIG_TARGET:-}" ]]; then
        echo "$OPENSSL_CONFIG_TARGET"
        return
    fi
    case "$TARGET" in
        linux-amd64)   echo linux-x86_64 ;;
        linux-arm64)   echo linux-aarch64 ;;
        linux-armv7)   echo linux-armv4 ;;
        windows-amd64) echo mingw64 ;;
        android-arm64) echo android-arm64 ;;
        android-armv7) echo android-arm ;;
        darwin-amd64)  echo darwin64-x86_64-cc ;;
        darwin-arm64)  echo darwin64-arm64-cc ;;
        *) die "no OpenSSL Configure target for $TARGET (set OPENSSL_CONFIG_TARGET)" ;;
    esac
}

build_openssl() {
    local deps="$1"
    if [[ -f "$deps/lib/libssl.a" && -f "$deps/lib/libcrypto.a" ]]; then
        log "OpenSSL already installed for $TARGET"
        return
    fi
    local cfg
    cfg=$(openssl_config_target)
    local sslbuild="$DEPS_ROOT/.ssl-build-$TARGET"
    rm -rf "$sslbuild"
    mkdir -p "$sslbuild"
    log "building OpenSSL $OPENSSL_VERSION for $TARGET ($cfg)"
    # no-apps/no-docs + install_dev: only the static libs and headers, no CLI
    # tool, no manpages.
    #
    # Compiler selection: either OPENSSL_CROSS_PREFIX (Configure prepends it
    # to its default cc AND to ar/ranlib) or the target env's CC. The target
    # scripts EXPORT CC, and Configure applies the prefix on top of whatever
    # CC it sees — so with a prefix, CC must be scrubbed from the env
    # (env -u), not merely left unset here. OPENSSL_PATH is prepended to PATH
    # (Android NDK toolchain bin), OPENSSL_EXTRA_ARGS is appended verbatim
    # (e.g. -D__ANDROID_API__=21).
    local cc_env=(CC="$CC")
    [[ -n "${OPENSSL_CROSS_PREFIX:-}" ]] && cc_env=(-u CC)
    # Mach-O archives can't be made by host GNU ar: darwin cross targets pass
    # the osxcross ar/ranlib explicitly (Configure honours AR/RANLIB env).
    [[ -n "${OPENSSL_AR:-}" ]] && cc_env+=(AR="$OPENSSL_AR")
    [[ -n "${OPENSSL_RANLIB:-}" ]] && cc_env+=(RANLIB="$OPENSSL_RANLIB")
    # shellcheck disable=SC2086
    ( cd "$sslbuild" && \
      export PATH="${OPENSSL_PATH:+$OPENSSL_PATH:}$PATH" && \
      env "${cc_env[@]}" "$SSL_SRC_DIR/Configure" "$cfg" \
        ${OPENSSL_CROSS_PREFIX:+--cross-compile-prefix="$OPENSSL_CROSS_PREFIX"} \
        no-shared no-tests no-apps no-docs \
        ${OPENSSL_EXTRA_ARGS:-} \
        --prefix="$deps" --libdir=lib >/dev/null && \
      make -s -j"$JOBS" build_libs && \
      make -s install_dev >/dev/null )
}

# --- webtorrent C deps (usrsctp + libjuice) via cmake ----------------
# libdatachannel's Jamfile builds these by shelling out to a bare
# `cmake .. && make` that ignores the b2 toolset — i.e. it would use the HOST
# compiler, producing wrong-arch archives on every cross target. Build them
# here with explicit cross parameters instead; build_libtorrent() then patches
# that Jamfile to just copy these archives.
build_webrtc_deps() {
    local deps="$1"
    if [[ -f "$deps/lib/libusrsctp.a" && -f "$deps/lib/libjuice-openssl.a" ]]; then
        log "webrtc deps already installed for $TARGET"
        return
    fi
    local dc="$LT_DIR/deps/libdatachannel"
    [[ -f "$dc/deps/usrsctp/CMakeLists.txt" && -f "$dc/deps/libjuice/CMakeLists.txt" ]] \
        || die "libdatachannel submodules missing — re-run build/_fetch_sources.sh"

    # Cross parameters for cmake. A target script may supply the whole set via
    # CMAKE_CROSS_ARGS (e.g. the NDK's android.toolchain.cmake — which must
    # not be mixed with explicit CMAKE_C_COMPILER); otherwise: linux native
    # needs none, the other targets get CMAKE_SYSTEM_NAME (which flips cmake
    # into cross mode) + the target compiler.
    local cross=()
    if [[ -n "${CMAKE_CROSS_ARGS:-}" ]]; then
        # shellcheck disable=SC2206
        cross=($CMAKE_CROSS_ARGS)
    else
        cross=(-DCMAKE_C_COMPILER="$CC")
        case "$GOOS" in
            windows) cross+=(-DCMAKE_SYSTEM_NAME=Windows -DCMAKE_SYSTEM_PROCESSOR=x86_64) ;;
            android) cross+=(-DCMAKE_SYSTEM_NAME=Android) ;;
            darwin)  [[ "$(uname -s)" == "Darwin" ]] || cross+=(-DCMAKE_SYSTEM_NAME=Darwin) ;;
        esac
    fi

    mkdir -p "$deps/lib"

    local b="$DEPS_ROOT/.usrsctp-build-$TARGET"
    rm -rf "$b" && mkdir -p "$b"
    log "building usrsctp for $TARGET"
    ( cd "$b" && cmake "${cross[@]}" \
        -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_FLAGS="-fPIC" \
        -Dsctp_werror=0 -Dsctp_build_shared_lib=0 -Dsctp_build_programs=0 \
        -Dsctp_inet=0 -Dsctp_inet6=0 \
        "$dc/deps/usrsctp" >/dev/null && \
      make -s -j"$JOBS" usrsctp )
    cp "$b/usrsctplib/libusrsctp.a" "$deps/lib/libusrsctp.a"

    local j="$DEPS_ROOT/.juice-build-$TARGET"
    rm -rf "$j" && mkdir -p "$j"
    log "building libjuice for $TARGET"
    ( cd "$j" && cmake "${cross[@]}" \
        -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_FLAGS="-fPIC" \
        -DUSE_NETTLE=0 -DOPENSSL_ROOT_DIR="$deps" -DOPENSSL_USE_STATIC_LIBS=ON \
        "$dc/deps/libjuice" >/dev/null && \
      make -s -j"$JOBS" juice-static )
    # The name libdatachannel's Jamfile links against (<gnutls>off flavour).
    cp "$j/libjuice-static.a" "$deps/lib/libjuice-openssl.a"
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
    # B2_USERCONFIG_EXTRA lets a target append toolset options (4th `using`
    # field), e.g. a Mach-O archiver/ranlib for darwin cross. It must start with
    # ": " when set. Empty for the gcc/clang targets that use the default ar.
    printf 'using %s : %s : %s %s ;\n' \
        "$compiler" "$B2_VARIANT" "$B2_TOOLSET_CXX" "${B2_USERCONFIG_EXTRA:-}" > "$jam"

    # b2's generate-pkg-config writes libtorrent-rasterbar.pc into the CWD
    # ($LT_DIR). Running two targets from the same CWD races on that file (a
    # parallel windows build can clobber an arm build's .pc). Give each target
    # its own hardlinked copy of the source tree — instant, ~no disk, fully
    # isolated CWD, and parallel builds stay correct. BSD cp (a native macOS
    # runner) has no -l; APFS clonefile (-c) is the same near-free copy there.
    local ltwork="$DEPS_ROOT/.lt-src-$TARGET"
    rm -rf "$ltwork"
    if [[ "$(uname -s)" == "Darwin" ]]; then
        cp -cR "$LT_DIR" "$ltwork" 2>/dev/null || cp -R "$LT_DIR" "$ltwork"
    else
        cp -al "$LT_DIR" "$ltwork"
    fi

    # Rewrite libdatachannel's usrsctp/libjuice build actions to copy the
    # archives cross-built by build_webrtc_deps() instead of running its own
    # host-compiler cmake (see that function). python writes a NEW file and
    # os.replace()s it so the hardlinked _src tree is never touched.
    python3 - "$ltwork/deps/libdatachannel/Jamfile" "$deps" <<'PYEOF'
import os, re, sys
path, deps = sys.argv[1], sys.argv[2]
with open(path) as f:
    src = f.read()
def graft(action, lib):
    global src
    pat = re.compile(r'(actions %s\n\{\n).*?(\n\})' % re.escape(action), re.S)
    out, n = pat.subn(lambda m: m.group(1) + '    cp "%s/lib/%s" $(<)' % (deps, lib) + m.group(2), src, count=1)
    if n != 1:
        sys.exit("patch failed: actions %s not found in %s" % (action, path))
    src = out
graft('make_libusrsctp', 'libusrsctp.a')
graft('make_libjuice_openssl', 'libjuice-openssl.a')
tmp = path + '.tsl'
with open(tmp, 'w') as f:
    f.write(src)
os.replace(tmp, path)
PYEOF

    log "building libtorrent for $TARGET (toolset $compiler-$B2_VARIANT, crypto=openssl, webtorrent=on)"
    # shellcheck disable=SC2086
    ( cd "$ltwork" && BOOST_ROOT="$BOOST_DIR" "$BOOST_DIR/b2" -q -j"$JOBS" \
        --user-config="$jam" \
        --prefix="$deps" \
        --build-dir="$DEPS_ROOT/.lt-build-$TARGET" \
        toolset="$compiler"-"$B2_VARIANT" \
        link=static crypto=openssl webtorrent=on \
        openssl-lib="$deps/lib" openssl-include="$deps/include" \
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
    # sed -i needs a suffix argument on BSD sed (native macOS runner); the
    # -i.bak + rm form is the portable spelling for both GNU and BSD.
    local pc="$deps/lib/pkgconfig/libtorrent-rasterbar.pc"
    sed -i.bak -E 's/-llib([A-Za-z0-9_-]+)\.a/-l\1/g' "$pc"
    local privlibs
    privlibs=$(grep -E '^Libs\.private:' "$pc" | grep -oE -- '-l[A-Za-z0-9_-]+' | tr '\n' ' ')
    if [[ -n "$privlibs" ]]; then
        sed -i.bak -E "s#^(Libs:.*)#\1 ${privlibs}#" "$pc"
    fi
    rm -f "$pc.bak"

    #  3. Static archives are position-sensitive for a single-pass linker:
    #     users first, their dependencies after. b2 emits -lssl -lcrypto right
    #     after -ltorrent-rasterbar, BEFORE the folded private libs that also
    #     need them (libdatachannel, juice). Normalise the tail of the Libs
    #     line to: ... -ldatachannel -lusrsctp -ljuice-openssl -lssl -lcrypto
    #     (datachannel pulls sctp/juice/ssl; juice and ssl pull crypto). b2
    #     installs the archive as libdatachannel.a, hence -ldatachannel; the
    #     -llibdatachannel spelling is dropped too in case a .pc from an older
    #     fixup is being re-processed.
    awk 'BEGIN {
             drop["-lssl"]; drop["-lcrypto"]; drop["-lusrsctp"];
             drop["-ljuice-openssl"]; drop["-ldatachannel"]; drop["-llibdatachannel"];
             # windows spellings: the Jamfile declares OpenSSL as
             # <name>libssl/<name>libcrypto there (MSVC naming); our archives
             # are libssl.a/libcrypto.a on every target.
             drop["-llibssl"]; drop["-llibcrypto"];
         }
         /^Libs:/ {
             out = "";
             for (i = 1; i <= NF; i++) if (!($i in drop)) out = out " " $i;
             sub(/^ /, "", out);
             print out " -ldatachannel -lusrsctp -ljuice-openssl -lssl -lcrypto";
             next;
         }
         { print }' "$pc" > "$pc.tsl" && mv "$pc.tsl" "$pc"
}

# --- Go binary -------------------------------------------------------
# gst_variant_wanted: platforms that get a SECOND binary with the GStreamer
# transcoding feature compiled in (-tags gst) — the ones a GStreamer runtime
# exists for, mirroring upstream build-all.sh's GST_PLATFORMS. The feature is
# pure Go (purego dlopen at runtime, no build-time linkage), so the variant
# differs only by the build tag; other targets ship the stub-only base binary.
gst_variant_wanted() {
    case "$GOOS/$GOARCH" in
        linux/amd64 | linux/arm64 | windows/amd64 | darwin/amd64 | darwin/arm64) return 0 ;;
        *) return 1 ;;
    esac
}

go_build() {
    local deps="$1"
    local out="$OUT_DIR/TorrServer-LT-${TARGET}${BINEXT:-}"
    # -gst suffix BEFORE the .exe extension; the suffix form keeps the CI
    # artifact globs (TorrServer-LT-<target>*) picking both variants up.
    local out_gst="$OUT_DIR/TorrServer-LT-${TARGET}-gst${BINEXT:-}"

    # Go caches a cgo package's resolved pkg-config flags keyed on the cgo
    # source + CGO_* env, NOT on the .pc file content. So a regenerated/changed
    # libtorrent-rasterbar.pc is silently ignored on rebuild. Fold a hash of the
    # .pc into CGO_CFLAGS as a harmless unused -D define: the stamp changes with
    # the .pc, busting the cache exactly when needed (and a cache hit otherwise).
    local pc_stamp
    pc_stamp=$( { cat "$deps/lib/pkgconfig/libtorrent-rasterbar.pc"; echo "${EXTRA_CGO_LDFLAGS:-}"; } | sha1sum | cut -c1-12 )

    log "go build ($TARGET)"
    cd "$ROOT/server"
    # one_build <output> [extra go build args...] — both variants share the
    # exact same cgo env, so the second build reuses the Go build cache for
    # everything except the gst-gated packages (near-free).
    one_build() {
        local target_out="$1"
        shift
        env \
            CGO_ENABLED=1 \
            GOOS="$GOOS" GOARCH="$GOARCH" ${GOARM:+GOARM="$GOARM"} \
            CC="$CC" CXX="$CXX" \
            PKG_CONFIG_PATH="$deps/lib/pkgconfig" \
            PKG_CONFIG_LIBDIR="$deps/lib/pkgconfig" \
            CGO_CFLAGS="-DTS_PC_STAMP=$pc_stamp" \
            CGO_CXXFLAGS="-DTS_PC_STAMP=$pc_stamp -DTSL_HAVE_LT_INTERNALS" \
            CGO_LDFLAGS="-L$deps/lib ${EXTRA_CGO_LDFLAGS:-}" \
            go build \
            "$@" \
            -ldflags "-s -w ${EXTRA_GO_LDFLAGS:-} -X server/version.Version=${TS_VERSION}" \
            -o "$target_out" \
            ./cmd
        file "$target_out"
        du -h "$target_out"
    }

    one_build "$out"

    # Second binary WITH the GStreamer transcoding feature for the platforms a
    # GStreamer runtime exists on (see gst_variant_wanted). -tags gst compiles
    # in server/gstreamer; the GStreamer libraries are dlopen'd at RUNTIME via
    # purego, so no headers/libs are needed here and nothing extra is linked —
    # the variant merely enables the feature for users who install GStreamer.
    if gst_variant_wanted; then
        log "go build ($TARGET, gst variant)"
        one_build "$out_gst" -tags gst
    fi
}

# --- orchestrator ----------------------------------------------------
cross_build() {
    local deps="$DEPS_ROOT/$TARGET"
    # Stale-tree guard: the per-function "already installed" early-returns
    # only check file presence, so a _deps tree built from an older pin
    # (2.0.13, no OpenSSL) would silently survive a version bump. Stamp the
    # tree with the exact recipe and nuke it on mismatch.
    local stamp="$deps/.stamp"
    local want="lt=$LIBTORRENT_TAG openssl=$OPENSSL_VERSION webtorrent=on"
    if [[ -e "$deps" && "$(cat "$stamp" 2>/dev/null || true)" != "$want" ]]; then
        log "deps tree for $TARGET is stale (want: $want) — rebuilding"
        rm -rf "$deps"
    fi
    mkdir -p "$deps"
    # The stamp records the RECIPE this tree is (being) built with, so it's
    # written up front: a failed run resumes from the per-function
    # "already installed" guards instead of being nuked and restarted.
    echo "$want" > "$stamp"
    fetch_sources
    ensure_b2
    build_openssl "$deps"
    build_webrtc_deps "$deps"
    build_libtorrent "$deps"
    go_build "$deps"
    log "done: $OUT_DIR/TorrServer-LT-${TARGET}${BINEXT:-}"
}
