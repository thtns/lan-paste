# LAN Paste API 协议契约 v1.2

所有子任务（服务端 / 桌面客户端 / Web 前端）严格按本文件实现。改动本文件必须先在联调时同步三方。

> v1.1（2026-10）：向后兼容的增量扩展——新增图片上传/下载端点、部署向导端点、
> clip 对象新增 `mime` / `width` / `height` / `blob_url` 字段。v1 客户端不受影响
> （新字段均可空，未知类型已被客户端忽略）。
>
> v1.2（2026-10）：多用户——新增 `/api/auth/*` 注册/登录/登出端点；设备以
> 「用户名+密码」登录换取**设备令牌**（每设备独立）；剪贴板按用户隔离。
> 原共享 token 不再兼容：仅作为**管理令牌**用于服务器管理接口（/api/setup 等），
> 不能访问剪贴板；剪贴板访问一律使用设备令牌。

## 基础约定

- 传输：HTTP/1.1 + WebSocket（MVP 阶段明文 HTTP，端口默认 `:8080`；TLS 后续由反向代理或服务端直接加，不改变路径）
- 数据格式：请求与响应体均为 `application/json; charset=utf-8`，UTF-8 编码
- 时间：RFC3339 UTC，如 `2026-10-21T12:34:56Z`
- 哈希：内容 SHA-256 的小写 hex（文本即 UTF-8 字节的 sha256，图片为文件字节的 sha256），用于去重与防回环
- 类型：`"text"` 与 `"image"` 已实现；`"file"` 为保留字

## 认证

两类凭据：

| 凭据 | 获取方式 | 可见范围 |
|---|---|---|
| **管理令牌** | 服务端首次启动生成（日志 / `<data-dir>/token` 文件 / `PASTE_TOKEN` 覆盖） | 仅服务器管理接口（/api/setup 等），不能访问剪贴板 |
| **设备令牌** | `POST /api/auth/login` 用户名+密码登录签发，每台设备独立 | 仅该用户的条目 |

- 所有 `/api/*` 剪贴板请求携带请求头：`Authorization: Bearer <设备令牌>`；`/api/setup` 系列使用管理令牌
- WebSocket 连接（浏览器无法自定义请求头）：`GET /ws?token=<凭据>`
- 认证失败返回 `401 {"error":"unauthorized"}`

### 账号端点 /api/auth/*（匿名开放）

| 端点 | 请求 | 响应 |
|---|---|---|
| `POST /api/auth/register` | `{"username","password"}` | `201 {"ok":true,"user","id"}`；重复/非法 → `400` |
| `POST /api/auth/login` | `{"username","password","device"}` | `200 {"token","user","id"}`；凭据错误 → `401` |
| `POST /api/auth/logout` | Bearer 设备令牌 | `204`；该设备令牌立即失效 |

- 用户名：2-32 位字母/数字/下划线/中划线；密码 ≥ 6 位
- 密码 bcrypt 加盐存储于 `<data-dir>/users.json`（0600）；服务端只存设备令牌的 sha256
- 设备令牌删除即失效（登出）；同一账号可登录任意多台设备，剪贴板互通

## 错误格式

所有非 2xx 响应：`{"error": "<human readable message>"}`

## REST 端点

### POST /api/clips — 上传一条剪贴板内容

请求：
```json
{
  "type": "text",
  "text": "要同步的文本",
  "hash": "sha256hex",
  "device": "macbook-pro"
}
```
- `device`：设备标识，客户端默认取 hostname，用户可配置；前端固定 `"web-<随机ID存localStorage>"`，用户可改
- 服务端校验 hash 与 text 一致，不一致以服务端计算为准

响应 `201`：
```json
{
  "id": 42,
  "type": "text",
  "text": "要同步的文本",
  "hash": "sha256hex",
  "source_device": "macbook-pro",
  "size": 24,
  "created_at": "2026-10-21T12:34:56Z"
}
```

clip 对象完整字段（image 类型专用字段 v1.1 新增）：

| 字段 | 类型 | 说明 |
|---|---|---|
| `id` / `type` / `hash` / `source_device` / `size` / `created_at` | | 同上 |
| `text` | string | 文本内容；image 恒为 `""` |
| `mime` | string | 仅 image：如 `image/png` |
| `width` / `height` | int | 仅 image：像素尺寸，未知为 0（不出现在 JSON） |
| `blob_url` | string | 仅 image：`/api/clips/{id}/blob`（服务端派生，非持久化） |

副作用：服务端向所有**其他**已连接 WebSocket 客户端广播该条目（见下）。若 hash 与库中最新一条相同则不新建记录、不广播，直接返回那条已有记录（响应 `200` 而非 `201`）。

### POST /api/clips/image — 上传一张图片（v1.1）

请求（JSON，`data` 为图片字节 base64，兼容 `data:image/png;base64,` 前缀）：
```json
{
  "data": "iVBORw0KGgo...",
  "device": "web-abc12",
  "width": 1280,
  "height": 720
}
```
- 上限：解码后 16 MB；类型仅 `image/png` / `image/jpeg` / `image/gif` / `image/webp`（服务端嗅探，非图片返回 415）
- 尺寸：png/jpeg/gif 由服务端解码为准；webp 用客户端上报的 `width`/`height`
- 存储：文件落服务端数据目录 `files/<日期>/<sha256>.<ext>`（内容寻址），数据库只存路径
- 去重：与文本同规则（只对最新一条 hash 去重，命中返回 200 + 已有记录）

响应 `201` + clip 对象（`type:"image"`，含 `blob_url`）。

### GET /api/clips/{id}/blob — 下载图片内容（v1.1）

- 需要 Bearer 认证；成功返回图片字节（`Content-Type` 为对应 MIME，支持 Range/ETag）
- 不存在 / 非 image 类型 / 文件缺失 → `404`

### GET /api/clips?limit=50 — 历史列表

响应 `200`：
```json
{
  "clips": [
    {"id":42,"type":"text","text":"...","hash":"...","source_device":"macbook-pro","size":24,"created_at":"2026-10-21T12:34:56Z"}
  ]
}
```
- 按 `created_at` 降序；`limit` 默认 50，上限 200

### GET /api/clips/latest — 最新一条

- 有记录：`200` + 单个 clip 对象（同上结构）
- 无记录：`204` 无 body（iOS 快捷指令用）

### GET /api/clips/{id} — 单条

`200` + clip 对象；不存在返回 `404`。

### DELETE /api/clips/{id} — 删除单条

`204`；同时向所有 WebSocket 客户端广播删除事件。image 类型会连带删除落盘文件。

## 部署向导端点 /api/setup（v1.1，服务端专属）

鉴权：**未配置时（config.json 不存在）全部放开**——局域网内谁先打开向导谁完成配置；
已配置后需要 `Authorization: Bearer <token>`（与剪贴板 API 同一个 token），否则 `401`。

### GET /api/setup — 当前状态

```json
{
  "configured": false,
  "status": {
    "configured": false,
    "listen": ":8080",
    "store": "memory",
    "db": {"store": "postgres", "host": "10.0.0.8", "port": 5432, "user": "paste",
           "database": "paste", "sslmode": "disable"}
  },
  "token": "<当前token>",
  "data_dir": "/var/lib/lanpaste",
  "version": "0.2.0"
}
```
- `db.password` 永不回显

### POST /api/setup/test — 测试连接

请求体：`{"listen":":8080","db":{"store":"postgres","host":"…","port":5432,"user":"…","password":"…","database":"paste","sslmode":"disable"}}`
- `store` 为 `memory` 直接返回 ok
- 成功 `200 {"ok":true}`；失败 `400 {"ok":false,"error":"…"}`
- 库不存在且账号有权限时**自动 CREATE DATABASE** 后重试（超时 5s）

### POST /api/setup — 保存并启用

- 请求体同上；先测试连接（失败 400），再原子写入 `<data-dir>/config.json`（0600），随后**热切换存储后端**（旧连接关闭，新请求即走新库）
- 响应 `{"ok":true,"listen":":8080","restart_required":false}`；`restart_required=true` 表示监听地址变更，需重启服务生效
- 保存成功但热切换失败：`500 {"ok":false,"error":"…"}`（配置已落盘，重启后按新配置运行）

### POST /api/setup/token — 重新生成 token

- 生成 64 位 hex 新 token，同时更新 API 鉴权、WebSocket 鉴权与 `<data-dir>/token` 文件
- 响应 `{"token":"<新token>"}`；环境变量 `PASTE_TOKEN` 覆盖期间不可更换（409 语义，返回 500 + 说明）

## WebSocket /ws?token=<token>

- 子协议：标准 WebSocket，JSON 文本帧
- 服务端 → 客户端消息（唯一必需的）：
```json
{"event": "new", "clip": {"id":42, "...": "...同 REST clip 对象"}}
```
- 删除广播：`{"event": "deleted", "id": 42}`
- 心跳：服务端每 30s 发 WebSocket Ping；客户端须回应 Pong（浏览器自动）
- 客户端 → 服务端：MVP 无（预留）
- 连接后服务端先推一条欢迎消息：`{"event":"hello","server":"lanpaste/v0.1","device_count":3}`

## 防回环规则（客户端与前端都必须实现）

1. 收到 `new` 事件：计算/取出事件内 `hash`
2. 若与「本地剪贴板当前内容的 hash」相同 → 忽略
3. 否则写入本地剪贴板，并记录 `lastWrittenHash = hash`
4. 本地监听到剪贴板变化时：先与 `lastWrittenHash` 比对，相同则仅更新 lastSeen 不上传（这是自己刚收推送写入的）；再与 `lastSeenHash` 比对，相同不上传
5. 上传成功后更新 `lastSeenHash`

## 静态资源

- 服务端以 `go:embed web` 内嵌 `server/web/` 目录，挂载在 `/`（无 SPA 回退：未知路径直接 404）
- `index.html` 主界面；`setup.html` 部署向导（服务端未配置时主页面会自动引导跳转）
- `/healthz`：无认证 `200 "ok"`，供探活

## 目录与模块边界（并行开发的冲突约定）

```
paste/
├── PLAN.md
├── docs/PROTOCOL.md        # 本文件（已冻结，勿改）
├── server/                 # 子任务A 独占；独立 Go module：module lanpaste/server
│   ├── go.mod
│   ├── main.go
│   ├── internal/...
│   └── web/                # 子任务C 独占此子树（纯静态文件，无构建步骤）
├── client/                 # 子任务B 独占；独立 Go module：module lanpaste/client
│   ├── go.mod
│   ├── main.go
│   └── internal/...
└── deploy/                 # 部署物料
```
