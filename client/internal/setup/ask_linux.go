//go:build linux

package setup

import (
	"os"
	"os/exec"
	"strings"
)

// Linux：桌面会话装有 zenity 时用图形表单（GNOME 等发行版普遍预装），
// 否则回退终端问答。zenity --forms 不支持逐字段预填，代价可接受
// （Linux 本就是可选托盘平台，服务器场景走终端路径）。

func haveZenity() bool {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return false // 无图形会话
	}
	_, err := exec.LookPath("zenity")
	return err == nil
}

// askFormPlatform 优先 zenity 图形表单，失败/取消/不可用时回退终端。
func askFormPlatform(prefill Credentials) (Credentials, bool) {
	if haveZenity() {
		if c, ok := askFormZenity(prefill); ok {
			return c, true
		}
	}
	return askFormTerm(prefill)
}

func askFormZenity(prefill Credentials) (Credentials, bool) {
	out, err := exec.Command("zenity", "--forms",
		"--title=LAN Paste — 设置",
		"--text=填写服务器与账号（服务器地址缺 http:// 会自动补全）",
		"--add-entry=服务器地址",
		"--add-entry=用户名",
		"--add-password=密码",
		"--add-entry=设备名（默认 "+prefill.Device+"）",
	).Output()
	if err != nil {
		return Credentials{}, false // 取消（exit 1）或 zenity 故障 → 终端兜底
	}
	parts := strings.Split(strings.TrimRight(string(out), "\n"), "|")
	if len(parts) != 4 {
		return Credentials{}, false
	}
	c := Credentials{Server: parts[0], Username: parts[1], Password: parts[2], Device: parts[3]}
	if strings.TrimSpace(c.Device) == "" {
		c.Device = prefill.Device
	}
	return c, true
}

// alertOKPlatform 图形会话弹 zenity 错误框，否则打印到终端。
func alertOKPlatform(title, message string) {
	if haveZenity() {
		_ = exec.Command("zenity", "--error", "--title="+title, "--text="+message).Run()
		return
	}
	alertTerm(title, message)
}

// notifyConfigSaved Linux 无需额外反馈（终端日志已打印，托盘构建有状态菜单）。
func notifyConfigSaved() {}

// askRetryPlatform zenity 三按钮问题，回退终端。
func askRetryPlatform(errMsg string) retryAction {
	if haveZenity() {
		out, err := exec.Command("zenity", "--question",
			"--title=LAN Paste 登录失败",
			"--text=登录失败："+errMsg+"\n\n选择「重试」重新填写，或「注册并登录」创建新账号。",
			"--ok-label=重试", "--cancel-label=取消", "--extra-button=注册并登录",
		).Output()
		if err == nil {
			return actionRetry
		}
		if strings.TrimSpace(string(out)) == "注册并登录" {
			return actionRegister
		}
		return actionCancel
	}
	return askRetryOrRegisterTerm(errMsg)
}
