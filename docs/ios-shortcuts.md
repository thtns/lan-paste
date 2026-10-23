# iOS 快捷指令配置（LAN Paste）

iOS 不允许 App 后台读剪贴板，用两个快捷指令把操作降到一次点击。两个指令都建议加入控制中心（设置→控制中心）或放到桌面/Apple Watch。

以下 `<SERVER>` 替换为你的服务端地址，如 `http://10.0.0.10:8080` 或 `https://paste.local`；`<USER>`/`<PASS>` 替换为你的账号（未注册可先用 POST /api/auth/register 创建）。

快捷指令不再直接使用管理令牌——管理令牌仅用于服务器管理。快捷指令的流程：
1. 「登录」步骤：POST `http://<SERVER>/api/auth/login`，Body
   `{"username":"<USER>","password":"<PASS>","device":"iphone"}`
   → 从响应 JSON 取 `token`（设备令牌）；
2. 「拉取/推送」步骤：携带 `Authorization: Bearer <设备令牌>`。
   设备令牌长期有效，可在登录步骤后用「获取词典值」存入剪贴板变量复用。

## 快捷指令 1：「推送到 Paste」（iPhone → 其他设备）

在「快捷指令」App → 新建快捷指令，依次添加动作：

1. **获取剪贴板**（Get Clipboard）
2. **获取 URL 内容**（Get Contents of URL），配置：
   - URL：`<SERVER>/api/clips`
   - 方法：`POST`
   - 请求头：
     - `Authorization` = `Bearer <TOKEN>`
     - `Content-Type` = `application/json`
   - 请求体（JSON）：添加字段
     | 键 | 类型 | 值 |
     |---|---|---|
     | type | 文本 | `text` |
     | text | magic Variable | 剪贴板（选择第 1 步的「剪贴板」变量） |
     | device | 文本 | `iphone` |

使用场景：在 iPhone 上复制了验证码/文字 → 控制中心点一下「推送到 Paste」→ 电脑剪贴板 1 秒内自动变成这段文字。

## 快捷指令 2：「从 Paste 拉取」（其他设备 → iPhone）

1. **获取 URL 内容**：
   - URL：`<SERVER>/api/clips/latest`
   - 方法：`GET`
   - 请求头：`Authorization` = `Bearer <TOKEN>`
2. **获取词典值**（Get Dictionary Value）：`text`（如果第 1 步没自动识别为词典，先加一个「获取词典值」并把输入指定为第 1 步的结果）
3. **拷贝到剪贴板**（Copy to Clipboard）：输入为第 2 步的 `text`

> 注意：服务端无记录时 `/api/clips/latest` 返回 204 空响应，快捷指令会在第 2 步报「无内容」——属正常，弹窗关掉即可；介意的话可在最前面加「如果」动作判空。

使用场景：在电脑上复制了文字 → iPhone 控制中心点「从 Paste 拉取」→ 直接去 App 里粘贴。

## Android 的等价用法

不做原生 App 的阶段：安装 PWA（浏览器菜单→添加到主屏幕），文本分享菜单直接选 PWA（Web Share Target）即上传；拉取则打开 PWA 点「复制」。

## 安全提醒

快捷指令里含设备令牌，等于一把对应你账号的钥匙；iCloud 同步快捷指令时注意不要公开分享。若设备令牌泄露，重新登录一次旧令牌不会失效，可在服务端更换管理令牌后于向导页视情况处理（多设备互不影响）。
