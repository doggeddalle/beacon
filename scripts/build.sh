#!/bin/sh
# Build Beacon binaries and the ASUSTOR App Central package.
#
# The companion package.ps1 is Windows-only and hardcodes a Go install path,
# even though cmd/mkapkg was deliberately written to be portable stdlib-only.
# This script builds the same artifacts anywhere Go runs.
#
# Usage:
#   scripts/build.sh                # binaries + package for the default arch
#   scripts/build.sh --arch amd64   # Intel ASUSTOR models
#   scripts/build.sh --check        # gofmt/vet/test only, no artifacts
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"

version=$(tr -d ' \t\n\r' < VERSION)
arch=arm64
check_only=0

while [ $# -gt 0 ]; do
  case "$1" in
    --arch)    arch=$2; shift 2 ;;
    --version) version=$2; shift 2 ;;
    --check)   check_only=1; shift ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

echo "==> Checking"
unformatted=$(gofmt -l . || true)
if [ -n "$unformatted" ]; then
  echo "gofmt needed on:" >&2
  echo "$unformatted" >&2
  exit 1
fi
go vet ./...
# The race detector needs a cgo toolchain, which is not present on every build
# host (notably a plain Windows install). Probe for it quietly, and say which
# mode actually ran so a race-free result is never assumed.
if CGO_ENABLED=1 go build -race -o /dev/null ./cmd/beacon >/dev/null 2>&1; then
  echo "    race detector available"
  CGO_ENABLED=1 go test -race -count=1 ./...
else
  echo "    NOTE: no cgo toolchain here, so tests run WITHOUT -race."
  echo "          Concurrency bugs will not be caught on this host; CI covers that."
  go test -count=1 ./...
fi

[ "$check_only" -eq 1 ] && { echo "==> Checks passed"; exit 0; }

dist="$repo/dist"
stage="$dist/pkgroot"
rm -rf "$stage"
mkdir -p "$stage/CONTROL" "$dist"

echo "==> Cross-compiling beacon $version (linux/$arch)"
GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w -X main.version=$version" \
  -o "$stage/beacon" ./cmd/beacon

echo "==> Staging package tree"
cp -R packaging/apkg/CONTROL/. "$stage/CONTROL/"
cp scripts/beaconctl.sh "$stage/"
cp beacon.example.toml "$stage/"

echo "==> Generating icon"
go run ./cmd/mkicon "$stage/CONTROL/icon.png" 256

# ASUSTOR packages use the .apk extension even though the format is APKG 2.0.
apkg="$dist/Beacon_${version}_${arch}.apk"
echo "==> Assembling $apkg"
go run ./cmd/mkapkg -root "$stage" -out "$apkg" -version "$version"

size=$(wc -c < "$apkg")
echo "==> Done: $apkg ($((size / 1024)) KiB)"
echo "    Install on the NAS: App Central > Manual Install > Browse to this .apk > Upload."
