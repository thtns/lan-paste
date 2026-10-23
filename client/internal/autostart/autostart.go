// Package autostart 负责注册/移除开机自启，按平台分别实现：
//
//	darwin  ：~/Library/LaunchAgents/ plist + launchctl load
//	windows ：注册表 HKCU\Software\Microsoft\Windows\CurrentVersion\Run
//	linux   ：~/.config/systemd/user/lanpaste.service（提示用户 systemctl --user enable）
//
// 自启条目运行的是本二进制本身（不带参数），配置从 ~/.config/lanpaste/config.json 读取，
// 因此 -install 前必须已有完整配置。
package autostart

import "log"

// Label 是自启条目的统一标识：
// macOS 的 launchd Label、Windows 的注册表值名、Linux 的 systemd 单元名都基于它。
const Label = "lanpaste-client"

// Install 注册 binPath（当前二进制的绝对路径）的开机自启。
func Install(binPath string, log *log.Logger) error { return install(binPath, log) }

// Uninstall 移除开机自启。
func Uninstall(log *log.Logger) error { return uninstall(log) }
