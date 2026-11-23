#!/usr/bin/env bash
# 交叉编译三个目标 + 生成分发包
set -euo pipefail
cd "$(dirname "$0")"
VER=$(sed -n 's/^const version = "\(.*\)"/\1/p' main.go)
export CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=mod

for arch in amd64 arm64; do
  echo "==> linux/$arch"
  GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "relay-retry-proxy-linux-$arch" .
done

echo "==> windows/amd64"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o relay-retry-proxy-windows-amd64.exe .

echo "==> 打包 dist/"
rm -rf dist/pkg && mkdir -p dist/pkg
cp install.sh relay-retry-proxy-linux-amd64 relay-retry-proxy-linux-arm64 README.md relay-retry-proxy.env.example dist/pkg/ 2>/dev/null || true
(cd dist/pkg && sha256sum relay-retry-proxy-linux-* install.sh > SHA256SUMS)
tar -czf "dist/relay-retry-proxy-$VER-linux.tar.gz" -C dist/pkg .

echo "==> 打包 Windows"
rm -rf bundle-win && mkdir -p bundle-win
cp relay-retry-proxy-windows-amd64.exe relay-retry-proxy.env 使用说明.txt *.bat bundle-win/ 2>/dev/null || true
(cd bundle-win && sha256sum ./* > SHA256SUMS && zip -qr ../relay-retry-proxy-windows.zip .)

echo "完成：dist/relay-retry-proxy-$VER-linux.tar.gz  relay-retry-proxy-windows.zip"
