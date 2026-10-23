# LAN Paste

局域网剪贴板同步工具：桌面端（macOS / Windows / Linux）全自动双向同步文本，手机与任意设备用网页同步文本和图片，服务端单二进制部署在 PVE 的 LXC 里，数据存现有 PostgreSQL / MySQL（也可纯内存零配置）。

## 当前状态

v0.4（应用内设置向导，告别浏览器配置）：

- 文本双向同步、WebSocket 实时推送、防回环（hash 比对）
- **图片全平台同步**：网页粘贴即传；桌面客户端（macOS / Windows / Linux）复制截图自动上传、收到图片自动写入系统剪贴板；服务端落盘内容寻址存储，删除连带清理
- **网页 v2**：按天分组卡片流、类型徽标、搜索 + 筛选、置顶、深色模式、图片灯箱，PWA
- **多用户账号**：用户名+密码注册/登录（bcrypt 存储），登录后签发设备令牌；剪贴板按用户隔离，同账号多设备互通；原共享 token 保留为管理令牌
- **应用内设置向导（v0.4）**：客户端未配置时直接弹**系统原生对话框**（macOS osascript / Windows WinForms / Linux zenity，无图形环境回退终端）填服务器与账号，登录+验证通过自动开始同步——全程不开浏览器；托盘菜单「账号与设置…」随时重配
- **跨设备回环防护（v0.4.3）**：系统剪贴板普遍会重编码内容（macOS 图片 PNGf 往返改变字节、Windows 行尾 `\n`↔`\r\n` 转换），字节 hash 会误判成新内容，造成「收到→写入→读回→再上传」的回环风暴。客户端用**像素级指纹**（图片）与**归一化比对**（文本）兜底判定 self-write，服务端再做归一化去重双保险
- **托盘客户端**：macOS 菜单栏 / Windows 托盘常驻图标，菜单含连接状态、暂停/恢复、打开历史页、设置、退出；macOS 一键打包 .app + .dmg（`client/build-mac.sh`）；Windows 一键打包无窗口 exe + zip（`client/build-windows.sh`）
- **TTL 自动清理**：向导里设「历史保留天数」，到期记录连同附件自动删除（0 = 永不）
- **`/download` 扫码接入**：向导里点「手机扫码接入」生成二维码，手机扫码自动填好地址 + Token
- **一键部署**：`deploy/install.sh remote user@host` 一条命令编译 + 上传 + 装 systemd 服务
- **网页部署向导**：选内存 / PostgreSQL / MySQL、测试连接（可自动建库）、保存即热切换，保存后自动带 Token 回主页面
- 服务端：单二进制 ~17MB，三存储后端，幂等建表 + 老库自动补列
- 客户端：三平台单二进制，纯 Go 零依赖（macOS 托盘需 cgo），`-install` 一键开机自启

## 下载安装（预编译包，无需 Go 环境）

从 [Releases](https://github.com/thtns/lan-paste/releases) 下载对应平台的安装包（每个包都附 sha256 校验值），三步之内开始同步：

| 平台 | 包 | 安装 |
|---|---|---|
| macOS (Apple Silicon) | `LanPaste-x.x.x-macos-arm64.dmg` | 挂载 → 拖入 Applications → 双击打开 |
| macOS (Intel) | `LanPaste-x.x.x-macos-amd64.dmg` | 同上 |
| Windows x64 / ARM64 | `LanPaste-x.x.x-windows-*.zip` | 解压 → 双击 `LanPaste.exe` |
| Linux x64 | `LanPaste-x.x.x-linux-amd64.tar.gz` | 解压 → `./lanpaste-client` |

首次打开的设置对话框里填服务器地址 + 用户名密码即可（详见 [client/README.md](client/README.md)）。**没有 Go 环境也能用**；有环境的也可以用 `client/build-mac.sh` / `build-windows.sh` 一键自行构建。

**安全提示（开源软件无付费签名证书，属正常现象）：**

- **macOS**：首次打开可能提示「已损坏，无法打开」或「无法验证开发者」。处理：右键 `LanPaste.app` →「打开」；仍不行就执行一次 `xattr -cr /Applications/LanPaste.app` 再打开。本应用为 ad-hoc 签名，Gatekeeper 放行一次后续不再打扰
- **Windows**：SmartScreen 可能提示「Windows 已保护你的电脑」→ 点「更多信息」→「仍要运行」。不放心可核对 Releases 里的 sha256 校验值，或用源码自行构建

服务端在任意有 Go 环境的机器上一条命令部署（或交叉编译后直接上传二进制，见 [deploy/](deploy/)）。

## 目录

```
PLAN.md              总体计划（架构、分期、风险）
docs/PROTOCOL.md     API 契约 v1.1（三端共同的协议，改动需联调同步）
docs/ios-shortcuts.md iOS 快捷指令配置
server/              服务端（Go）：REST + WebSocket + 部署向导 + 内嵌 web/
server/web/          Web 前端（纯静态，无构建步骤；主界面 + 向导页）
client/              桌面客户端（Go）：三平台剪贴板 + 自启动
deploy/              部署：install.sh 一键脚本、systemd unit、mkcert 脚本、PVE 指南
design/              网页设计稿（web-design-v2.html，评审用交互稿）
```

## 快速开始（本机体验）

```bash
# 1. 启动服务端（内存模式，无需数据库），记下打印的 token
cd server && go run .
# token 也可直接指定：PASTE_TOKEN=dev-token go run .

# 2. 启动本机客户端（另一个终端）
cd client && go run . -server http://127.0.0.1:8080 -token dev-token -device my-mac

# 3. 浏览器打开 http://127.0.0.1:8080 填入 token
# 现在三个角色互相同步：复制任何文本、或直接在网页上粘贴一张图片试试
```

前端单独调试（不需要 Go）：`cd server/web && node mock-server.mjs` → http://localhost:8899（Token: devtoken，含模拟图片与向导）。

单元测试：`cd server && go test ./...`、`cd client && go test ./...`。

## 部署到 PVE（两条命令）

```bash
cd deploy
./install.sh remote root@<LXC-IP>     # 编译 + 上传 + 装 systemd 服务
# 浏览器打开 http://<LXC-IP>:8080 → 网页向导里选数据库、测试连接、保存
```

详见 [deploy/PVE-DEPLOY.md](deploy/PVE-DEPLOY.md)（LXC 创建、数据库准备、HTTPS/网页剪贴板完整支持）。

各端使用：

- 桌面三平台构建/安装/自启动：[client/README.md](client/README.md)
- 手机网页 / PWA：浏览器打开服务端地址，填 token
- iPhone 一次点击推送/拉取：[docs/ios-shortcuts.md](docs/ios-shortcuts.md)

## 路线图（未完成部分）

- 服务端内置 TLS（当前用 Caddy 反代方案；内置后手机 PWA / 自动剪贴板免反代）
- Android 原生 App（前台服务自动同步；当前 PWA + 网页已覆盖）
- 端到端加密（客户端加密、服务端只见密文）
- webp 图片的桌面端写入（等标准库或引入 x/image/webp）

## 许可

[MIT](LICENSE) · 仓库：<https://github.com/thtns/lan-paste>
