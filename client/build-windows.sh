#!/bin/sh
# =============================================================================
# LAN Paste Windows 客户端打包（在 macOS / Linux 上交叉编译即可）
#
#   ./build-windows.sh             产出 amd64（绝大多数 PC）
#   ARCH=arm64 ./build-windows.sh  产出 arm64（Surface 等设备）
#
# 产物在 client/build/windows/ 下：
#   LanPaste.exe                          主程序（无黑色控制台窗口，托盘运行）
#   安装自启.bat                          双击 = 注册开机自启 + 立即启动
#   使用说明.txt
#   LanPaste-<版本>-windows-<架构>.zip    以上三者打包，直接分发
#
# 说明：
#   - 纯 Go 交叉编译，无需 Windows 环境；exe 已内嵌应用图标与版本资源
#   - 首次使用：双击 LanPaste.exe → 弹出应用内设置窗口（填服务器地址 + 用户名密码）
#   - 日志位置：C:\Users\<你>\.config\lanpaste\client.log
# =============================================================================
set -eu

cd "$(dirname "$0")"
VERSION=$(go run . -version | awk '{print $2}')
ARCH="${ARCH:-amd64}"
OUT=build/windows
APP_NAME=LanPaste

say() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }

command -v zip >/dev/null 2>&1 || { echo "未找到 zip 命令" >&2; exit 1; }

rm -rf "$OUT"
mkdir -p "$OUT"

say "生成 exe 图标资源（.syso，自动链接进主程序）"
go run ./tools/genicon -size 256 -ico -out "$OUT/app.ico"
go run github.com/akavel/rsrc@latest -ico "$OUT/app.ico" -arch "$ARCH" \
    -o "rsrc_windows_${ARCH}.syso"

say "交叉编译 LanPaste v${VERSION}（windows/${ARCH}，无控制台窗口）"
CGO_ENABLED=0 GOOS=windows GOARCH="$ARCH" go build -trimpath \
    -ldflags "-s -w -H=windowsgui" \
    -o "$OUT/${APP_NAME}.exe" .
rm -f "rsrc_windows_${ARCH}.syso"   # 移出包目录，避免影响其他平台构建

say "生成安装自启脚本与说明"
# 注意：bat 需要 CRLF 换行
printf '@echo off\r\ncd /d %%~dp0\r\nLanPaste.exe -install\r\nstart "" "LanPaste.exe"\r\necho 已注册开机自启并启动 LAN Paste。\r\ntimeout /t 3 >nul\r\n' \
    > "$OUT/安装自启.bat"
cat > "$OUT/使用说明.txt" <<'EOF'
LAN Paste — 局域网剪贴板同步（Windows 客户端）

【首次使用】
  1. 双击 LanPaste.exe，自动弹出设置窗口（无需浏览器）
  2. 填写服务器地址（如 192.168.1.10:8080）、用户名、密码、设备名
     （首次使用可勾选「注册新用户」，创建账号后其他设备用同一账号登录）
  3. 点「保存并连接」→ 托盘出现图标，开始自动同步

【开机自启】
  双击「安装自启.bat」一次即可（已运行时也可执行，不会重复启动）。

【日常使用】
  任何设备上复制文字/图片 → 托盘无操作自动同步 → 直接粘贴。
  托盘右键菜单：连接状态 / 账号与设置 / 暂停-恢复同步 / 打开历史页面 / 退出。

【日志与配置】
  C:\Users\<你>\.config\lanpaste\config.json   配置
  C:\Users\<你>\.config\lanpaste\client.log    日志（超 8MB 自动重建）

【卸载】
  退出托盘 → 删除本目录 → 命令行执行 LanPaste.exe -uninstall 移除自启。
EOF

say "打包 zip"
(cd "$OUT" && zip -q "${APP_NAME}-${VERSION}-windows-${ARCH}.zip" \
    "${APP_NAME}.exe" "安装自启.bat" "使用说明.txt")
rm -f "$OUT/app.ico"

say "完成！"
echo "    目录: $OUT"
ls -la "$OUT"
