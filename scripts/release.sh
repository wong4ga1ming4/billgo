#!/usr/bin/env bash
# 交叉编译各平台的发行包，输出到 dist/：
#   billgo_<version>_<os>_<arch>.tar.gz|.zip
#   checksums.txt
#   install.sh / install.ps1
#
# 用法: scripts/release.sh v1.0.0
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION=${1:-}
if [ -z "$VERSION" ]; then
  echo "用法: scripts/release.sh v1.0.0" >&2
  exit 1
fi
case "$VERSION" in
  v*) ;;
  *) VERSION="v$VERSION" ;;
esac

OUT=dist
rm -rf "$OUT"
mkdir -p "$OUT"

LDFLAGS="-s -w -X main.version=$VERSION"
TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

for target in $TARGETS; do
  os=${target%/*}
  arch=${target#*/}
  name="billgo_${VERSION}_${os}_${arch}"
  dir="$OUT/$name"
  mkdir -p "$dir"

  ext=""
  if [ "$os" = "windows" ]; then ext=".exe"; fi

  echo "==> $name"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" \
    -o "$dir/billgod$ext" ./cmd/billgod
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" \
    -o "$dir/billgo$ext" ./cmd/billgo
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" \
    -o "$dir/billui$ext" ./cmd/billui
  cp README.md "$dir/"
  if [ "$os" != "windows" ]; then
    cp scripts/wake.sh "$dir/"
    tar -czf "$OUT/$name.tar.gz" -C "$OUT" "$name"
  else
    (cd "$OUT" && zip -qr "$name.zip" "$name")
  fi
  rm -rf "$dir"
done

cp scripts/install.sh scripts/install.ps1 "$OUT/"
(
  cd "$OUT"
  # 不能带 ./ 前缀：安装脚本按文件名精确匹配。
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum ./*.tar.gz ./*.zip | sed 's| \./| |' >checksums.txt
  else
    shasum -a 256 ./*.tar.gz ./*.zip | sed 's| \./| |' >checksums.txt
  fi
)

echo
echo "产物在 $OUT/："
ls -1 "$OUT"
