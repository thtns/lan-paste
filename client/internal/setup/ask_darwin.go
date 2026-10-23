//go:build darwin

package setup

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// macOS 用 osascript 的 display dialog/alert 实现原生对话框：
// 系统自带、无需 GUI 框架、无签名问题。表单是 4 个连续对话框
// （AppleScript 单个对话框只支持一个输入框），一次 osascript 调用完成，
// 任一步取消即整体取消。预填值经环境变量传入（LANPASTE_*），不进 argv。

var errUserCanceled = fmt.Errorf("user canceled")

// osa 运行一段 AppleScript（经 stdin 传入，UTF-8），返回 stdout；
// 用户点取消（AppleScript 错误 -128）时返回 errUserCanceled。
func osa(script string, env map[string]string) (string, error) {
	cmd := exec.Command("osascript", "-")
	cmd.Stdin = strings.NewReader(script)
	if len(env) > 0 {
		e := os.Environ()
		for k, v := range env {
			e = append(e, k+"="+v)
		}
		cmd.Env = e
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := errb.String()
		if strings.Contains(msg, "User canceled") || strings.Contains(msg, "-128") {
			return "", errUserCanceled
		}
		return "", fmt.Errorf("osascript: %v：%s", err, strings.TrimSpace(msg))
	}
	return out.String(), nil
}

// asq 转义 AppleScript 字符串字面量中的 \ 与 "。
func asq(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// askFormPlatform 依次弹出 服务器地址 → 用户名 → 密码（隐藏输入）→ 设备名 四个
// 原生对话框；取消返回 ok=false。
func askFormPlatform(prefill Credentials) (Credentials, bool) {
	script := `
set s0 to system attribute "LANPASTE_SERVER"
set u0 to system attribute "LANPASTE_USERNAME"
set d0 to system attribute "LANPASTE_DEVICE"
set r to display dialog "服务器地址（缺 http:// 会自动补全）" default answer s0 with title "LAN Paste 设置（1/4）"
set s to text returned of r
set r to display dialog "用户名" default answer u0 with title "LAN Paste 设置（2/4）"
set u to text returned of r
set r to display dialog "密码（至少 6 位，输入显示为圆点）" default answer "" with hidden answer with title "LAN Paste 设置（3/4）"
set p to text returned of r
set r to display dialog "设备名（本机在其他设备上的显示名）" default answer d0 with title "LAN Paste 设置（4/4）"
set d to text returned of r
return s & linefeed & u & linefeed & p & linefeed & d
`
	out, err := osa(script, map[string]string{
		"LANPASTE_SERVER":   prefill.Server,
		"LANPASTE_USERNAME": prefill.Username,
		"LANPASTE_DEVICE":   prefill.Device,
	})
	if err != nil {
		return Credentials{}, false
	}
	// 行分隔的四段答案；输入框本身不允许换行，安全切分
	parts := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(parts) != 4 {
		return Credentials{}, false
	}
	return Credentials{Server: parts[0], Username: parts[1], Password: parts[2], Device: parts[3]}, true
}

// alertOKPlatform 弹原生错误提示（display alert，红色警示图标）。
func alertOKPlatform(title, message string) {
	_, _ = osa(`display alert `+appleQuote(title)+` message `+appleQuote(message)+` as critical`, nil)
}

// askRetryPlatform 登录失败后询问：重试 / 注册新账号并登录 / 取消。
func askRetryPlatform(errMsg string) retryAction {
	script := `display dialog ` + appleQuote("登录失败："+errMsg+"\n\n可选择重新填写，或注册一个新账号。") + `
	buttons {"取消", "注册并登录", "重新填写"} default button "重新填写" cancel button "取消" with icon caution with title "LAN Paste"
return button returned of result`
	out, err := osa(script, nil)
	if err != nil {
		return actionCancel
	}
	switch strings.TrimSpace(out) {
	case "注册并登录":
		return actionRegister
	default:
		return actionRetry
	}
}

// appleQuote 把任意文本安全包成 AppleScript 字符串字面量（转义 \ 与 "）。
func appleQuote(s string) string {
	return `"` + asq(s) + `"`
}

// notifyConfigSaved macOS 菜单栏图标立即可见且显示连接状态，无需额外弹窗。
func notifyConfigSaved() {}
