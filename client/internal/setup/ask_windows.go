//go:build windows

package setup

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Windows 用系统自带 PowerShell + WinForms 弹一个原生设置窗口
// （服务器地址 / 用户名 / 密码 / 设备名 / 注册勾选，一次填完）。
// 脚本写成带 BOM 的 UTF-8 临时 .ps1（PowerShell 5.1 靠 BOM 识别中文），
// 预填值经环境变量传入（Windows 环境变量是 UTF-16，无转义问题），
// 结果以 JSON 打到 stdout；CREATE_NO_WINDOW 防止控制台黑框闪烁。

const psForm = `
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing

$form = New-Object System.Windows.Forms.Form
$form.Text = 'LAN Paste — 设置'
$form.FormBorderStyle = [System.Windows.Forms.FormBorderStyle]::FixedDialog
$form.StartPosition = 'CenterScreen'
$form.MaximizeBox = $false
$form.MinimizeBox = $false
$form.TopMost = $true
$form.ShowInTaskbar = $false
$form.Font = New-Object System.Drawing.Font('Segoe UI', 9)
$form.ClientSize = New-Object System.Drawing.Size(400, 252)

function New-Label([string]$text, [int]$y) {
    $l = New-Object System.Windows.Forms.Label
    $l.Text = $text
    $l.Location = New-Object System.Drawing.Point(14, ($y + 3))
    $l.AutoSize = $true
    $form.Controls.Add($l)
}
function New-Input([int]$y, [string]$text, [bool]$secret) {
    $t = New-Object System.Windows.Forms.TextBox
    $t.Location = New-Object System.Drawing.Point(100, $y)
    $t.Size = New-Object System.Drawing.Size(280, 23)
    $t.Text = $text
    if ($secret) { $t.UseSystemPasswordChar = $true }
    $form.Controls.Add($t)
    return $t
}

New-Label '服务器地址' 14
$inServer = New-Input 12  $env:LANPASTE_SERVER $false
New-Label '用户名' 48
$inUser   = New-Input 46  $env:LANPASTE_USERNAME $false
New-Label '密码' 82
$inPass   = New-Input 80  '' $true
New-Label '设备名' 116
$inDevice = New-Input 114 $env:LANPASTE_DEVICE $false

$chkRegister = New-Object System.Windows.Forms.CheckBox
$chkRegister.Text = '注册新用户（已有账号请勿勾选）'
$chkRegister.Location = New-Object System.Drawing.Point(100, 146)
$chkRegister.AutoSize = $true
$form.Controls.Add($chkRegister)

$btnOK = New-Object System.Windows.Forms.Button
$btnOK.Text = '保存并连接'
$btnOK.Location = New-Object System.Drawing.Point(190, 190)
$btnOK.Size = New-Object System.Drawing.Size(90, 30)
$btnOK.DialogResult = [System.Windows.Forms.DialogResult]::OK
$form.AcceptButton = $btnOK
$btnCancel = New-Object System.Windows.Forms.Button
$btnCancel.Text = '取消'
$btnCancel.Location = New-Object System.Drawing.Point(288, 190)
$btnCancel.Size = New-Object System.Drawing.Size(90, 30)
$btnCancel.DialogResult = [System.Windows.Forms.DialogResult]::Cancel
$form.CancelButton = $btnCancel
$form.Controls.Add($btnOK)
$form.Controls.Add($btnCancel)
$form.Add_Shown({ $form.Activate() })

if ($form.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) {
    $form.Hide(); $form.Dispose()
    [pscustomobject]@{
        cancelled = $false
        server    = $inServer.Text
        username  = $inUser.Text
        password  = $inPass.Text
        device    = $inDevice.Text
        register  = $chkRegister.Checked
    } | ConvertTo-Json -Compress
} else {
    '{"cancelled":true}'
}
`

type psFormResult struct {
	Cancelled bool   `json:"cancelled"`
	Server    string `json:"server"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Device    string `json:"device"`
	Register  bool   `json:"register"`
}

// runPS 把脚本写入带 BOM 的临时 .ps1 并执行，返回 stdout。
// CREATE_NO_WINDOW：本程序是无控制台的 windowsgui 进程，必须避免
// 系统给子进程 powershell 新开一个控制台窗口。
func runPS(script string, env map[string]string) (string, error) {
	tmp, err := os.CreateTemp("", "lanpaste-*.ps1")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append([]byte{0xEF, 0xBB, 0xBF}, []byte(script)...)); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	cmd := exec.Command("powershell.exe",
		"-NoProfile", "-NonInteractive", "-STA", "-ExecutionPolicy", "Bypass", "-File", tmpName)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	e := os.Environ()
	for k, v := range env {
		e = append(e, k+"="+v)
	}
	cmd.Env = e
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("powershell: %v：%s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("powershell: %v", err)
	}
	return string(out), nil
}

// askFormPlatform 弹出 WinForms 设置窗口（预填当前值）；取消返回 ok=false。
func askFormPlatform(prefill Credentials) (Credentials, bool) {
	out, err := runPS(psForm, map[string]string{
		"LANPASTE_SERVER":   prefill.Server,
		"LANPASTE_USERNAME": prefill.Username,
		"LANPASTE_DEVICE":   prefill.Device,
	})
	if err != nil {
		return Credentials{}, false
	}
	var res psFormResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err != nil || res.Cancelled {
		return Credentials{}, false
	}
	return Credentials{
		Server:   res.Server,
		Username: res.Username,
		Password: res.Password,
		Device:   res.Device,
		Register: res.Register,
	}, true
}

// alertOKPlatform 弹系统消息框（TopMost 属主，避免被其他窗口压住）。
func alertOKPlatform(title, message string) {
	_, _ = runPS(`Add-Type -AssemblyName System.Windows.Forms
$top = New-Object System.Windows.Forms.Form
$top.TopMost = $true
[System.Windows.Forms.MessageBox]::Show($top, `+psQuote(message)+`, `+psQuote(title)+`, 'OK', 'Warning') | Out-Null
$top.Dispose()`, nil)
}

// askRetryPlatform 登录失败后询问：是=注册并登录，否=重新填写，取消=退出向导。
func askRetryPlatform(errMsg string) retryAction {
	out, err := runPS(`Add-Type -AssemblyName System.Windows.Forms
$top = New-Object System.Windows.Forms.Form
$top.TopMost = $true
switch ([System.Windows.Forms.MessageBox]::Show($top, `+psQuote(errMsg+"\n\n【是】注册新账号并登录    【否】重新填写    【取消】退出设置")+`, 'LAN Paste 登录失败', 'YesNoCancel', 'Warning')) {
  'Yes'    { 'register' }
  'No'     { 'retry' }
  'Cancel' { 'cancel' }
}
$top.Dispose()`, nil)
	if err != nil {
		return actionCancel
	}
	switch strings.TrimSpace(out) {
	case "register":
		return actionRegister
	case "retry":
		return actionRetry
	default:
		return actionCancel
	}
}

// notifyConfigSaved 保存成功后的可见确认：托盘图标在 Win11 常被收进
// 任务栏「^」折叠区，不弹这个框用户会以为「点了保存没反应」。
func notifyConfigSaved() {
	_, _ = runPS(`Add-Type -AssemblyName System.Windows.Forms
$top = New-Object System.Windows.Forms.Form
$top.TopMost = $true
[System.Windows.Forms.MessageBox]::Show($top, '已连接并保存，剪贴板同步已开始！

提示：托盘图标可能收在任务栏右下角「^」折叠区，可将其拖到任务栏常显。', 'LAN Paste 已连接', 'OK', 'Information') | Out-Null
$top.Dispose()`, nil)
}

// psQuote 把任意文本安全包成 PowerShell 单引号字符串（单引号加倍转义）。
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
