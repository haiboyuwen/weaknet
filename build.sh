#!/usr/bin/env bash
# 交叉编译弱网模拟器：先编译手机端中转程序并嵌入，再编译 Windows / macOS 主程序，产物在 dist/
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${VERSION:-$(date +%Y%m%d)}"
LDFLAGS="-s -w -X main.version=${VERSION}"

# 手机端中转程序：静态链接的 Linux 程序可直接在 Android 上运行
rm -f relaybin/wnrelay-*
for arch in arm64 arm amd64; do
  echo "building relay ${arch}"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch GOARM=7 go build -trimpath -ldflags "-s -w" -o "relaybin/wnrelay-${arch}" ./relay
done

rm -rf dist && mkdir -p dist
build() {
  local os=$1 arch=$2 out=$3
  echo "building ${out}"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "dist/${out}" .
}

build windows amd64 weaknet-windows-amd64.exe
build windows arm64 weaknet-windows-arm64.exe
build darwin  arm64 weaknet-macos-arm64
build darwin  amd64 weaknet-macos-amd64

cp README.md dist/
ls -lh dist relaybin
