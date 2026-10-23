// download.go — /download 手机扫码接入页。
//
// 多用户（v1.2）后认证走账号登录，二维码只编码服务端地址：
// 手机扫码打开 → 浏览器登录页输入用户名密码（本机记住），无需分发任何令牌。
package main

import (
	"encoding/base64"
	"fmt"
	"html"
	"net/http"

	"github.com/skip2/go-qrcode"
)

func handleDownload(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host
	connectURL := base
	tokenNote := "扫码后在登录页输入用户名与密码（本机会记住登录状态）。"

	png, err := qrcode.Encode(connectURL, qrcode.Medium, 320)
	qrURI := ""
	if err == nil {
		qrURI = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	}

	page := fmt.Sprintf(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>LAN Paste — 扫码接入</title>
<style>
  body{margin:0;background:#f4f5f7;color:#1f2430;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;
       display:grid;place-items:center;min-height:100vh}
  .card{background:#fff;border:1px solid #e5e7eb;border-radius:16px;box-shadow:0 1px 3px rgb(16 24 40/.08);
        padding:28px 32px;max-width:520px;margin:24px;text-align:center}
  h1{margin:0 0 4px;font-size:19px}
  p{margin:6px 0;font-size:13.5px;color:#6b7280;line-height:1.7}
  img.qr{width:260px;height:260px;image-rendering:pixelated;border:1px solid #e5e7eb;border-radius:12px;margin:12px 0}
  code{background:#f8f9fb;border:1px solid #e5e7eb;border-radius:6px;padding:2px 6px;font-size:12px;word-break:break-all}
  .sec{text-align:left;margin-top:18px;padding-top:14px;border-top:1px dashed #e5e7eb}
  .sec h2{font-size:14px;margin:0 0 6px}
  pre{background:#1f2430;color:#e5e7eb;border-radius:10px;padding:10px 12px;font-size:12px;overflow-x:auto;line-height:1.7}
  a{color:#4f46e5;text-decoration:none}
</style>
</head>
<body>
  <div class="card">
    <h1>LAN Paste 扫码接入</h1>
    <p>用手机相机 / 浏览器扫码，在手机上打开同一个剪贴板</p>
    %s
    <p style="font-size:12px;color:#9ca3af">%s</p>
    <p>地址:<code>%s</code></p>

    <div class="sec">
      <h2>桌面客户端（可选，复制即全自动同步）</h2>
      <pre># macOS / Linux（编译或下载后）
./lanpaste-client -server %q -token &lt;你的Token&gt; -device my-mac
./lanpaste-client -install   # 注册开机自启</pre>
      <p>构建方法见项目 <a href="/">client/README.md</a>；网页端无需安装任何东西。</p>
    </div>
    <p style="margin-top:16px"><a href="/">← 返回主页面</a></p>
  </div>
</body>
</html>`,
		func() string {
			if qrURI == "" {
				return `<p style="color:#dc2626">二维码生成失败</p>`
			}
			return `<img class="qr" alt="接入二维码" src="` + qrURI + `">`
		}(),
		html.EscapeString(tokenNote),
		html.EscapeString(base),
		base,
	)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(page))
}
