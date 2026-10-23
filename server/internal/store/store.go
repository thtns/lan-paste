// Package store 定义剪贴板条目的数据模型与存储接口。
// MVP 仅支持文本（type == "text"），但表结构与接口已为 image / file 预留扩展位。
package store

import (
	"context"
	"time"
)

// Clip 一条剪贴板记录。JSON 标签与 docs/PROTOCOL.md 中的 clip 对象严格一致。
// v1.1 起支持图片：文本存 Text，图片落盘（BlobPath）+ 元数据（Mime/Width/Height）。
type Clip struct {
	ID           int64     `json:"id"`
	Type         string    `json:"type"` // "text" / "image"（"file" 为保留字）
	Text         string    `json:"text"` // 文本内容；图片恒为空
	Hash         string    `json:"hash"` // 内容 SHA-256 小写 hex（图片为文件字节的 sha256）
	SourceDevice string    `json:"source_device"`
	Size         int64     `json:"size"`               // 字节数（文本为 UTF-8 长度，图片为文件字节数）
	Mime         string    `json:"mime,omitempty"`     // 图片 MIME，如 image/png
	Width        int       `json:"width,omitempty"`    // 图片宽（像素），未知为 0
	Height       int       `json:"height,omitempty"`   // 图片高（像素），未知为 0
	BlobPath     string    `json:"-"`                  // 图片落盘相对路径（相对数据目录），不出 JSON
	BlobURL      string    `json:"blob_url,omitempty"` // 图片下载地址（服务端派生，非持久化）
	UserID       int64     `json:"-"`                  // 所属用户（0 = 管理令牌/legacy），不出 JSON
	CreatedAt    time.Time `json:"created_at"`
}

// Store 存储接口，memstore（内存）与 pgstore（PostgreSQL）两个实现。
type Store interface {
	// Add 新增一条记录，并回填 ID 与 CreatedAt。
	Add(ctx context.Context, c *Clip) error
	// Latest 返回指定用户的最新一条记录；userID == 0 时跨全部用户（管理视角）。
	// 无记录时返回 (nil, nil)。
	Latest(ctx context.Context, userID int64) (*Clip, error)
	// List 按创建时间降序返回该用户最多 limit 条（userID == 0 时跨全部用户）。
	List(ctx context.Context, userID int64, limit int) ([]Clip, error)
	// Get 按 ID 查询单条；不存在返回 (nil, nil)。
	Get(ctx context.Context, id int64) (*Clip, error)
	// Delete 删除单条；返回是否确实删除了（目标不存在返回 false）。
	Delete(ctx context.Context, id int64) (bool, error)
	// AdoptLegacy 把历史遗留（UserID=0，共享令牌时代）的记录归入指定用户。
	// 供首个注册用户接管旧数据。
	AdoptLegacy(ctx context.Context, userID int64) error
	// DeleteOlderThan 删除早于 t 的所有记录，返回被删记录的附件相对路径
	// （文本条目无附件，不出现；供 TTL 清理连带删除落盘文件）。
	DeleteOlderThan(ctx context.Context, t time.Time) (paths []string, err error)
}
