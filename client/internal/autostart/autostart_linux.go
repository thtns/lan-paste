//go:build linux

package autostart

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
)

// 实现：systemd 用户服务。只写入单元文件并提示用户执行 enable --now，
// 不直接调用 systemctl（无 systemd 的桌面环境里调用只会徒增报错，
// 且 enable 由用户显式执行更可控）。

const unitContent = `[Unit]
Description=LAN Paste clipboard sync client
After=graphical-session.target

[Service]
ExecStart=%s
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
`

func unitPath() (string, error) {
	cfgDir, err := os.UserConfigDir() // ~/.config（XDG_CONFIG_HOME 可覆盖）
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "systemd", "user", Label+".service"), nil
}

func install(binPath string, lg *log.Logger) error {
	path, err := unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf(unitContent, strconv.Quote(binPath))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	lg.Printf("已写入 systemd 用户服务：%s", path)
	lg.Printf("请执行以下命令启用（如已启用会自动重启服务）：")
	lg.Printf("  systemctl --user enable --now %s.service", Label)
	lg.Printf("查看日志：journalctl --user -u %s.service -f", Label)
	return nil
}

func uninstall(lg *log.Logger) error {
	path, err := unitPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	lg.Printf("已删除 %s", path)
	lg.Printf("如服务正在运行，请执行：systemctl --user disable --now %s.service", Label)
	return nil
}
