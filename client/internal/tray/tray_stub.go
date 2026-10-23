//go:build notray || (!darwin && !windows && !tray)

package tray

import "fmt"

// Options 托盘初始化参数（本构建未启用托盘，仅为编译占位）。
type Options struct {
	Device       string
	ServerURL    string
	Token        string
	OnPause      func(pause bool)
	Unconfigured bool
	OnSetup      func()
}

// Supported 报告当前平台/构建是否提供托盘界面：未启用。
func Supported() bool { return false }

// Run 占位：当前构建不含托盘（可用 -tags tray 构建启用）。
func Run(o Options) error {
	return fmt.Errorf("当前构建未包含托盘界面（Linux 需 -tags tray 且安装 gtk）")
}

// SetStatus 占位。
func SetStatus(connected bool, devices int) {}

// Quit 占位。
func Quit() {}
