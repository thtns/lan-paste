//go:build !darwin && !windows

package setup

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// 终端问答版向导（Linux 无 zenity / 无桌面、BSD、headless 等）：
// stdin 不是 TTY 时所有交互直接返回取消，由调用方降级为打印引导。

var stdin = bufio.NewReader(os.Stdin)

func stdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// askFormTerm 终端逐项提问；EOF / 非交互 → 取消。
func askFormTerm(prefill Credentials) (Credentials, bool) {
	if !stdinIsTTY() {
		return Credentials{}, false
	}
	ask := func(label, def string, secret bool) (string, bool) {
		if def != "" {
			fmt.Printf("%s [%s]: ", label, def)
		} else {
			fmt.Printf("%s: ", label)
		}
		var line string
		if secret && stdinIsTTY() {
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Println()
			if err != nil {
				return "", false
			}
			line = string(b)
		} else {
			l, err := stdin.ReadString('\n')
			if err != nil && l == "" {
				return "", false
			}
			line = l
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			line = def
		}
		return line, true
	}
	fmt.Println("=== LAN Paste 设置 ===")
	server, ok := ask("服务器地址（如 192.168.1.10:8080）", prefill.Server, false)
	if !ok {
		return Credentials{}, false
	}
	username, ok := ask("用户名", prefill.Username, false)
	if !ok {
		return Credentials{}, false
	}
	password, ok := ask("密码（至少 6 位，输入不回显）", "", true)
	if !ok {
		return Credentials{}, false
	}
	device, ok := ask("设备名", prefill.Device, false)
	if !ok {
		return Credentials{}, false
	}
	return Credentials{Server: server, Username: username, Password: password, Device: device}, true
}

// alertTerm 终端模式直接打印（醒目前缀）。
func alertTerm(title, message string) {
	fmt.Printf("✗ %s：%s\n", title, strings.ReplaceAll(message, "\n", " "))
}

// askRetryOrRegisterTerm 登录失败后的 r/g/c 三选一。
func askRetryOrRegisterTerm(errMsg string) retryAction {
	if !stdinIsTTY() {
		return actionCancel
	}
	for {
		fmt.Printf("登录失败：%s\n  [r] 重新填写  [g] 注册新账号并登录  [c] 取消\n请选择: ", errMsg)
		line, err := stdin.ReadString('\n')
		if err != nil && line == "" {
			return actionCancel
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "r", "":
			return actionRetry
		case "g":
			return actionRegister
		case "c", "q":
			return actionCancel
		}
	}
}
