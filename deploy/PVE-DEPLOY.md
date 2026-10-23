# PVE 部署指南（LAN Paste 服务端）

目标拓扑：PVE 上新建一个轻量 LXC 跑服务端，数据库（可选）复用 PVE 上现有的 PostgreSQL / MySQL。

**部署只需两条命令**：开发机上 `./install.sh remote root@<LXC-IP>`，然后浏览器打开向导完成配置。数据库参数不用写任何配置文件，全部在网页向导里完成。

## 1. 创建 LXC（一次性）

PVE 网页 → 创建 CT，或命令行：

```bash
# 模板建议先下载：pveam update && pveam download local debian-12-standard_12.x-1_amd64.tar.zst
pct create 120 local:vztmpl/debian-12-standard_12.x-1_amd64.tar.zst \
  --hostname paste --memory 512 --cores 1 --rootfs local-lvm:8 \
  --net0 name=eth0,bridge=vmbr0,ip=10.0.0.10/24,gw=10.0.0.1 \
  --unprivileged 1 --features nesting=1 --onboot 1 --start 1
```

- 把 `10.0.0.10/24`、`10.0.0.1` 换成你的网段；或在路由器上对 DHCP 地址做保留
- 资源 1 核 / 512MB / 8GB 足够（实测服务端空闲内存 <30MB）

## 2. 一键部署（开发机执行）

```bash
cd deploy
./install.sh remote root@10.0.0.10
```

脚本自动完成：交叉编译（自动探测 amd64/arm64）→ 上传 → 创建 `lanpaste` 系统用户与 `/var/lib/lanpaste` 数据目录 → 安装 systemd 服务并设为开机自启。

## 3. 网页向导完成配置（替代一切手工配置文件）

浏览器打开 `http://10.0.0.10:8080`，首次访问会自动进入**部署向导**：

1. **选择存储后端**：内存（零配置，重启丢失）/ PostgreSQL（推荐）/ MySQL
2. **填数据库连接**：主机、端口、账号、密码、库名（默认 `paste`）
   - 「测试连接」按钮即点即测；**库不存在且账号有权限时会自动 `CREATE DATABASE`**
   - 建议提前建专用账号（最小权限，只授权 `paste` 一个库）：

     ```sql
     -- PostgreSQL
     CREATE DATABASE paste;
     CREATE USER paste WITH PASSWORD '换成强密码';
     GRANT ALL PRIVILEGES ON DATABASE paste TO paste;
     -- PG 15+ 还需要：\c paste; GRANT ALL ON SCHEMA public TO paste;
     ```
3. **保存**：配置写入 `/var/lib/lanpaste/config.json`，存储后端**即时热切换**，无需重启
4. 向导页同时显示**访问 Token**（也在 `/var/lib/lanpaste/token`），各设备连接时使用；可一键重新生成

> 修改监听端口也在向导里（第 3 步），保存后提示 `systemctl restart lanpaste` 生效。

## 4. 验证

```bash
curl -s http://10.0.0.10:8080/healthz        # -> ok
journalctl -u lanpaste -n 20
```

主界面贴一段文字 → 手机打开同一地址能实时看到，即部署成功。

## 5.（可选）mDNS 别名 paste.local

```bash
apt update && apt install -y avahi-daemon
# Debian 默认已广播 hostname.local（即 paste.local），验证：局域网任一设备 ping paste.local
```

## 6.（可选，建议做）HTTPS：mkcert + Caddy

网页端的自动剪贴板读写（`navigator.clipboard`）、PWA 安装与**图片复制**都要求 Secure Context，纯 HTTP 下网页只能降级使用（历史可见、复制降级、图片长按另存）。桌面客户端不受影响。

```bash
# 开发机上生成证书（见 gen-cert.sh 说明）
./deploy/gen-cert.sh 10.0.0.10

# LXC 内安装 caddy，反代到 8080
apt install -y caddy
# /etc/caddy/Caddyfile：
#   paste.local {
#       tls /path/paste.pem /path/paste-key.pem
#       reverse_proxy 127.0.0.1:8080
#   }
```

把 `mkcert -CAROOT` 下的 `rootCA.pem` 传到各设备并信任（iOS：设置→通用→关于本机→证书信任设置；Android：设置→安全→加密与凭据；Windows/Mac 双击安装）。之后所有设备统一用 `https://paste.local`。

## 7. 客户端安装（各设备）

- **macOS / Windows / Linux**：`client/README.md` 有构建与交叉编译命令。单二进制，运行：
  `lanpaste-client -server http://10.0.0.10:8080 -token <token> -device <设备名>`
  然后加 `-install` 注册开机自启。（当前桌面端只同步文本；图片在网页端查看/复制）
- **手机**：浏览器打开 `http://10.0.0.10:8080`（或 HTTPS 后的 `https://paste.local`），填 token 即用；可安装为 PWA。iOS 进阶用法见 `docs/ios-shortcuts.md`。

## 8. 备份

- LXC 整体：PVE 数据中心→备份，勾选 CT 120，或 `vzdump 120`
- 数据层面：clips 表随你现有 PG/MySQL 备份体系；图片附件在 `/var/lib/lanpaste/files/`，vzdump 已覆盖
- 恢复 = 重建 LXC + `./install.sh remote` + 指回同一数据库；`/var/lib/lanpaste` 里的 `token` 与 `config.json` 保留则各端无需重新配置

## 常见问题

- **连不上数据库**：LXC 内 `nc -vz <db-ip> 5432` 测连通性；PG 的 `pg_hba.conf` 需允许 LXC 网段（`host paste paste 10.0.0.0/24 scram-sha-256`）
- **向导里测试连接失败**：看返回的具体错误；账号密码错 / 网段不通 / 库名非法（只允许字母数字下划线）
- **网页复制 / 图片复制降级提示**：Secure Context 未生效，走第 6 步 HTTPS
- **换端口**：向导里改监听地址 → `systemctl restart lanpaste`
- **忘记 token**：`cat /var/lib/lanpaste/token`，或向导里重新生成
