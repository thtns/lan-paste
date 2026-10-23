# LAN Paste 服务端

局域网剪贴板同步工具的服务端。单二进制，内嵌 Web UI（主界面 + 部署向导），REST + WebSocket 实时推送，支持文本与图片。
API 契约见 [`../docs/PROTOCOL.md`](../docs/PROTOCOL.md)（v1.1）。
配套配置项：`config.json` 支持 `retention_days`（历史保留天数，0=永不清理，默认 0；`settings.html` 服务设置页可改，向导保存不会覆盖该值）。

## 功能

- `POST /api/clips` 上传文本，SHA-256 去重（与最新一条相同则返回 `200` 已有记录且不广播）
- `POST /api/clips/image` 上传图片（base64 JSON，png/jpeg/gif/webp，≤16MB），落盘 `files/<日期>/<hash>.<ext>`，数据库只存路径
- `GET /api/clips?limit=50` 历史（时间降序，limit 默认 50、上限 200）
- `GET /api/clips/latest` 最新一条（无记录 `204`）
- `GET / DELETE /api/clips/{id}` 单条查询 / 删除（图片删除连带清理落盘文件）
- `GET /api/clips/{id}/blob` 图片内容下载（带认证，支持 Range/ETag）
- `GET /ws?token=<token>&device=<name>` WebSocket：连接先收 `hello`，其他设备上传时收 `new`（来源设备被跳过），删除时全员收 `deleted`；服务端每 30s Ping
- **多用户账号**（v1.2）：`/api/auth/register|login|logout`，bcrypt 密码、设备令牌签发；条目按用户隔离，管理令牌可见全部
- Bearer Token 认证（`/healthz`、静态页面、`/api/auth/*` 与未配置时的向导除外）
- 存储后端三选一：**内存**（默认，容量 1000 条自动淘汰最旧）/ **PostgreSQL** / **MySQL**（均幂等建表，老库自动补列迁移）
- **部署向导** `/setup.html`：选数据库、测试连接（可自动建库）、保存即热切换，零配置文件手工编辑；可设历史保留天数、重新生成 token、生成手机扫码接入链接
- **TTL 自动清理**：`retention_days > 0` 时每小时清理过期记录并连带删除附件（0 = 永不清理；向导热更新即时生效）
- **`/download` 扫码接入页**：携带有效 token（`?token=` 或 Bearer）时二维码包含 `origin/?token=…`，手机扫码自动完成配置
- 图片元数据：服务端嗅探 MIME、解码 png/jpeg/gif 尺寸（webp 用客户端上报值）

## 本地运行（memory 模式）

```bash
cd server
go build -o lanpaste-server .
./lanpaste-server                # 默认 :8080，数据目录 ~/.local/share/lanpaste
# 首次启动会在 stdout 打印生成的 token，并存入 <data-dir>/token
```

常用参数（均有 config.json 默认值，**一般用不到**——部署时通过网页向导配置）：

| 参数 | 默认 | 说明 |
|---|---|---|
| `-listen` | config / `:8080` | HTTP 监听地址 |
| `-store` | config / `memory` | `memory` / `postgres` / `mysql` |
| `-data-dir` | `~/.local/share/lanpaste` | config.json / token / 图片附件 |
| `-config` | `<data-dir>/config.json` | 配置文件路径 |
| `-version` | — | 打印版本号 |

环境变量（兼容 v1.0 方式，向导配置的优先级更低）：

| 变量 | 说明 |
|---|---|
| `PASTE_TOKEN` | 覆盖 token（优先级最高，优先于 token 文件；覆盖期间向导不能在线换 token） |
| `PASTE_PG_DSN` | postgres 模式的连接串（设置后优先于 config.json 的 DSN） |

## 接入 PostgreSQL / MySQL

**推荐方式：不用改任何文件**——服务端跑起来后浏览器打开 `http://<IP>:8080`，部署向导里选择数据库、填连接参数、点「测试连接」（库不存在且有权限时会自动建库），保存即热切换生效。配置持久化为 `<data-dir>/config.json`。

手工方式（等价）：直接写 `config.json` 后重启服务：

```json
{
  "listen": ":8080",
  "db": {
    "store": "postgres",
    "host": "10.0.0.8", "port": 5432,
    "user": "paste", "password": "强密码",
    "database": "paste", "sslmode": "disable"
  }
}
```

表结构由服务端首次连接自动创建（幂等）；v1.0 建过的老库会自动补 `mime` / `width` / `height` 列。

## 冒烟测试

```bash
TOKEN=$(cat ~/.local/share/lanpaste/token)   # 或首次启动 stdout 里那串
BASE=http://127.0.0.1:8080

# 探活（无认证）
curl -s $BASE/healthz                          # -> ok

# 上传一条文本（201；重复内容再传返回 200 已有记录）
curl -s -X POST $BASE/api/clips \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"type":"text","text":"hello from mac","device":"macbook-pro"}'

# 上传一张图片（base64；返回含 blob_url）
curl -s -X POST $BASE/api/clips/image \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"data\":\"$(base64 -i photo.png)\",\"device\":\"macbook-pro\"}"

# 下载图片
curl -s $BASE/api/clips/1/blob -H "Authorization: Bearer $TOKEN" -o out.png

# 拉最新一条（无记录时 204）
curl -s $BASE/api/clips/latest -H "Authorization: Bearer $TOKEN"

# 部署向导状态
curl -s $BASE/api/setup
```

浏览器打开 `http://<服务器IP>:8080/` 即为内嵌 Web UI；`/setup.html` 为部署向导。

## 测试

```bash
cd server && go test ./...        # 全部单元 / 集成测试
go vet ./...
```

## 目录结构

```
server/
├── main.go                       # 入口：config 加载、token、装配路由与 go:embed web
├── internal/
│   ├── store/                    # Clip 模型 + Store 接口
│   │   ├── memstore/             # 内存实现（锁 + 容量 1000 淘汰）
│   │   ├── pgstore/              # PostgreSQL 实现（pgx v5，幂等建表 + 补列迁移）
│   │   └── mysqlstore/           # MySQL 实现（go-sql-driver，幂等建表）
│   ├── blob/                     # 图片附件落盘（files/<日期>/<hash>.<ext>，防目录穿越）
│   ├── api/                      # REST 路由 + Bearer 认证 + hash 去重 + 图片端点 + 热切换
│   ├── setup/                    # 部署向导：config.json 管理 + /api/setup 端点
│   └── ws/                       # /ws 端点 + Hub 广播（实现 api.Broadcaster）
└── web/                          # 内嵌静态资源（主界面 + 向导页，纯静态无构建）
```

## 安全说明

- MVP 阶段为明文 HTTP（局域网），TLS 由后续反向代理或服务端直接加，不改变路径
- WebSocket 跳过 Origin 校验（浏览器场景 Origin 不固定），鉴权完全依赖 token 查询参数
- token 文件权限 0600，数据目录 0700，config.json 0600（内含数据库密码），users.json 0600（密码 bcrypt 哈希、设备令牌仅存 sha256）
- 部署向导在「未配置」状态下对局域网放开（谁先部署谁配置）；配置完成后所有写操作需要 token
