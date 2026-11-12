#!/usr/bin/env bash
# Builds WizeStream (a NewPipe fork) with "Play on home audio" / "Queue on home audio" in the
# stream long-press menu and the Cast picker.
#
# Usage: wizestream/build.sh [checkout-dir]      APK: <checkout-dir>/app/build/outputs/apk/debug/
#
# The patch sits on top of TubeBeam's send-to-tubebeam.patch, so a tubebeam checkout is needed for
# that patch and for the Android build image (JDK 21 + SDK). Set TUBEBEAM_DIR to point at one;
# the default is a sibling directory, cloned if missing.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
SRC="${1:-$HERE/WizeStream}"
TUBEBEAM_DIR="${TUBEBEAM_DIR:-$(cd "$HERE/../.." && pwd)/tubebeam}"

if [[ ! -d "$TUBEBEAM_DIR/.git" ]]; then
  git clone --depth 1 https://github.com/jagjordi/tubebeam.git "$TUBEBEAM_DIR"
fi
if [[ ! -d "$SRC/.git" ]]; then
  git clone --depth 1 https://github.com/jagjordi/WizeStream.git "$SRC"
fi
cd "$SRC"
apply() {
  local patch=$1
  if git apply --check "$patch" 2>/dev/null; then
    git am --keep-cr "$patch" 2>/dev/null || git apply "$patch"
  elif ! git apply --reverse --check "$patch" 2>/dev/null; then
    echo "$(basename "$patch") does not apply to this WizeStream revision; apply it by hand." >&2
    exit 1
  fi
}
apply "$TUBEBEAM_DIR/wizestream/send-to-tubebeam.patch"
apply "$HERE/send-to-yt-fifo.patch"

# Same image as tubebeam's `docker compose build`, built directly: compose's `network: host`
# build option needs an extra --allow flag with recent buildx, plain docker build does not.
docker build --network host --build-arg UID="$(id -u)" --build-arg GID="$(id -g)" \
  -t tubebeam-build:latest "$TUBEBEAM_DIR/docker"
chmod +x gradlew
docker run --rm --network host -u "$(id -u):$(id -g)" \
  -v "$PWD":/workspace -v wizestream-gradle-cache:/home/dev/.gradle \
  tubebeam-build:latest ./gradlew --no-daemon assembleDebug
ls -la app/build/outputs/apk/debug/*.apk
