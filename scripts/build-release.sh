#!/usr/bin/env bash
# 构建一个发布目标：先编译手机端中转程序（嵌入主程序），再编译主程序。CI 与本地 build.sh 共用。
set -euo pipefail
cd "$(dirname "$0")/.."
: "${GOOS:?GOOS must be set}"
: "${GOARCH:?GOARCH must be set}"
: "${OUTPUT:?OUTPUT must be set}"
VERSION="${VERSION:-dev}"
case "$GOOS/$GOARCH" in
  darwin/amd64|darwin/arm64|windows/amd64|windows/arm64) ;;
  *) echo "unsupported release target: $GOOS/$GOARCH" >&2; exit 1 ;;
esac

# 手机端中转程序：静态链接的 Linux 程序可直接在 Android 上运行
for arch in arm64 arm amd64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch GOARM=7 go build -trimpath -ldflags "-s -w" -o "relaybin/wnrelay-${arch}" ./relay
done

if [ -f embedded_ca/ca.pem ]; then
  echo "embedding shared CA from embedded_ca/ca.pem (the binary contains its private key)"
fi
mkdir -p "$(dirname "$OUTPUT")"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$OUTPUT" .
