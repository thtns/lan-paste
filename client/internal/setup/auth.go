package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpSetupClient 设置向导专用 HTTP 客户端（短超时：向导需要快速反馈）。
var httpSetupClient = &http.Client{Timeout: 8 * time.Second}

// registerAccount POST /api/auth/register 创建账号（密码至少 6 位）。
func registerAccount(server, username, password string) error {
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := httpSetupClient.Post(server+"/api/auth/register", "application/json; charset=utf-8", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("无法连接服务器：%v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return nil
	}
	return apiError(resp.StatusCode, data)
}

// login POST /api/auth/login 用账号密码换取本设备令牌。
func login(server, username, password, device string) (token string, err error) {
	body, _ := json.Marshal(map[string]string{
		"username": username, "password": password, "device": device,
	})
	resp, err := httpSetupClient.Post(server+"/api/auth/login", "application/json; charset=utf-8", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("无法连接服务器：%v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp.StatusCode, data)
	}
	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || parsed.Token == "" {
		return "", fmt.Errorf("服务端未返回设备令牌")
	}
	return parsed.Token, nil
}

// verify 保存前先确认：服务端可达（/healthz）且令牌有效（/api/clips/latest 非 401），
// 避免存下一份连不上的配置。
func verify(server, token string) error {
	resp, err := httpSetupClient.Get(server + "/healthz")
	if err != nil {
		return fmt.Errorf("无法连接服务器：%v", err)
	}
	resp.Body.Close()

	req, err := http.NewRequest(http.MethodGet, server+"/api/clips/latest", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = httpSetupClient.Do(req)
	if err != nil {
		return fmt.Errorf("验证令牌失败：%v", err)
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusUnauthorized:
		return fmt.Errorf("令牌校验失败（401）")
	default:
		return fmt.Errorf("服务端返回异常（HTTP %d）", resp.StatusCode)
	}
}

// apiError 解析服务端统一错误格式 {"error":"..."}。
func apiError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Errorf("%s", e.Error)
	}
	return fmt.Errorf("服务端返回异常（HTTP %d）", status)
}
