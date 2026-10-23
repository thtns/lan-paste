//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"
)

// alertGUI 弹出 Windows 系统消息框并阻塞等待点击（调用方决定何时使用）。
func alertGUI(msg string) {
	showMessageBox(msg)
}

// alertGUIAsync 弹出消息框但不阻塞调用方：对话框在自己的线程显示，
// 调用方先睡 3 秒确保其渲染出来，然后继续执行（适合「已在运行」这类
// 不应卡住进程的提示）。
func alertGUIAsync(msg string) {
	go showMessageBox(msg)
	time.Sleep(3 * time.Second)
}

func showMessageBox(msg string) {
	u32 := syscall.NewLazyDLL("user32.dll")
	box := u32.NewProc("MessageBoxW")
	title, _ := syscall.UTF16PtrFromString("LAN Paste")
	text, _ := syscall.UTF16PtrFromString(msg)
	const mbOKIconInfo = 0x40
	_, _, _ = box.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), mbOKIconInfo)
}
