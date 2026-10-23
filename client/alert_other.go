//go:build !windows

package main

// alertGUI 非 Windows 平台无系统对话框（托盘菜单/终端日志已足够）。
func alertGUI(msg string) {}

// alertGUIAsync 非 Windows 平台占位。
func alertGUIAsync(msg string) {}
