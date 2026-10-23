//go:build darwin

package autostart

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 实现：登录项用 LaunchAgents plist（比 osascript 操作 System Events 更简单可靠，
// 且不触发自动化权限弹窗）。KeepAlive 保证进程异常退出后 launchd 自动拉起。

const plistLabel = "com.lanpaste.client"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", plistLabel+".plist"), nil
}

// xmlEscape 转义二进制路径中可能出现的 XML 特殊字符。
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func install(binPath string, lg *log.Logger) error {
	plist, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	logDir := filepath.Join(home, "Library", "Logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}

	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <!-- 仅崩溃（非 0 退出）时自动拉起；托盘菜单「退出」的正常退出不会被重启 -->
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, plistLabel, xmlEscape(binPath),
		xmlEscape(filepath.Join(logDir, Label+".out.log")),
		xmlEscape(filepath.Join(logDir, Label+".err.log")))

	if err := os.WriteFile(plist, []byte(content), 0o644); err != nil {
		return err
	}
	// 先 unload 再 load，保证重复 -install 时拿到最新配置
	_ = exec.Command("launchctl", "unload", plist).Run()
	if out, err := exec.Command("launchctl", "load", plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load 失败: %w: %s", err, strings.TrimSpace(string(out)))
	}
	lg.Printf("已安装开机自启（launchd）：%s", plist)
	lg.Printf("日志：%s / %s", filepath.Join(logDir, Label+".out.log"), filepath.Join(logDir, Label+".err.log"))
	return nil
}

func uninstall(lg *log.Logger) error {
	plist, err := plistPath()
	if err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "unload", plist).CombinedOutput(); err != nil {
		// 未加载时 unload 会失败，属正常情况，仅在 plist 仍存在时提示
		if _, statErr := os.Stat(plist); statErr == nil {
			lg.Printf("提示：launchctl unload 返回 %v: %s（可能本就未加载）", err, strings.TrimSpace(string(out)))
		}
	}
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	lg.Printf("已移除开机自启：%s", plist)
	return nil
}
