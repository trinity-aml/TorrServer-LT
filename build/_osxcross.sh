# shellcheck shell=bash
# Locate an OSXCross toolchain and resolve the clang wrappers + Mach-O binutils
# for a given arch. Supports both layouts:
#   * source build (tpoechtrager/osxcross): $OSXCROSS_ROOT/target/bin
#   * crazymax/osxcross image tree:          $OSXCROSS_ROOT/bin
#     (extract with: docker create --name oxc crazymax/osxcross:13.1-ubuntu
#                    docker cp oxc:/osxcross /opt/osxcross
#                    docker cp oxc:/osxsdk   /opt/osxcross/SDK )
#
# osxcross_setup <arm64|x86_64> exports, for that arch:
#   OSX_CC OSX_CXX      clang / clang++ wrapper
#   OSX_AR OSX_RANLIB   Mach-O archiver / ranlib (so b2 doesn't use host GNU ar)
# and prepends the toolchain bin/ to PATH plus points it at the SDK.

# shellcheck source=_common.sh
. "$(dirname "${BASH_SOURCE[0]}")/_common.sh"

# Pick the short-named tool (o64-ar) if present, else the newest triple-named
# one (x86_64-apple-darwin23-ar). Echoes the basename, or nothing if absent
# (never fails — callers treat ar/ranlib as optional). Uses globs, not ls|grep,
# so a missing match can't trip pipefail/errexit.
_osx_tool() {
    local bin="$1" arch="$2" short="$3" suffix="$4"  # suffix: clang, clang++, ar, ranlib
    if [[ -x "$bin/${short}-${suffix}" ]]; then
        printf '%s\n' "${short}-${suffix}"
        return 0
    fi
    local f best=""
    for f in "$bin/${arch}-apple-darwin"*"-${suffix}"; do
        [[ -e "$f" ]] || continue                       # literal glob (no match)
        [[ -z "$best" || "${f##*/}" > "$best" ]] && best="${f##*/}"
    done
    [[ -n "$best" ]] && printf '%s\n' "$best"
    return 0
}

osxcross_setup() {
    local arch="$1"   # arm64 | x86_64
    : "${OSXCROSS_ROOT:?set OSXCROSS_ROOT to an OSXCross tree (source build or crazymax/osxcross)}"

    local bin=""
    local cand
    for cand in "$OSXCROSS_ROOT/target/bin" "$OSXCROSS_ROOT/bin"; do
        [[ -d "$cand" ]] && { bin="$cand"; break; }
    done
    [[ -n "$bin" ]] || die "no bin/ under $OSXCROSS_ROOT (looked in target/bin and bin)"
    export PATH="$bin:$PATH"

    # The cctools linker (ld64) is dynamically linked against libxar/libtapi that
    # ship in the osxcross lib/. A relocated tree (e.g. crazymax image copied out)
    # loses the baked rpath, so make the host loader find them. Harmless to host
    # tools — only libxar/libtapi live there.
    local libdir
    for libdir in "$OSXCROSS_ROOT/lib" "$(dirname "$bin")/lib"; do
        [[ -e "$libdir/libtapi.so" || -e "$libdir/libxar.so.1" ]] && {
            export LD_LIBRARY_PATH="$libdir${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
            break
        }
    done

    local short
    case "$arch" in
        arm64)  short=oa64 ;;
        x86_64) short=o64 ;;
        *) die "osxcross_setup: unknown arch '$arch'" ;;
    esac

    OSX_CC=$(_osx_tool "$bin" "$arch" "$short" clang)
    OSX_CXX=$(_osx_tool "$bin" "$arch" "$short" clang++)
    OSX_AR=$(_osx_tool "$bin" "$arch" "$short" ar)
    OSX_RANLIB=$(_osx_tool "$bin" "$arch" "$short" ranlib)
    # Boost's darwin toolset archives static libs with `libtool -static`, not ar.
    OSX_LIBTOOL=$(_osx_tool "$bin" "$arch" "$short" libtool)
    [[ -n "$OSX_CC" && -n "$OSX_CXX" ]] || die "no clang wrapper for $arch in $bin"
    export OSX_CC OSX_CXX OSX_AR OSX_RANLIB OSX_LIBTOOL

    # Point the wrappers at the SDK. If bin and SDK sit as <root>/bin + <root>/SDK
    # the wrappers resolve ../SDK on their own; otherwise (relocated crazymax
    # tree) export the override so -isysroot is set explicitly.
    if [[ -z "${OSXCROSS_SDKROOT:-}${SDKROOT:-}" ]]; then
        local sdk
        sdk=$(ls -d "$OSXCROSS_ROOT"/SDK/MacOSX*.sdk "$OSXCROSS_ROOT"/target/SDK/MacOSX*.sdk \
                     /osxsdk/MacOSX*.sdk 2>/dev/null | sort -V | tail -1 || true)
        if [[ -n "$sdk" ]]; then
            export OSXCROSS_SDKROOT="$sdk" SDKROOT="$sdk"
            log "osxcross SDK: $sdk"
        fi
    fi
    log "osxcross $arch: CC=$OSX_CC AR=${OSX_AR:-<default>}"
}
