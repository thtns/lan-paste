//go:build windows

package autostart

import (
	"fmt"
	"log"

	"golang.org/x/sys/windows/registry"
)

// 实现：当前用户注册表 Run 键，登录时由 Explorer 启动，无需管理员权限。

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func install(binPath string, lg *log.Logger) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer k.Close()
	// 值加引号，路径含空格时也能被正确解析
	if err := k.SetStringValue(Label, `"`+binPath+`"`); err != nil {
		return fmt.Errorf("写入注册表失败: %w", err)
	}
	lg.Printf("已安装开机自启（注册表 HKCU\\%s\\%s）", runKey, Label)
	return nil
}

func uninstall(lg *log.Logger) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("打开注册表 Run 键失败: %w", err)
	}
	defer k.Close()
	if err := k.DeleteValue(Label); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("删除注册表值失败: %w", err)
	}
	lg.Printf("已移除开机自启（注册表 HKCU\\%s\\%s）", runKey, Label)
	return nil
}
