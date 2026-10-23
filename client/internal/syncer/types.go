// Package syncer 实现与平台无关的剪贴板同步引擎：
// 轮询本地剪贴板上传（REST），接收服务端 WebSocket 推送写入本地。
// 决策逻辑（LoopGuard）与 IO（剪贴板、网络）严格分离，便于单测。
package syncer

import (
	"crypto/sha256"
	"encoding/hex"
)

// Clip 对应 PROTOCOL.md 中 REST/WebSocket 共用的 clip 对象。
// v1.1 协议为图片条目新增 mime / width / height / blob_url（text 条目为空值）。
type Clip struct {
	ID           int64  `json:"id"`
	Type         string `json:"type"`
	Text         string `json:"text"`
	Hash         string `json:"hash"`
	SourceDevice string `json:"source_device"`
	Size         int64  `json:"size"`
	BlobURL      string `json:"blob_url"`
	CreatedAt    string `json:"created_at"`
}

// wsMessage 是 /ws 上的 JSON 文本帧：
// {"event":"new","clip":{...}} / {"event":"deleted","id":42} / {"event":"hello",...}
type wsMessage struct {
	Event       string `json:"event"`
	Clip        *Clip  `json:"clip,omitempty"`
	ID          int64  `json:"id,omitempty"`
	Server      string `json:"server,omitempty"`
	DeviceCount int    `json:"device_count,omitempty"`
}

// uploadRequest 是 POST /api/clips 的请求体。
type uploadRequest struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Hash   string `json:"hash"`
	Device string `json:"device"`
}

// HashText 返回文本 UTF-8 字节的 sha256 小写 hex（与 PROTOCOL.md 一致）。
func HashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// HashBytes 返回任意字节（图片文件内容）的 sha256 小写 hex。
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// shortHash 生成日志用的短 hash。
func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8] + "…"
	}
	return h
}
