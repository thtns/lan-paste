# LAN Paste Web 前端 v2（主界面 + 部署向导 + PWA）

本目录是 LAN Paste 的浏览器端界面，由服务端以 `go:embed` 原样内嵌并挂载在 `/`。
**纯静态文件，无构建步骤，无任何外部依赖（框架 / CDN 均未使用），局域网断网也能打开。**

## 文件清单

| 文件 | 职责 |
|---|---|
| `index.html` | 主界面骨架（卡片流 + 工具条 + 灯箱，中文 UI，移动端优先响应式） |
| `style.css` | 浅色 / 深色双主题、系统字体栈、圆角卡片；向导页共用 |
| `app.js` | 入口：配置引导、文本/图片上传、筛选搜索、主题、实时同步、防回环 |
| `ui.js` | ClipListView：按天分组卡片流（类型徽标 / 置顶 / 图片懒加载），textContent 防 XSS |
| `setup.html` / `setup.js` | 部署向导：选数据库、测试连接、保存热切换、token 管理 |
| `settings.html` / `settings.js` | 服务设置页：历史保留天数等运行时参数（保存即生效） |
| `api.js` | REST 封装（Bearer 认证、超时、错误解析；含图片与 setup 端点） |
| `ws.js` | WebSocket 封装（`/ws?token=&device=`，断线指数退避重连） |
| `utils.js` | SHA-256（含纯 JS 兜底）、时间格式化、toast、复制降级、图片尺寸读取 |
| `manifest.webmanifest` | PWA 清单（standalone、主题色 #4f46e5） |
| `sw.js` | Service Worker：静态资源 cache-first（v2）；`/api`、`/ws`、`/healthz` 直连不缓存 |
| `icon.svg` | 剪贴板图标（manifest / favicon / 空状态共用） |
| `mock-server.mjs` | 前端自测用 mock 服务器（见下文；会被 embed 但页面不引用，无副作用） |

## 功能

- **卡片流历史**（v2 新版式）：按「今天 / 昨天 / 日期」分组；内容自动识别类型徽标（文本 / 代码 / 链接 / 颜色 / 图片）；置顶（本地存储，钉在列表最上方）；全部 / 文本 / 图片筛选 + 关键词搜索；深浅色主题切换（跟随系统 + 记忆）
- **文本同步**：粘贴即上传（任意位置 `Ctrl / ⌘ + V`）或输入框 + 「同步」/ `Ctrl / ⌘ + Enter`；收到推送自动写入系统剪贴板（安全上下文）
- **图片同步**（v1.1）：任意位置直接粘贴图片上传；图片卡片显示缩略图 + 尺寸 / 大小 / 格式；点击灯箱看大图；复制（安全上下文直接写剪贴板，否则引导长按 / 右键另存）；删除连带清理服务端文件
- **首访引导**：无配置时显示连接卡片，支持**账号密码登录/注册**（v1.2 多用户）或高级 Token 直填，存 `localStorage`；顶栏齿轮随时修改
- **实时更新**：WebSocket 收 `new` 插入顶部（同 id 记录自动合并）、`deleted` 移除；断线显示状态条并指数退避重连，重连成功自动补拉
- **扫码接入入口**：顶栏二维码按钮（手机端自动隐藏）跳转 `/download?token=…`，二维码含地址与 Token，手机扫码即自动配置
- **防回环**（按 PROTOCOL.md）：`lastSeenHash` / `lastWrittenHash` 双 hash 比对（文本方向）
- **空列表 / 无匹配引导**、**toast 反馈**、**401 自动弹出设置卡**

## 部署向导（setup.html）

服务端未完成配置时（`GET /api/setup` 返回 `configured:false`），主页面会自动跳转到向导：

1. 选择存储后端：内存 / PostgreSQL / MySQL
2. 填数据库连接参数，「测试连接」即点即测（库不存在且有权限时服务端自动建库）
3. 保存：写入服务端 `<data-dir>/config.json` 并**热切换**存储；随后**自动带 Token 回主页面并直接进入**（仅监听端口变更时提示重启，重启后也有直达链接）
4. 显示 / 重新生成访问 Token；运行时参数（历史保留天数）在独立的服务设置页 `settings.html`（首页齿轮 → 服务端设置，或向导页入口）

已配置的服务器进入向导需先输入 token 解锁（防误改）；「跳过向导」仅未配置时出现，同样会带回 token；跳过后主页面不再自动跳转（localStorage `lanpaste.setupDismissed`）。

## Token 从哪来

1. 服务端**首次启动时打印到日志**；
2. 服务端**数据目录下的 `token` 文件**；
3. 部署**向导页**直接显示，可一键重新生成；
4. 可用环境变量 `PASTE_TOKEN` 覆盖（详见 PROTOCOL.md「认证」一节）。

Token 保存在浏览器 `localStorage`（键 `lanpaste.config`），设备名键 `lanpaste.device`。

## HTTP（非安全上下文，即局域网 IP + HTTP）下的降级行为

| 能力 | HTTPS / localhost | HTTP 局域网 |
|---|---|---|
| 内容 SHA-256 | `crypto.subtle` | 内置纯 JS SHA-256（结果与原生一致，防回环不受影响） |
| 「复制」文本 | `navigator.clipboard.writeText` | 降级 `execCommand('copy')`（仅能写）；再失败则选中全文提示手动 `Ctrl+C` |
| 「复制」图片 | `ClipboardItem` 直接写剪贴板 | 不可用 → 打开灯箱引导长按 / 右键另存 |
| 收到推送自动写剪贴板 | 尽力写入（页面需聚焦） | 不可用，toast 提示手动点「复制」 |
| PWA / Service Worker | 正常注册，静态资源离线缓存 | 浏览器不支持注册，页面照常使用（无离线缓存） |

注意：`document paste` 事件捕获是**主动粘贴**行为，HTTP 下同样可用，不受剪贴板权限限制，这是移动端 / 桌面浏览器体验最好的路径。

## 本地自测（mock 服务器）

```bash
cd server/web
node mock-server.mjs            # http://localhost:8899  Token: devtoken
PORT=9000 PASTE_TOKEN=xxx node mock-server.mjs
```

零依赖（Node ≥ 20，内置 WebSocket 帧编解码），按 PROTOCOL.md 仿真全部 REST 端点
（含图片上传 / 下载与 `/api/setup` 系列向导端点）与 `/ws` 推送，内存数据、预置含 1 张图片的 5 条记录、重启即重置，静态文件 `Cache-Control: no-store` 方便调试。

## 已知限制

- 桌面客户端当前只同步文本；图片在网页端查看 / 复制（客户端收到图片会在日志提示）
- 置顶列表保存在浏览器本地，不跨设备同步（协议未含 pin 字段）
- `lastSeenHash` / `lastWrittenHash` 存内存，页面刷新后重置（重复上传由服务端对最新一条去重兜底）
- 列表默认拉取 100 条（协议上限 200）；超大文本展开时限制高度内部滚动
- webp 的像素尺寸依赖浏览器上报（服务端标准库不解 webp），异常时显示 0×0
- iOS 主屏图标：`apple-touch-icon` 指向 SVG，iOS 不支持 SVG 图标，添加到主屏时会是截图式图标（如需完美图标可后续补 PNG）
