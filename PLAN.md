# LAN Paste — 局域网剪贴板同步工具实施计划

> 目标：局域网内多设备剪贴板同步，全平台覆盖（覆盖不了的平台用网页兜底），服务端部署在 PVE。
>
> 已确定的约束：PVE 上已有 MySQL 和 PostgreSQL 实例（复用，不新装数据库）；客户端最优先考虑**体积小、安装简单**。

## 0. 平台现实约束（决定整个架构的前提）

| 平台 | 后台自动监听剪贴板 | 结论 |
|---|---|---|
| Windows | ✅ 完全可以 | 常驻客户端，全自动 |
| macOS | ✅ 完全可以（NSPasteboard 轮询，无需特殊权限） | 常驻客户端，全自动 |
| Linux X11 | ✅ 完全可以 | 常驻客户端，全自动 |
| Linux Wayland | ⚠️ 受限（wl-clipboard、需 compositor 配合） | 常驻客户端，基本可用 |
| Android | ⚠️ Android 10+ 禁止后台读剪贴板 | PWA + 分享（零安装），原生 app 后置 |
| iOS | ❌ 系统禁止 App 后台读剪贴板 | 快捷指令 / Share Sheet / PWA，手动 |
| 任意浏览器 | ⚠️ Clipboard API 要求 Secure Context | HTTPS（mkcert）后可用，兜底方案 |

两个关键坑（已调研确认）：

1. **iOS 硬限制**：任何 App 都无法在后台持续读剪贴板。业界通行做法是 Share Sheet 分享、Shortcuts 快捷指令。Apple 设备之间可用系统自带 Universal Clipboard，本工具只需覆盖 iPhone↔Windows/Linux 等跨生态场景。
2. **网页版剪贴板 API**：`navigator.clipboard` 在非安全上下文（局域网 IP + HTTP）下是 `undefined`。必须用 **mkcert** 自建 CA，给服务器签发含 SAN 的证书（`paste.lan` + PVE 内网 IP），并在各设备信任该 CA。降级方案 `document.execCommand('copy')` 只能写不能读。

## 1. 路线决策：现成方案 vs 自研

| 方案 | 平台覆盖 | 评价 |
|---|---|---|
| [ClipCascade](https://github.com/Sathvik-Rao/ClipCascade) | Win/mac/Linux/Android，无 iOS | 最接近需求的现成自托管方案，Docker 部署 |
| KDE Connect | Linux/Win/mac/Android，iOS 支持极弱 | 无服务器、P2P，跨生态体验一般 |
| LocalSend | 全平台含 iOS | 是「手动发送」不是「剪贴板同步」，可作参考 |
| **自研** | 桌面全自动 + 移动端半自动 + Web 兜底 | 完全贴合需求，工作量约 4-6 周（业余时间） |

**建议**：先花半天在 PVE 上用 Docker 跑一遍 ClipCascade（Phase 0），验证真实需求和交互习惯；自研主线照下面执行。

## 2. 总体架构

```
┌──────────────────┐                ┌───────────────────────────┐
│ 桌面客户端 (常驻)  │◄───WSS/HTTPS──►│        服务端 (PVE LXC)     │
│ Win / mac / Linux │                │  Go 单二进制 (~15MB)        │
│  自动监听+自动写入  │                │  ├─ REST API（上传/历史）    │
└──────────────────┘                │  ├─ WebSocket（实时推送）    │
┌──────────────────┐                │  ├─ 内嵌 Web UI（兜底+下载页）│
│ Android PWA/分享   │◄───HTTPS──────►│  └─ PostgreSQL（复用现有实例）│
└──────────────────┘                └───────────┬───────────────┘
┌──────────────────┐                            │ 内网连接
│ iOS 快捷指令/PWA   │◄───HTTPS API──►   现有 PG 实例（新建独立库 paste）
└──────────────────┘                附件/图片存本地文件系统，不进数据库
```

核心同步流程：

1. 客户端监听本地剪贴板（聚焦时轮询 + hash 比较）
2. 内容变化 → `POST /api/clips`（带设备 ID + 内容 hash）
3. 服务端广播 WebSocket 事件给其他在线设备
4. 各设备收到后**先比 hash**，与本地剪贴板相同则忽略（防回环——收到推送→写入本地→又触发本地监听→再上传，这是必须第一天就解决的核心问题）
5. 不同则写入本地剪贴板，并记录「我写入的 hash」用于下一轮比对

## 3. 技术选型

### 3.1 服务端：Go + 现有 PostgreSQL

- 单二进制 ~15MB、内存 <30MB、零依赖，最适合塞进 LXC
- **数据库用现成的 PG**（新建独立 database `paste` + 最小权限专用账号，只授权这一个库），不在服务容器里装任何数据库
- 选 PG 而非 MySQL 的原因：JSONB 存元数据方便、TTL 清理任务好写；MySQL 也完全可行，驱动都是成熟的，改造成本≈0
- **图片/文件附件不进数据库**，落盘到 LXC 本地目录（`/var/lib/paste/files/`，按日期分目录），数据库只存路径——避免把大 blob 塞进你正在跑其他业务的 PG
- 库：`net/http` + `nhooyr.io/websocket` + `jackc/pgx`
- 开发期本机可用 SQLite 跑单测，通过薄 DAO 层隔离，生产只接 PG

数据模型（PostgreSQL）：

```sql
CREATE TABLE clips (
  id          BIGSERIAL PRIMARY KEY,
  hash        TEXT NOT NULL,           -- 内容 sha256，防回环 + 去重
  type        TEXT NOT NULL,           -- text / image / file
  text        TEXT,                    -- 文本内容直接存
  blob_path   TEXT,                    -- 图片/文件落盘相对路径
  size        BIGINT,
  source_device TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON clips (created_at DESC);
CREATE INDEX ON clips (hash);
-- 定时任务清理 N 天前记录（删库中行 + 对应附件文件）
```

### 3.2 客户端：Go 单文件后台代理（体积与安装优先）

桌面客户端**不需要独立 GUI**——同步是全自动的后台行为，唯一的界面就是托盘图标 + 菜单（状态 / 暂停同步 / 打开历史页 / 退出），历史和设置全走服务器的 Web 页。据此直接砍掉所有 GUI 框架：

| 方案 | 体积 | 安装体验 | 结论 |
|---|---|---|---|
| Electron | 100-200MB | 安装包 | ❌ |
| Compose Multiplatform | 80-150MB（含 JRE） | 拖拽安装 | ❌ |
| Flutter Desktop | 30-50MB | 安装包 | ❌ |
| Tauri (Rust) | 5-10MB | 单文件，但 Linux 依赖 webkit2gtk | 备选 |
| **Go + systray 纯后台代理** | **8-12MB 单文件** | 复制即用，`--install` 一条命令开机自启 | ✅ 采用 |

桌面端三平台共用一套 Go 代码（与服务端同一个 module），差异只在剪贴板读写和自启动注册的 platform 实现（build tags 隔离，每平台约 100-200 行）：

- 剪贴板：mac 用 `NSPasteboard`（cgo/objc）、Windows 用 `win32` OpenClipboard、Linux 用 X11 selection / `wl-paste`
- 托盘：`fyne.io/systray`（三平台通吃）
- **安装 = 下载一个文件**：
  - Windows：`paste.exe` 双击即用；`paste.exe --install` 写当前用户启动项（注册表 Run，不需要管理员权限）+ 建开始菜单快捷方式。未签名 exe 首次运行有 SmartScreen 提示，点「仍要运行」即可（后续可买代码签名证书消除）
  - macOS：`paste` 单文件；`--install` 注册登录项。未签名首次需右键打开
  - Linux：`--install` 生成 systemd user unit
- **客户端下载页**：服务器内嵌各平台二进制，浏览器打开 `paste.lan/download` 自动识别平台一键下载 → 扫二维码即可给新设备装好

### 3.3 移动端：零安装优先

- **Android**：PWA 安装到桌面 + **Web Share Target**（系统分享菜单里直接出现「保存到 Paste」，分享文本/图片即上传）；拉取方向打开 PWA 点复制。不写原生 app，后续真有前台服务需求再上（Phase 4）
- **iOS**：两个快捷指令（「推送剪贴板」= 读剪贴板 POST；「拉取最新」= GET 并拷入剪贴板），加入控制中心/桌面；Safari PWA 作为完整 UI。不上架 App Store，零安装成本

### 3.4 通信协议

- `POST /api/clips` 上传；`GET /api/clips?limit=50` 历史；`GET /api/clips/latest` 供快捷指令拉取
- `WSS /ws` 实时推送（消息体小的直接带内容，大的只发通知再 HTTP 拉取）
- 认证：部署时生成共享 Token（配对码），请求头携带；局域网内够用，后续可升级设备级密钥

## 4. PVE 部署方案

- **LXC 容器**（Debian 12，比 VM 更省）：1 vCPU / 512MB RAM / 8GB 盘，绑定静态 IP（路由器 DHCP 保留）
- 跑法：LXC 内直接跑 Go 二进制 + systemd service（无 Docker 层；升级 = 替换二进制重启）
- **连接现有 PG**：在 PG 实例里 `CREATE DATABASE paste` + 专用账号，仅授权该库；LXC 通过内网 IP 访问。首次启动服务端自动建表（或附带 `migrate` 子命令）
- **域名与证书**：LXC 里跑 avahi 做 `paste.local` mDNS 解析；mkcert 生成 CA + 证书（SAN 含 `paste.local` 和 PVE IP），CA 证书分发到各设备信任
- **端口**：8443（HTTPS + WSS）
- **备份**：clips 表随你现有 PG 备份体系走；LXC 走 PVE vzdump；附件目录纳入 vzdump 即可（丢了也可接受——剪贴板内容天然是临时数据）

## 5. 分期实施

### Phase 0 — 需求验证（0.5 天）
- PVE 上 Docker 跑 ClipCascade，常用设备装一遍，用一周
- 明确：常用平台排序、是否需要图片/文件、历史记录要不要、自动同步还是按键触发

### Phase 1 — MVP：两台桌面机文本同步（1~2 周）
- 服务端：Go 骨架 + PG 接入 + REST + WebSocket 广播 + Token 认证 + 最简 Web 页（粘贴框 + 历史）
- 客户端：先做自己的主力平台（macOS 托盘常驻：监听 → 上传 → 收推送写入，hash 防回环，`--install` 自启动）
- 验收：两台桌面机之间复制粘贴文本零操作互通

### Phase 2 — 全桌面 + 图片 + Web 兜底（1 周）
- 客户端补齐 Windows、Linux（X11 优先，Wayland 尽力）
- 图片支持（附件落盘 + HTTP 传输）
- mkcert HTTPS + Web UI 完善（复制按钮、二维码、PWA manifest、下载页）
- 部署到 PVE LXC：systemd + avahi + 接现有 PG + 自动备份

### Phase 3 — 移动端（3~5 天，零安装方案）
- Android：PWA + Web Share Target，扫码安装
- iOS：两个快捷指令 + PWA
- 验收：手机↔电脑双向都能在 5 秒内完成一次传递

### Phase 4 — 增强（按需）
- Android 原生 app（前台服务自动同步，体积预计 ~15MB）
- 文件传输（分块上传 + 下载页）
- 端到端加密（客户端加密、服务端只见密文）
- 设备在线状态、多槽位 Pin、TTL 自动清理、代码签名消除 SmartScreen 提示

## 6. 风险与对策

| 风险 | 对策 |
|---|---|
| 剪贴板回环/震荡 | hash 比对 + 「本机刚写入」标记，第一天就设计进协议 |
| 大附件拖垮现有 PG | 附件一律落盘不进库，库里只有路径；连接池限流 |
| 剪贴板历史含密码等敏感内容 | 内容 TTL 自动清理、端到端加密（Phase 4） |
| Wayland 各 compositor 行为不一 | X11 先行；Wayland 用 wl-paste --watch，文档标注限制 |
| 大内容（截图/文件）阻塞推送 | WebSocket 只发通知，内容走 HTTP 拉取，分块上传 |
| 未签名二进制的系统提示 | Windows SmartScreen / macOS Gatekeeper 首次手动放行，文档写清楚；Phase 4 可上代码签名 |
| iOS 用户体验天花板 | 快捷指令 + 控制中心把操作降到一次点击；文档明确这是系统限制 |

## 7. 参考

- [ClipCascade（最接近的现成方案）](https://github.com/Sathvik-Rao/ClipCascade)、[发布讨论](https://www.reddit.com/r/selfhosted/comments/1ghpfnu/clipcascade_v021_release_seamless_clipboard_sync/)
- [Apple Universal Clipboard（Apple 生态内免费方案）](https://support.apple.com/en-us/102430)
- [Octoclip 文档：iOS 剪贴板后台限制详解](https://docs.octoclip.app/advanced/ios/limitations)
- [非 HTTPS 下 navigator.clipboard 的坑](https://medium.com/@seeranjeeviramavel/the-pitfall-of-using-navigator-clipboard-in-non-https-web-apps-b47e3f065065)、[web.dev 异步剪贴板指南](https://web.dev/articles/async-clipboard)
