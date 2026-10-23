#!/bin/sh
# =============================================================================
# LAN Paste 一键部署脚本
#
# 用法（二选一）：
#
#   1) 在开发机上一条命令部署到 PVE / 远端 Linux 主机：
#        ./install.sh remote root@192.168.1.10
#      自动完成：交叉编译 → scp 上传 → 远端安装 → 启动 systemd 服务
#
#   2) 已经把二进制放到目标机上时，在目标机上直接执行：
#        sudo ./install.sh local [二进制路径]
#      默认找当前目录与 /tmp 下的 lanpaste-server-linux-* 可执行文件
#
# 安装内容：/usr/local/bin/lanpaste-server + systemd 服务（lanpaste.service）
# 数据目录：/var/lib/lanpaste（config.json / token / 图片附件）
# 安装完成后：浏览器打开 http://<主机IP>:8080 完成部署向导（选数据库、测试连接）
# =============================================================================
set -eu

SERVICE_NAME=lanpaste
BIN_NAME=lanpaste-server
INSTALL_BIN=/usr/local/bin/$BIN_NAME
DATA_DIR=/var/lib/lanpaste
SERVICE_USER=lanpaste
DEFAULT_PORT=8080

say()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m警告:\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

# -----------------------------------------------------------------------------
# 远程模式：本机编译 + 上传 + 触发远端 local 安装
# -----------------------------------------------------------------------------
do_remote() {
    [ $# -ge 1 ] || die "用法: $0 remote <user@host> [go 架构 amd64|arm64（留空自动探测）]"
    TARGET="$1"
    ARCH="${2:-}"

    command -v go >/dev/null 2>&1 || die "本机未安装 Go（编译需要）。也可手动编译后用 local 模式安装"
    command -v ssh >/dev/null 2>&1 || die "未找到 ssh"
    command -v scp >/dev/null 2>&1 || die "未找到 scp"

    SERVER_DIR="$(cd "$(dirname "$0")/../server" && pwd)"
    [ -f "$SERVER_DIR/go.mod" ] || die "未找到 server 目录: $SERVER_DIR"

    # SSH 连接复用：密码认证时整个过程只输一次密码（探测/两次 scp/远程安装共用一条连接）
    CM_SOCKET="/tmp/lanpaste-sshmux-$$"
    SSH_OPTS="-o ControlMaster=auto -o ControlPath=$CM_SOCKET -o ControlPersist=300"
    close_mux() { ssh -o "ControlPath=$CM_SOCKET" -O exit "$TARGET" >/dev/null 2>&1 || true; }
    trap close_mux EXIT INT TERM

    say "探测目标机架构…"
    if [ -z "$ARCH" ]; then
        ARCH="$(ssh $SSH_OPTS "$TARGET" uname -m)"
        case "${ARCH}" in
            x86_64)  ARCH=amd64 ;;
            aarch64|arm64) ARCH=arm64 ;;
            *) die "不支持的架构: ${ARCH}（可手动指定 amd64 / arm64）" ;;
        esac
    fi
    say "目标 ${TARGET} 架构 ${ARCH}，开始交叉编译…"
    OUT="/tmp/$BIN_NAME-linux-$ARCH"
    (cd "$SERVER_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
        go build -trimpath -ldflags "-s -w" -o "$OUT" .)
    say "编译完成: $OUT ($(du -h "$OUT" | cut -f1))"

    say "上传二进制与安装脚本…"
    scp -q $SSH_OPTS "$OUT" "$TARGET:/tmp/$BIN_NAME"
    scp -q $SSH_OPTS "$0"   "$TARGET:/tmp/lanpaste-install.sh"

    say "在目标机上执行安装…"
    ssh -t $SSH_OPTS "$TARGET" "sh /tmp/lanpaste-install.sh local /tmp/$BIN_NAME"

    close_mux
    trap - EXIT INT TERM

    say "部署完成！下一步："
    echo "    1. 浏览器打开  http://<主机IP>:$DEFAULT_PORT  完成部署向导（选数据库、测试连接）"
    echo "    2. 向导页会显示访问 Token，各设备填入即可同步"
    echo "    3. 手机/PWA: 同一地址；桌面客户端: 见 client/README.md"
}

# -----------------------------------------------------------------------------
# 本地模式：在目标机上安装
# -----------------------------------------------------------------------------
do_local() {
    # 非 root 时自动用 sudo 重执行自身（ssh -t 提供了 tty，sudo 可正常交互输密码）
    if [ "$(id -u)" != "0" ]; then
        command -v sudo >/dev/null 2>&1 || die "请用 root 运行，或先安装 sudo: apt install -y sudo"
        say "当前非 root，使用 sudo 重新执行…"
        exec sudo /bin/sh "$0" local "$@"
    fi

    # ---- 找二进制 ----
    BIN="${1:-}"
    if [ -z "$BIN" ]; then
        for c in ./$BIN_NAME-linux-* ./$BIN_NAME /tmp/$BIN_NAME-linux-* /tmp/$BIN_NAME; do
            [ -x "$c" ] && BIN="$c" && break
        done
    fi
    [ -n "$BIN" ] && [ -f "$BIN" ] || die "未找到服务端二进制。用法: $0 local <二进制路径>"
    say "安装二进制: $BIN"

    # ---- 服务用户 ----
    if ! id "$SERVICE_USER" >/dev/null 2>&1; then
        say "创建系统用户 $SERVICE_USER"
        useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$SERVICE_USER" 2>/dev/null \
            || adduser --system --group --home "$DATA_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
    fi

    # ---- 目录 ----
    say "准备数据目录 $DATA_DIR"
    mkdir -p "$DATA_DIR"
    chown -R "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR"
    chmod 700 "$DATA_DIR"

    # ---- 二进制 ----
    install -m 0755 "$BIN" "$INSTALL_BIN"
    chown root:root "$INSTALL_BIN"

    # ---- systemd 单元 ----
    say "写入 systemd 服务 $SERVICE_NAME.service"
    cat > /etc/systemd/system/$SERVICE_NAME.service <<EOF
# LAN Paste 剪贴板同步服务
# 配置（数据库/端口/token）：浏览器打开 http://<主机IP>:$DEFAULT_PORT 的部署向导，
# 配置持久化在 $DATA_DIR/config.json，无需编辑本文件。
[Unit]
Description=LAN Paste clipboard sync server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
ExecStart=$INSTALL_BIN -data-dir $DATA_DIR
EnvironmentFile=-/etc/lanpaste.env
Restart=on-failure
RestartSec=3

# 轻量加固
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA_DIR

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    # 注意必须 restart 而不是 enable --now：升级重装时旧进程可能还在跑旧二进制，
    # enable --now 对已激活的服务不会做任何事，会导致"装了新版却仍跑旧版"
    systemctl enable $SERVICE_NAME.service
    systemctl restart $SERVICE_NAME.service
    sleep 1
    systemctl --no-pager --lines 5 status $SERVICE_NAME.service || true

    # ---- 提示 ----
    LAN_IP="$(ip -4 addr show scope global 2>/dev/null | awk '/inet /{print $2; exit}' | cut -d/ -f1 || true)"
    [ -n "$LAN_IP" ] || LAN_IP="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
    echo
    say "安装完成！"
    echo "    服务状态 : systemctl status $SERVICE_NAME"
    echo "    数据目录 : ${DATA_DIR}（config.json / token / files/）"
    echo "    部署向导 : http://${LAN_IP:-<主机IP>}:$DEFAULT_PORT  （首次打开会自动进入）"
    echo "    健康检查 : curl http://127.0.0.1:$DEFAULT_PORT/healthz"
    echo
    echo "    Token 位置: $DATA_DIR/token（向导页也会显示）"
    echo "    修改端口后: 编辑向导里的监听地址 → systemctl restart $SERVICE_NAME"
}

# -----------------------------------------------------------------------------
case "${1:-}" in
    remote) shift; do_remote "$@" ;;
    local)  shift; do_local "$@" ;;
    *)
        die "用法:
  $0 remote <user@host> [amd64|arm64]   在开发机上一键编译并部署到远端
  $0 local  [二进制路径]                 在目标机上安装（root/sudo）"
        ;;
esac
