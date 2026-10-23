# LAN Paste 桌面客户端（lanpaste-client）

局域网剪贴板同步工具的桌面常驻客户端。单二进制、无 GUI、纯 Go（`CGO_ENABLED=0`），
支持 macOS / Windows / Linux 三平台，按 [docs/PROTOCOL.md](../docs/PROTOCOL.md) v1.1 契约实现。

核心行为：

- 轮询本地剪贴板（默认 500ms），内容变化且未命中防回环规则时 `POST /api/clips` 上传
- 维持一条 WebSocket 连接（`/ws?token=&device=`），断线指数退避重连（1s 起、翻倍、上限 30s）
- 收到 `new` 事件经防回环判定后写入本地剪贴板
- **托盘/菜单栏界面**：macOS 菜单栏 / Windows 托盘常驻图标，菜单含「连接状态 · 设置… · 暂停/恢复同步 · 打开历史页面 · 退出」；`-no-tray` 回到纯后台；Linux 默认纯后台（`-tags tray` 可选，需 gtk）
- **应用内设置向导（v0.4，不经浏览器）**：未配置时启动直接弹**系统原生对话框**收集服务器地址、用户名、密码、设备名——macOS 用 osascript、Windows 用 WinForms（单窗口表单，含「注册新用户」勾选）、Linux 桌面用 zenity、无图形环境回退终端问答；登录换取设备令牌前先验证服务端连通性与凭据，保存即自动开始同步；托盘菜单「账号与设置…」可随时重新配置（换服务器/换账号/令牌失效重登）
- **单实例保护**：重复启动直接退出（实例锁 `~/.config/lanpaste/client.lock`），菜单栏不会出现重复图标
- **macOS .app 打包**：LSUIElement 菜单栏应用 + AppIcon.icns + ad-hoc 签名；LaunchAgent 为「仅崩溃时重启」——托盘菜单「退出」后不会被系统拉回
- **图片同步（v0.2）**：文本为空时自动探测图片通道，复制截图即上传；收到图片推送下载并写入系统剪贴板
  - macOS：`osascript` PNGf 数据类（系统自带，无 cgo）
  - Windows：Win32 API，PNG 注册格式优先，回退 CF_DIB/DIBV5 内部转 PNG
  - Linux：wl-copy/wl-paste 与 xclip 按 `image/png` 目标类型读写（xsel 无图片能力，自动降级纯文本）
  - 服务端下发的 jpeg/gif 自动转 PNG 后写入；webp 无法本地解码时会记日志跳过（网页端仍可查看）

## 构建

本机构建（以 macOS arm64 为例）：

```bash
cd client
CGO_ENABLED=0 go build -o lanpaste-client .          # Windows/Linux（含 Windows 托盘）
go build -o lanpaste-client .                        # macOS（托盘需要 cgo，在本机构建）
CGO_ENABLED=0 GOOS=linux go build -o lanpaste-client-linux .   # Linux 纯后台版
```

### Windows 打包（exe + zip）

```bash
cd client && ./build-windows.sh     # 产物：build/windows/（amd64；ARM64 用 ARCH=arm64）
```

- `LanPaste.exe`：**无控制台窗口**（`-H=windowsgui`），内嵌应用图标与版本资源；托盘图标为 ICO 格式（Windows LoadImage 只认 .ico，macOS/Linux 用 PNG）
- 提示：Windows 11 默认把新托盘图标收进「^」折叠区——任务栏设置 → 其他系统托盘图标 → 打开 LAN Paste，或直接把图标拖到任务栏
- zip 内含 `安装自启.bat`（双击 = 注册开机自启 + 立即启动）与使用说明
- 分发：把 zip 发给 Windows 机器 → 解压 → 双击 LanPaste.exe → 应用内设置窗口完成配置 → 双击 bat 装自启

### macOS 打包（.app + .dmg）

```bash
cd client && ./build-mac.sh     # 产物：build/LanPaste.app + build/LanPaste-<版本>.dmg
```

- `.app` 为 **LSUIElement 菜单栏应用**：无 Dock 图标、无主窗口，只有菜单栏图标；自带 AppIcon.icns 与 ad-hoc 签名
- 分发：把 `.dmg` 发给其他 Mac → 挂载 → 拖入 Applications → 双击打开 → 应用内设置对话框完成配置即用
- 开机自启：`/Applications/LanPaste.app/Contents/MacOS/lanpaste-client -install`

交叉编译（在任意平台均可执行，无 cgo、无系统依赖）：

```bash
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o lanpaste-client-darwin .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o lanpaste-client.exe .
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o lanpaste-client-linux .
```

体积约 9MB；如需更小可加 `-ldflags "-s -w"`（约减 30%）。

## 快速开始

```bash
# 1. 直接运行（推荐）：弹出应用内设置向导（原生对话框），
#    填服务器地址 + 用户名密码即可，无需 token、无需浏览器
./lanpaste-client

# 2. 也可用命令行直配（token 见服务端启动日志或其数据目录 token 文件）
./lanpaste-client -server http://10.0.0.5:8080 -token <token>

# 3. 注册开机自启 / 移除
./lanpaste-client -install
./lanpaste-client -uninstall
```

全部选项：

| 选项 | 说明 | 默认 |
|---|---|---|
| `-server` | 服务端地址，缺 scheme 时自动补 `http://` | 配置文件 |
| `-token` | 认证 token | 配置文件 |
| `-device` | 设备名 | hostname |
| `-poll` | 本地剪贴板轮询间隔（最小 50ms） | 500ms |
| `-install` / `-uninstall` | 注册 / 移除开机自启 | — |
| `-config` | 配置文件路径 | `$LANPASTE_CLIENT_CONFIG` 或 `~/.config/lanpaste/config.json` |
| `-version` | 打印版本退出 | — |

flag 值优先于配置文件；显式传入的 flag 会回写配置文件（开机自启条目无参运行，依赖该文件）。
首次无配置且未提供 `-server`/`-token` 时自动进入**应用内设置向导**：有图形环境弹原生对话框（macOS osascript / Windows WinForms / Linux zenity），无图形环境（如 ssh）回退终端问答；stdin 不是 TTY 且无对话框可用时打印引导信息退出。

## 配置文件

路径统一为 `~/.config/lanpaste/config.json`（三平台一致，token 敏感，权限 0600）：

```json
{
  "server": "http://10.0.0.5:8080",
  "token": "xxx",
  "device": "my-mac",
  "poll_interval_ms": 500
}
```

## 开机自启（-install）各平台实现

| 平台 | 机制 | 位置 / 说明 |
|---|---|---|
| macOS | LaunchAgents plist + `launchctl load` | `~/Library/LaunchAgents/com.lanpaste.client.plist`，`RunAtLoad` + `KeepAlive`（崩溃自动拉起）；日志在 `~/Library/Logs/lanpaste-client.{out,err}.log` |
| Windows | 注册表当前用户 Run 键 | `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 下的 `lanpaste-client` 值（带引号的二进制绝对路径），无需管理员权限 |
| Linux | systemd 用户服务单元 | 写入 `~/.config/systemd/user/lanpaste.service`，需手动执行 `systemctl --user enable --now lanpaste.service` 启用；日志用 `journalctl --user -u lanpaste.service -f` |

注意：`-install` 注册的是**当前二进制文件本身**的绝对路径（已解析符号链接），
请先把二进制放到最终安装位置（如 `/usr/local/bin`、`~/bin`）再执行 `-install`。

## 剪贴板后端

| 平台 | 实现 | 依赖 |
|---|---|---|
| macOS | `pbpaste` / `pbcopy` | 系统自带 |
| Windows | Win32 API（`CF_UNICODETEXT`，x/sys/windows） | 无 |
| Linux | `wl-paste`/`wl-copy`（Wayland）优先，回退 `xsel`，再回退 `xclip` | 需安装其一 |

Linux 上三种工具都缺失时客户端打印告警但继续运行（装好后自动开始工作，无需重启）。

## 防回环机制

严格按 PROTOCOL.md 规则实现（`internal/syncer/guard.go`，纯状态机，与 IO 分离）：

1. 收到 `new` 事件：hash 与本地剪贴板当前内容相同 → 忽略（顺手更新 lastSeen）
2. 不同则写入本地剪贴板，成功后记录 `lastWrittenHash`
3. 本地轮询到变化：先比 `lastWrittenHash`（命中 = 自己刚收推送写入的，仅更新 lastSeen 不上传），
   再比 `lastSeenHash`（命中 = 重复，不上传），否则上传，成功后更新 `lastSeenHash`

两点实现决策（对契约的保守补充）：

- **启动时把当前剪贴板内容直接标记为 lastSeen**：避免客户端每次重启都把剪贴板里的旧内容重传
  一遍（多设备互相重启时会互相覆盖对方刚复制的内容）
- **上传失败不推进轮询游标**：内容不丢，服务端恢复后在下一轮轮询自动重试；错误日志按 30 秒限流

## 日志样例

```
2026/10/21 22:00:00 lanpaste-client 0.1.0 启动：server=http://10.0.0.5:8080 device=mbp poll=500ms 剪贴板后端=pbpaste/pbcopy
2026/10/21 22:00:00 已连接 ws://10.0.0.5:8080/ws?device=mbp&token=…
2026/10/21 22:00:00 连接就绪：服务端 lanpaste/v0.1，在线设备 3 台
2026/10/21 22:00:05 上传 24B sha256:3f2a91c6… → 服务端记录 #42
2026/10/21 22:00:09 接收 24B sha256:9b41a0dd… 来自 thinkpad（服务端记录 #43）
2026/10/21 22:00:09 忽略上传（self-write）sha256:9b41a0dd…
2026/10/21 22:01:00 已断开：…；4.2s 后重连
```

## 开发

```bash
go vet ./...
go test ./...          # 含防回环决策单测 + httptest 假服务端端到端测试
go test -race ./...
```

目录结构：

```
client/
├── main.go                    # 入口：flag 解析、配置合并、设置向导编排、托盘/引擎接线
├── config.go                  # 配置持久化（~/.config/lanpaste/config.json）
├── internal/syncer/           # 平台无关核心：LoopGuard 决策、轮询引擎、REST、WebSocket 重连
├── internal/setup/            # 应用内设置向导：原生对话框（三平台）+ 注册/登录/验证
├── internal/clipboard/        # 三平台剪贴板（build tags 隔离）
├── internal/tray/             # 托盘/菜单栏（darwin/windows；linux 需 -tags tray）
└── internal/autostart/        # 三平台开机自启（build tags 隔离）
```

## 故障排查

- **无图形环境且非交互运行**：设置向导自动跳过并打印引导（按提示带 `-server`/`-token` 或在终端交互运行）
- **日志一直 `已断开：…connection refused`**：服务端不可达或 token 错误（401 也会重连）；托盘菜单「账号与设置…」重新登录
- **Linux 无剪贴板**：安装 `wl-clipboard`（Wayland）或 `xsel`/`xclip`（X11）
- **Linux 设置向导没有弹出窗口**：安装 `zenity`（或确认有 `DISPLAY`/`WAYLAND_DISPLAY`）；无桌面时自动走终端问答
- **macOS 自启动不生效**：检查 `launchctl list | grep lanpaste` 与 `~/Library/Logs/lanpaste-client.err.log`
- **改了 -server/-token**：带新 flag 运行一次即可；或用托盘菜单「账号与设置…」在应用内重配
