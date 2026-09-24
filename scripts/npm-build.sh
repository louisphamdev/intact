#!/usr/bin/env bash
# Builds the npm packages of intact into OUT (default dist/npm): the main package and one
# package per platform. Publish with: for d in OUT/*/; do npm publish "$d"; done (platforms first).
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=${1:-dist/npm}
VERSION=$(node -p "require('./npm/intact-proxy/package.json').version")
rm -rf "$OUT" && mkdir -p "$OUT"
for target in linux/amd64/linux/x64 linux/arm64/linux/arm64 darwin/amd64/darwin/x64 \
              darwin/arm64/darwin/arm64 windows/amd64/win32/x64 windows/arm64/win32/arm64; do
  IFS=/ read -r goos goarch os cpu <<<"$target"
  name="intact-proxy-$os-$cpu"
  # npm refused intact-proxy-win32-x64 as spam; bin/intact.js maps win32-x64 to this name.
  [ "$name" = intact-proxy-win32-x64 ] && name=intact-proxy-windows-x64
  dir="$OUT/$name"
  exe=intact; [ "$goos" = windows ] && exe=intact.exe
  mkdir -p "$dir/bin"
  GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$dir/bin/$exe" ./cmd/intact
  cat > "$dir/package.json" <<JSON
{
  "name": "$name",
  "version": "$VERSION",
  "description": "The intact binary for $os $cpu. Install intact-proxy instead.",
  "license": "MIT",
  "repository": { "type": "git", "url": "git+https://github.com/louisphamdev/intact.git" },
  "os": ["$os"],
  "cpu": ["$cpu"],
  "files": ["bin"]
}
JSON
  cp LICENSE "$dir/"
done
cp -r npm/intact-proxy "$OUT/intact-proxy"
cp README.md LICENSE "$OUT/intact-proxy/"
echo "built $VERSION into $OUT"
