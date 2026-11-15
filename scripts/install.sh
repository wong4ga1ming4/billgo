#!/bin/sh
# billgo 安装脚本（macOS / Linux）
#
#   curl -fsSL https://raw.githubusercontent.com/wong4ga1ming4/billgo/main/scripts/install.sh | sh
#
# 可覆盖：
#   VERSION=v1.0.0   指定版本
#   INSTALL_DIR=...  指定安装目录（默认 /usr/local/bin，不可写时用 ~/.local/bin）
set -eu

REPO=${REPO:-wong4ga1ming4/billgo}
VERSION=${VERSION:-}
INSTALL_DIR=${INSTALL_DIR:-}

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "缺少依赖：$1" >&2
    exit 1
  }
}
need curl
need tar

os=$(uname -s 2>/dev/null || echo unknown)
case "$os" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *)
    echo "不支持的平台：$os（Windows 请用 install.ps1）" >&2
    exit 1
    ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *)
    echo "不支持的架构：$arch" >&2
    exit 1
    ;;
esac

if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
  if [ -z "$VERSION" ]; then
    echo "无法获取最新版本，可用 VERSION=v1.0.0 指定" >&2
    exit 1
  fi
fi

asset="billgo_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "下载 $asset ..."
curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

expected=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
if [ -z "$expected" ]; then
  echo "校验文件里没有 $asset" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
else
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
fi
if [ "$actual" != "$expected" ]; then
  echo "校验失败：期望 $expected，实际 $actual" >&2
  exit 1
fi

tar -xzf "$tmp/$asset" -C "$tmp"

if [ -z "$INSTALL_DIR" ]; then
  if [ -w /usr/local/bin ] 2>/dev/null; then
    INSTALL_DIR=/usr/local/bin
  else
    INSTALL_DIR="$HOME/.local/bin"
  fi
fi
mkdir -p "$INSTALL_DIR"

src="$tmp/billgo_${VERSION}_${os}_${arch}"
install -m 0755 "$src/billgod" "$INSTALL_DIR/billgod"
install -m 0755 "$src/billgo" "$INSTALL_DIR/billgo"

echo "已安装 $VERSION 到 $INSTALL_DIR"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo "把 $INSTALL_DIR 加进 PATH："
    echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac
"$INSTALL_DIR/billgo" version
