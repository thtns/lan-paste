#!/bin/sh
# =============================================================================
# LAN Paste Windows 客户端打包（在 macOS / Linux 上交叉编译即可）
#
#   ./build-windows.sh             产出 amd64（绝大多数 PC）
#   ARCH=arm64 ./build-windows.sh  产出 arm64（Surface 等设备）
#
# 产物在 client/build/windows/ 下：
#   LanPaste.exe                          主程序（无黑色控制台窗口，托盘运行）
#   install-autostart.bat                 双击 = 注册开机自启 + 立即启动
#   README.txt                            使用说明（UTF-8）
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

say "生成自启脚本与说明（文件名用纯 ASCII：Windows 资源管理器的 zip 解码无视 UTF-8 标志位，中文名必乱码）"
# bat：内容纯 ASCII + CRLF，任何代码页下都不会乱码
printf '@echo off\r\ncd /d %%~dp0\r\nLanPaste.exe -install\r\nstart "" "LanPaste.exe"\r\necho LAN Paste: autostart installed, app started.\r\ntimeout /t 3 >nul\r\n' \
    > "$OUT/install-autostart.bat"
# 说明：中文内容放文件里（UTF-8 + BOM，记事本双击即读），文件名保持 ASCII
{ printf '\xef\xbb\xbf'; cat <<'EOF'
LAN Paste — 局域网剪贴板同步（Windows 客户端）

【首次使用】
  1. 双击 LanPaste.exe，自动弹出设置窗口（无需浏览器）
  2. 填写服务器地址（如 192.168.1.10:8080）、用户名、密码、设备名
     （首次使用可勾选「注册新用户」，创建账号后其他设备用同一账号登录）
  3. 点「保存并连接」→ 托盘出现图标，开始自动同步

【Windows 安全提示（SmartScreen）】
  开源项目没有购买代码签名证书，首次运行可能弹出
  「Windows 已保护你的电脑」→ 点「更多信息」→「仍要运行」即可。
  不放心可校验哈希（下载页附 sha256 校验值），或用源码自行构建。

【开机自启】
  双击「install-autostart.bat」一次即可（已运行时也可执行，不会重复启动）。

【日常使用】
  任何设备上复制文字/图片 → 托盘无操作自动同步 → 直接粘贴。
  托盘右键菜单：连接状态 / 账号与设置 / 暂停-恢复同步 / 打开历史页面 / 退出。

【日志与配置】
  C:\Users\<你>\.config\lanpaste\config.json   配置
  C:\Users\<你>\.config\lanpaste\client.log    日志（超 8MB 自动重建）

【卸载】
  退出托盘 → 删除本目录 → 命令行执行 LanPaste.exe -uninstall 移除自启。
EOF
} > "$OUT/README.txt"

say "打包 zip（python3 zipfile，纯 ASCII 文件名，任何系统与解压工具都不乱码）"
command -v python3 >/dev/null || { echo "缺少 python3（用于打包）" >&2; exit 1; }
python3 - "$OUT" "${APP_NAME}-${VERSION}-windows-${ARCH}.zip" <<'PY'
import os, sys, zipfile
src, out = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(os.path.join(src, out), "w", zipfile.ZIP_DEFLATED, compresslevel=9) as zf:
    for n in ("LanPaste.exe", "install-autostart.bat", "README.txt"):
        zf.write(os.path.join(src, n), n)
PY
rm -f "$OUT/app.ico"

say "完成！"
echo "    目录: $OUT"
ls -la "$OUT"
