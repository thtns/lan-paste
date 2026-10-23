# LAN Paste v0.4.3

局域网剪贴板同步：服务端单二进制 + macOS / Windows / Linux 客户端 + 网页端。文本 + 图片，多用户账号，全自动双向同步，数据不出内网。

## 首次使用

1. 部署服务端（任意 Linux）：`deploy/install.sh remote user@host`，浏览器打开向导完成配置
2. 客户端下载对应平台安装包，首次打开弹**应用内原生设置对话框**（不开浏览器），填服务器地址 + 用户名密码，保存即开始同步
3. 手机浏览器打开服务端地址登录，可添加到主屏幕当 PWA

## 安全提示（开源项目无付费签名证书，属正常现象）

**macOS**：首次打开可能提示「已损坏，无法打开」或「无法验证开发者」。

- 右键 `LanPaste.app` →「打开」；或执行一次：

```bash
xattr -cr /Applications/LanPaste.app
```

**Windows**：SmartScreen 可能提示「Windows 已保护你的电脑」→ 「更多信息」→「仍要运行」。

不放心请核对下方 sha256 校验值，或用源码自行构建（`client/build-mac.sh`、`client/build-windows.sh` 一键打包）。

## 校验值

见 `SHA256SUMS.txt`。

## 更新内容（v0.4.x）

- v0.4.0：应用内原生设置向导（告别浏览器配置页）
- v0.4.1：修复 Windows 设置窗口「点了保存没反应」（按钮 DialogResult）；修复 GUI 进程日志不落盘（MultiWriter 顺序）
- v0.4.2：跨设备图片回环防护（像素级指纹）；网页端图片并发加载修复；托盘「打开历史页面」免登录直连
- v0.4.3：文本回环防护（换行归一化）+ 服务端归一化去重双保险
