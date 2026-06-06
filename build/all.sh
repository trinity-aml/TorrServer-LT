#!/usr/bin/env bash
# Build every target whose toolchain is present on this host.
#
# Nothing is silently dropped: each target is attempted and the final summary
# lists OK / FAIL / SKIP (toolchain missing) so you always know the full state.
#
# Override the set with:  TARGETS="linux-arm64 windows-amd64" build/all.sh
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

ALL_TARGETS="linux-amd64 linux-arm64 linux-armv7 windows-amd64 \
android-arm64 android-armv7 darwin-arm64 darwin-amd64"
TARGETS=${TARGETS:-$ALL_TARGETS}

# Is the toolchain for a target available? Returns 0 if buildable.
have_toolchain() {
    case "$1" in
        linux-amd64)   command -v gcc >/dev/null ;;
        linux-arm64)   command -v aarch64-linux-gnu-g++ >/dev/null ;;
        linux-armv7)   command -v arm-linux-gnueabihf-g++ >/dev/null ;;
        windows-amd64) command -v x86_64-w64-mingw32-g++ >/dev/null ;;
        android-*)     [[ -n "${ANDROID_NDK_HOME:-}" ]] ;;
        darwin-*)      [[ -n "${OSXCROSS_ROOT:-}" ]] ;;
        *) return 1 ;;
    esac
}

declare -A RESULT
for t in $TARGETS; do
    if ! have_toolchain "$t"; then
        printf '\033[1;33m[skip]\033[0m %s — toolchain missing\n' "$t"
        RESULT[$t]=SKIP
        continue
    fi
    printf '\033[1;36m[====]\033[0m building %s\n' "$t"
    if bash "$HERE/$t.sh"; then
        RESULT[$t]=OK
    else
        RESULT[$t]=FAIL
    fi
done

echo
echo "===== build summary ====="
rc=0
for t in $TARGETS; do
    r=${RESULT[$t]:-?}
    case "$r" in
        OK)   printf '  \033[1;32m%-7s\033[0m %s\n' "$r" "$t" ;;
        FAIL) printf '  \033[1;31m%-7s\033[0m %s\n' "$r" "$t" ; rc=1 ;;
        *)    printf '  \033[1;33m%-7s\033[0m %s\n' "$r" "$t" ;;
    esac
done
exit $rc
