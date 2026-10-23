#!/usr/bin/env bash
# 生成局域网 HTTPS 证书（mkcert）。
# 用途：让网页端的 navigator.clipboard 与 PWA 在局域网设备上可用（Secure Context 要求）。
# 前置：brew install mkcert（macOS）或参考 https://github.com/FiloSottile/mkcert 安装。
#
# 用法：
#   ./gen-cert.sh <LAN_IP> [额外域名...]
# 示例：
#   ./gen-cert.sh 10.0.0.10                  # 证书覆盖 paste.local + 10.0.0.10
#   ./gen-cert.sh 10.0.0.10 pve.lan other.lan
#
# 输出：./certs/paste.local+<ip>.pem（证书）与 .key.pem（私钥），供 Caddy/nginx 使用。
set -euo pipefail

LAN_IP="${1:?用法: $0 <LAN_IP> [额外域名...]}"
shift || true
EXTRA_SANS=("$@")

command -v mkcert >/dev/null || { echo "未找到 mkcert，请先安装" >&2; exit 1; }

mkdir -p certs
# 首次运行会在本机生成根证书（mkcert -install 信任它）；
# 其他设备要信任的根证书位于 $(mkcert -CAROOT)/rootCA.pem，拷给手机/电脑安装即可。
mkcert -install

SANS=(paste.local "$LAN_IP" "${EXTRA_SANS[@]}")
mkcert -cert-file certs/paste.pem -key-file certs/paste-key.pem "${SANS[@]}"

echo ""
echo "证书已生成：certs/paste.pem certs/paste-key.pem"
echo "SAN: ${SANS[*]}"
echo "根证书（分发到各设备信任）：$(mkcert -CAROOT)/rootCA.pem"
