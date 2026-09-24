#!/usr/bin/env bash
# Builds the npm packages of intact into OUT (default dist/npm): the main package and one
# package per platform. Publish with: for d in OUT/*/; do npm publish "$d"; done (platforms first).
# NPM_SCOPE=@owner/ prefixes every name, for GitHub Packages.
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=${1:-dist/npm}
VERSION=$(node -p "require('./npm/intact-gateway/package.json').version")
rm -rf "$OUT" && mkdir -p "$OUT"
for target in linux/amd64/linux/x64 linux/arm64/linux/arm64 darwin/amd64/darwin/x64 \
              darwin/arm64/darwin/arm64 windows/amd64/win32/x64 windows/arm64/win32/arm64; do
  IFS=/ read -r goos goarch os cpu <<<"$target"
  name="${NPM_SCOPE:-}intact-gateway-$os-$cpu"
  # npm refused the name intact-proxy-win32-x64 as spam, so Windows x64 keeps a separate name; bin/intact.js maps win32-x64 to this name.
  [ "$os-$cpu" = win32-x64 ] && name="${NPM_SCOPE:-}intact-gateway-windows-x64"
  dir="$OUT/${name#"${NPM_SCOPE:-}"}"
  exe=intact; [ "$goos" = windows ] && exe=intact.exe
  mkdir -p "$dir/bin"
  GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$dir/bin/$exe" ./cmd/intact
  cat > "$dir/package.json" <<JSON
{
  "name": "$name",
  "version": "$VERSION",
  "description": "The intact binary for $os $cpu. Install intact-gateway instead.",
  "license": "MIT",
  "repository": { "type": "git", "url": "git+https://github.com/louisphamdev/intact.git" },
  "os": ["$os"],
  "cpu": ["$cpu"],
  "files": ["bin"]
}
JSON
  cp LICENSE "$dir/"
done
cp -r npm/intact-gateway "$OUT/intact-gateway"
cp README.md LICENSE "$OUT/intact-gateway/"
if [ -n "${NPM_SCOPE:-}" ]; then
  (cd "$OUT/intact-gateway" && node -e '
    const fs = require("fs"), scope = process.argv[1], p = JSON.parse(fs.readFileSync("package.json", "utf8"));
    p.name = scope + p.name;
    p.optionalDependencies = Object.fromEntries(Object.entries(p.optionalDependencies).map(([k, v]) => [scope + k, v]));
    fs.writeFileSync("package.json", JSON.stringify(p, null, 2) + "\n");' "$NPM_SCOPE")
fi
echo "built $VERSION into $OUT"
