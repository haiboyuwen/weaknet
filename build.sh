#!/usr/bin/env bash
# 本地交叉编译全部发布目标，产物在 dist/，文件名与 GitHub Release、npm 包一致
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${VERSION:-$(date +%Y%m%d)}"
if [ -f embedded_ca/ca.pem ]; then
  echo "embedding shared CA from embedded_ca/ca.pem (do not publish these binaries)"
else
  echo "no embedded_ca/ca.pem: each machine generates its own CA"
fi

rm -rf dist && mkdir -p dist
for target in darwin/arm64:weaknet-darwin-arm64 darwin/amd64:weaknet-darwin-amd64 windows/amd64:weaknet-win32-x64.exe windows/arm64:weaknet-win32-arm64.exe; do
  platform=${target%%:*} out=${target#*:}
  echo "building ${out}"
  GOOS=${platform%/*} GOARCH=${platform#*/} VERSION="$VERSION" OUTPUT="dist/${out}" scripts/build-release.sh
done

cp README.md dist/
ls -lh dist relaybin
