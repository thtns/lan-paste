//go:build !darwin && !windows && !linux

package setup

// 其余平台（BSD 等）：无原生对话框约定，统一终端问答。
// 托盘构建在这些平台本就不可用，终端即“程序内”。

func askFormPlatform(prefill Credentials) (Credentials, bool) { return askFormTerm(prefill) }

func alertOKPlatform(title, message string) { alertTerm(title, message) }

func askRetryPlatform(errMsg string) retryAction { return askRetryOrRegisterTerm(errMsg) }

func notifyConfigSaved() {}
