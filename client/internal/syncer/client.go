package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"lanpaste/client/internal/clipboard"
)

// restClient 是 /api/clips 的最小 REST 客户端（契约见 PROTOCOL.md）。
type restClient struct {
	baseURL string // 形如 http://host:port，不带尾部斜杠
	token   string
	device  string
	http    *http.Client
}

func newRESTClient(serverURL, token, device string) *restClient {
	return &restClient{
		baseURL: strings.TrimRight(serverURL, "/"),
		token:   token,
		device:  device,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// UploadClip 上传一条文本。
// dedup=true 表示服务端判定与库中最新一条 hash 相同（HTTP 200，未广播）；
// dedup=false 表示新建记录（HTTP 201）。认证失败等返回错误。
func (c *restClient) UploadClip(ctx context.Context, text string) (clip *Clip, dedup bool, err error) {
	reqBody, err := json.Marshal(uploadRequest{Type: "text", Text: text, Hash: HashText(text), Device: c.device})
	if err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/clips", bytes.NewReader(reqBody))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var got Clip
		if err := json.Unmarshal(body, &got); err != nil {
			return nil, false, fmt.Errorf("解析上传响应失败: %w", err)
		}
		return &got, resp.StatusCode == http.StatusOK, nil
	}
	// 统一错误格式 {"error": "..."}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error == "" {
		e.Error = strings.TrimSpace(string(body))
	}
	return nil, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error)
}

// ---- 图片（v0.2）----

// uploadImageRequest 是 POST /api/clips/image 的请求体（data 为 base64）。
type uploadImageRequest struct {
	Data   string `json:"data"`
	Device string `json:"device"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// UploadImage 上传一张图片（PNG 字节）。返回语义与 UploadClip 相同。
func (c *restClient) UploadImage(ctx context.Context, img *clipboard.Image) (clip *Clip, dedup bool, err error) {
	reqBody, err := json.Marshal(uploadImageRequest{
		Data:   base64.StdEncoding.EncodeToString(img.Data),
		Device: c.device,
		Width:  img.Width,
		Height: img.Height,
	})
	if err != nil {
		return nil, false, err
	}
	// 图片可能较大（≤16MB），给独立的长超时
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, c.baseURL+"/api/clips/image", bytes.NewReader(reqBody))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var got Clip
		if err := json.Unmarshal(body, &got); err != nil {
			return nil, false, fmt.Errorf("解析图片上传响应失败: %w", err)
		}
		return &got, resp.StatusCode == http.StatusOK, nil
	}
	return nil, false, httpError(resp.StatusCode, body)
}

// DownloadBlob 下载图片内容（GET /api/clips/{id}/blob）。
func (c *restClient) DownloadBlob(ctx context.Context, id int64) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet,
		fmt.Sprintf("%s/api/clips/%d/blob", c.baseURL, id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, httpError(resp.StatusCode, body)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBlobBytes))
	if err != nil {
		return nil, err
	}
	return data, nil
}

const maxBlobBytes = 20 << 20 // 与服务端图片上限（16MB）留出余量

// httpError 统一解析 {"error": "..."} 错误体。
func httpError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error == "" {
		e.Error = strings.TrimSpace(string(body))
	}
	return fmt.Errorf("HTTP %d: %s", status, e.Error)
}
