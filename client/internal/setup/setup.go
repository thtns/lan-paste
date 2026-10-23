// Package setup 提供客户端的应用内设置向导（不经浏览器）：
// 用系统原生对话框（macOS osascript / Windows WinForms / Linux zenity，
// 均不可用时回退终端问答）收集服务器地址与账号密码，登录换取设备令牌，
// 验证通过后交给调用方写盘。平台界面由 ask_*.go 提供，本文件只做流程编排。
package setup

import (
	"errors"
	"net/url"
	"strings"
)

// Credentials 是向导表单收集到的原始输入。
type Credentials struct {
	Server   string
	Username string
	Password string // 仅存在于进程内存，不落盘、不进日志
	Device   string
	Register bool // 表单上勾选了「注册新用户」（Windows 表单支持勾选；其余平台经确认对话框触发）
}

// Result 是一次成功配置的产物（写入 config.json 的字段）。
type Result struct {
	Server   string
	Token    string
	Device   string
	Username string
}

// ErrCancelled 表示用户在向导中点了取消（或终端输入 EOF），不是故障。
var ErrCancelled = errors.New("已取消设置")

// retryAction 是登录失败后的用户选择。
type retryAction int

const (
	actionCancel   retryAction = iota // 取消向导
	actionRetry                       // 重新填写
	actionRegister                    // 注册新账号后重试
)

// 平台交互入口（ask_*.go 提供具体实现；变量化便于单测注入）。
var (
	askForm            = askFormPlatform
	alertOK            = alertOKPlatform
	askRetryOrRegister = askRetryPlatform
)

// NotifyConfigSaved 在配置保存成功、同步即将开始时给出可见反馈。
// Windows 上托盘图标常被收进「^」折叠区，没有这一步用户会以为保存没生效。
func NotifyConfigSaved() { notifyConfigSaved() }

// Run 运行设置向导：收集输入 → （可选）注册 → 登录 → 验证连通，
// 失败时在向导内提示并允许重试，直到成功或用户取消。
// prefill 为预填值（上一次的配置或当前输入），Device 为空时填 hostname。
func Run(prefill Credentials) (Result, error) {
	creds := prefill
	for {
		c, ok := askForm(creds)
		if !ok {
			return Result{}, ErrCancelled
		}
		creds = c

		server := NormalizeServer(creds.Server)
		if server == "" {
			alertOK("服务器地址无效", "示例：http://192.168.1.10:8080\n请检查后重试。")
			continue
		}
		username := strings.TrimSpace(creds.Username)
		if username == "" || creds.Password == "" {
			alertOK("缺少账号信息", "用户名与密码都需要填写。")
			continue
		}
		device := strings.TrimSpace(creds.Device)
		if device == "" {
			device = prefill.Device
		}
		if device == "" {
			device = "unknown"
		}
		if len(device) > 32 {
			device = device[:32]
		}

		if creds.Register {
			if err := registerAccount(server, username, creds.Password); err != nil {
				// 用户名已存在（比如之前注册过）：当作已注册，直接登录
				if !strings.Contains(err.Error(), "已存在") {
					alertOK("注册失败", err.Error())
					continue
				}
			}
			creds.Register = false // 消费一次；重填表单时按新勾选来
		}
		token, err := login(server, username, creds.Password, device)
		if err == nil {
			if err := verify(server, token); err != nil {
				alertOK("连接验证失败", err.Error()+"\n请检查服务器地址与网络后重试。")
				continue
			}
			return Result{Server: server, Token: token, Device: device, Username: username}, nil
		}
		// 登录失败：重新填写 / 注册新账号（用同一凭据直接注册再登录）/ 取消
		switch askRetryOrRegister(err.Error()) {
		case actionRetry:
			continue
		case actionRegister:
			if err := registerAccount(server, username, creds.Password); err != nil &&
				!strings.Contains(err.Error(), "已存在") {
				alertOK("注册失败", err.Error())
				continue
			}
			token, err = login(server, username, creds.Password, device)
			if err != nil {
				alertOK("登录失败", err.Error())
				continue
			}
			if err := verify(server, token); err != nil {
				alertOK("连接验证失败", err.Error()+"\n请检查服务器地址与网络后重试。")
				continue
			}
			return Result{Server: server, Token: token, Device: device, Username: username}, nil
		default: // 取消
			return Result{}, ErrCancelled
		}
	}
}

// NormalizeServer 补全缺省 scheme 并规范化地址；无效返回空串。
// "10.0.0.5:8080" → "http://10.0.0.5:8080"。
func NormalizeServer(input string) string {
	s := strings.TrimSpace(input)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
