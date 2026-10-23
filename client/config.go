package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 是 ~/.config/lanpaste/config.json 的结构。
// token 属于敏感信息，文件权限 0600、目录 0700。
type Config struct {
	Server string `json:"server"`                     // 服务端地址，如 http://10.0.0.5:8080
	Token  string `json:"token"`                      // 认证凭据（设备令牌或管理令牌）
	Device string `json:"device,omitempty"`           // 设备名，空则运行时取 hostname
	User   string `json:"user,omitempty"`             // 账号名（展示用，v1.2 多用户）
	PollMS int    `json:"poll_interval_ms,omitempty"` // 轮询间隔（毫秒），空则 500
}

// defaultConfigPath 返回 ~/.config/lanpaste/config.json。
// 统一放在 ~/.config 而非各 OS 的惯例目录（macOS Application Support、
// Windows AppData），与 PROTOCOL/需求文档描述保持一致，三平台路径一致。
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("lanpaste", "config.json")
	}
	return filepath.Join(home, ".config", "lanpaste", "config.json")
}

// loadConfig 读取配置；文件不存在返回零值与 exists=false。
func loadConfig(path string) (cfg Config, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, true, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, true, fmt.Errorf("配置文件 %s 不是合法 JSON: %w", path, err)
	}
	return cfg, true, nil
}

// saveConfig 原子写回配置（先写临时文件再改名，避免写一半损坏）。
func saveConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 改名成功后 Remove 只是兜底空操作
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// normalizeServerURL 补全缺省 scheme 并去掉尾部斜杠：
// "10.0.0.5:8080" → "http://10.0.0.5:8080"。
func normalizeServerURL(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	if s != "" && !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return s
}

// printGuide 打印设置引导信息（仅在无图形环境且非交互运行时兜底出现）。
func printGuide(configPath string) {
	fmt.Printf(`当前环境无法弹出设置界面（无图形会话且非交互运行）。两种方式完成配置：

  1. 在有桌面/终端的环境直接运行本程序，会自动弹出设置对话框；
  2. 命令行直配（token 在服务端首次启动时打印，或存于其数据目录 token 文件）：

       lanpaste-client -server http://10.0.0.5:8080 -token <token>

说明：
  - 可选：-device 设备名（默认取 hostname）、-poll 本地轮询间隔（默认 500ms）
  - 配置将保存到 %s，之后直接运行 lanpaste-client 即可
  - 开机自启：lanpaste-client -install（需先有配置）；移除：-uninstall
`, configPath)
}
