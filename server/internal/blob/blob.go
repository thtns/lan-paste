// Package blob 管理图片附件的本地落盘：
// 数据目录下按 files/20060102/<sha256>.<ext> 组织（按日分目录，内容寻址天然去重）。
// 大对象一律不进数据库（PLAN.md 的既定原则），数据库只存相对路径 BlobPath。
package blob

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Manager 以 root（绝对路径）为根的附件目录管理器，并发安全（依赖 os 原子语义）。
type Manager struct {
	root string
}

// New 创建附件根目录（含父目录）。root 应位于数据目录下，如 /var/lib/lanpaste/files。
func New(root string) (*Manager, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("blob: 创建附件目录 %s 失败: %w", root, err)
	}
	return &Manager{root: root}, nil
}

// Validate 校验相对路径合法（防目录穿越：只允许字母数字与 / - _ .，且不允许 ..）。
func Validate(rel string) error {
	if rel == "" {
		return fmt.Errorf("blob: 空路径")
	}
	if strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return fmt.Errorf("blob: 非法路径 %q", rel)
	}
	return nil
}

// Save 把 r 的内容写入 root/rel（自动创建父目录）。调用方负责保证 rel 与内容一致
// （约定 rel 即 files/<日期>/<sha256>.<ext>，天然幂等：同内容重复写覆盖同一路径）。
func (m *Manager) Save(rel string, r io.Reader) error {
	if err := Validate(rel); err != nil {
		return err
	}
	full := filepath.Join(m.root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("blob: 创建目录失败: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(full), ".upload-*")
	if err != nil {
		return fmt.Errorf("blob: 创建临时文件失败: %w", err)
	}
	tmp := f.Name()
	_, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil {
			return fmt.Errorf("blob: 写入失败: %w", copyErr)
		}
		return fmt.Errorf("blob: 关闭临时文件失败: %w", closeErr)
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("blob: 落盘失败: %w", err)
	}
	return nil
}

// Open 打开 root/rel，调用方负责 Close。不存在返回错误。
func (m *Manager) Open(rel string) (*os.File, error) {
	if err := Validate(rel); err != nil {
		return nil, err
	}
	return os.Open(filepath.Join(m.root, rel))
}

// Delete 删除 root/rel；不存在视为成功（幂等）。其余错误照常返回（由调用方记日志）。
func (m *Manager) Delete(rel string) error {
	if err := Validate(rel); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(m.root, rel)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
