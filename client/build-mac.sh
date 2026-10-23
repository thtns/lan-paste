#!/bin/sh
# =============================================================================
# LAN Paste macOS 客户端打包：LanPaste.app + LanPaste-x.y.z.dmg
#
#   ./build-mac.sh            构建当前架构（arm64 或 amd64）
#   ARCH=amd64 ./build-mac.sh 指定架构（给老 Intel Mac 用）
#
# 产物在 client/build/ 下：
#   LanPaste.app              拷到 /Applications 即可使用（菜单栏应用，不占 Dock）
#   LanPaste-<版本>.dmg       分发给其他 Mac（挂载后拖拽安装）
#
# 说明：
#   - 必须在 macOS 上运行（托盘界面需要 cgo）
#   - .app 为 LSUIElement：只有菜单栏图标，无 Dock 图标、无主窗口
#   - ad-hoc 签名（--sign -）：本机无缝运行；分发到其他 Mac 首次打开需
#     「右键 → 打开」绕过 Gatekeeper（未购买开发者证书）
#   - 首次使用：双击打开 LanPaste.app，自动弹出应用内设置对话框
#     （填服务器地址 + 用户名密码，无需终端、无需浏览器）；
#     之后菜单栏常驻，菜单「账号与设置…」可随时重配
# =============================================================================
set -eu

cd "$(dirname "$0")"
VERSION=$(go run . -version | awk '{print $2}')
APP_NAME=LanPaste
APP="build/${APP_NAME}.app"
ARCH="${ARCH:-}"

say() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }

[ "$(uname)" = "Darwin" ] || { echo "请在 macOS 上运行本脚本（托盘需要 cgo）" >&2; exit 1; }
command -v iconutil >/dev/null 2>&1 || { echo "未找到 iconutil（macOS 自带）" >&2; exit 1; }

GOARCH_FLAGS=""
if [ -n "$ARCH" ]; then
    GOARCH_FLAGS="GOARCH=$ARCH"
fi

say "构建 LanPaste v${VERSION}（${ARCH:-$(uname -m)}）"
rm -rf "$APP" build/dmg build/AppIcon.iconset   # 只清理自身产物,不影响 build/windows/
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
env CGO_ENABLED=1 $GOARCH_FLAGS go build -trimpath -ldflags "-s -w" \
    -o "$APP/Contents/MacOS/lanpaste-client" .

say "写入 Info.plist（LSUIElement 菜单栏应用）"
cat > "$APP/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleName</key><string>${APP_NAME}</string>
    <key>CFBundleDisplayName</key><string>LAN Paste</string>
    <key>CFBundleIdentifier</key><string>com.lanpaste.client</string>
    <key>CFBundleExecutable</key><string>lanpaste-client</string>
    <key>CFBundlePackageType</key><string>APPL</string>
    <key>CFBundleShortVersionString</key><string>${VERSION}</string>
    <key>CFBundleVersion</key><string>${VERSION}</string>
    <key>CFBundleIconFile</key><string>AppIcon</string>
    <key>LSMinimumSystemVersion</key><string>11.0</string>
    <!-- 菜单栏应用：无 Dock 图标、无主窗口 -->
    <key>LSUIElement</key><true/>
    <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
EOF

say "生成应用图标（AppIcon.icns）"
ICONSET=build/AppIcon.iconset
mkdir -p "$ICONSET"
go run ./tools/genicon -size 1024 -out "$ICONSET/icon_512x512@2x.png"
for spec in "16:icon_16x16" "32:icon_16x16@2x" "32:icon_32x32" "64:icon_32x32@2x" \
            "128:icon_128x128" "256:icon_128x128@2x" "256:icon_256x256" \
            "512:icon_256x256@2x" "512:icon_512x512"; do
    px=${spec%%:*}; name=${spec##*:}
    sips -z "$px" "$px" "$ICONSET/icon_512x512@2x.png" --out "$ICONSET/$name.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/AppIcon.icns"
rm -rf "$ICONSET" build/AppIcon.iconset 2>/dev/null || true

say "ad-hoc 签名（本机运行免提示）"
codesign --force --sign - "$APP" 2>/dev/null || echo "（签名跳过）"

say "打包 DMG"
mkdir -p build/dmg
cp -R "$APP" build/dmg/
ln -s /Applications build/dmg/Applications   # 拖拽安装布局
DMG="build/${APP_NAME}-${VERSION}.dmg"
hdiutil create -volname "LAN Paste" -srcfolder build/dmg -ov -format UDZO "$DMG" >/dev/null
rm -rf build/dmg

say "完成！"
echo "    应用 : ${APP}（可直接拷到 /Applications）"
echo "    DMG  : $DMG"
echo ""
echo "    安装：挂载 DMG → 拖入 Applications → 双击打开 → 应用内对话框完成设置"
echo "    自启：/Applications/LanPaste.app/Contents/MacOS/lanpaste-client -install"
