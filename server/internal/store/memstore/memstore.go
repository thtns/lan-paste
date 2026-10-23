// Package memstore 提供 store.Store 的内存实现：带互斥锁，
// 容量达到上限后自动淘汰最旧的记录。适用于单机演示 / 测试，重启数据即失。
package memstore

import (
	"context"
	"sync"
	"time"

	"lanpaste/server/internal/store"
)

// defaultCap 默认容量上限（条）。
const defaultCap = 1000

// MemStore 内存存储，并发安全。
type MemStore struct {
	mu    sync.Mutex
	items []store.Clip // 按插入顺序（即 ID / 时间升序）存放
	seq   int64        // ID 分配器
	cap   int          // 容量上限
}

var _ store.Store = (*MemStore)(nil)

// New 创建默认容量（1000 条）的内存存储。
func New() *MemStore { return newWithCap(defaultCap) }

// newWithCap 指定容量构造，仅供包内测试使用。
func newWithCap(capacity int) *MemStore {
	if capacity <= 0 {
		capacity = defaultCap
	}
	return &MemStore{items: make([]store.Clip, 0, capacity), cap: capacity}
}

// Add 新增记录并回填 ID / CreatedAt；超过容量时淘汰最旧记录。
func (m *MemStore) Add(_ context.Context, c *store.Clip) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.seq++
	c.ID = m.seq
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	m.items = append(m.items, *c)
	if len(m.items) > m.cap {
		// 淘汰切片头部（最旧）。偶发的底层数组前部空洞由 append 再分配自然回收。
		excess := len(m.items) - m.cap
		m.items = m.items[excess:]
	}
	return nil
}

// Latest 返回指定用户的最新一条（userID == 0 时跨全部用户）；无记录返回 (nil, nil)。
func (m *MemStore) Latest(_ context.Context, userID int64) (*store.Clip, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := len(m.items) - 1; i >= 0; i-- {
		if userID == 0 || m.items[i].UserID == userID {
			c := m.items[i]
			return &c, nil
		}
	}
	return nil, nil
}

// List 按时间降序返回该用户最多 limit 条（userID == 0 时跨全部用户）。
func (m *MemStore) List(_ context.Context, userID int64, limit int) ([]store.Clip, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]store.Clip, 0, limit)
	for i := len(m.items) - 1; i >= 0 && len(out) < limit; i-- {
		if userID == 0 || m.items[i].UserID == userID {
			out = append(out, m.items[i])
		}
	}
	return out, nil
}

// Get 按 ID 查询；不存在返回 (nil, nil)。
func (m *MemStore) Get(_ context.Context, id int64) (*store.Clip, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.items {
		if m.items[i].ID == id {
			c := m.items[i]
			return &c, nil
		}
	}
	return nil, nil
}

// Delete 删除单条；目标不存在返回 false。
func (m *MemStore) Delete(_ context.Context, id int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i := range m.items {
		if m.items[i].ID == id {
			m.items = append(m.items[:i], m.items[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// AdoptLegacy 把 UserID=0 的历史记录归入指定用户。
func (m *MemStore) AdoptLegacy(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.items {
		if m.items[i].UserID == 0 {
			m.items[i].UserID = userID
		}
	}
	return nil
}

// DeleteOlderThan 删除早于 t 的记录，返回被删记录的附件路径（文本条目无附件）。
// items 本身按时间升序，从头扫描即可。
func (m *MemStore) DeleteOlderThan(_ context.Context, t time.Time) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cut := 0
	for cut < len(m.items) && m.items[cut].CreatedAt.Before(t) {
		cut++
	}
	var paths []string
	for i := 0; i < cut; i++ {
		if p := m.items[i].BlobPath; p != "" {
			paths = append(paths, p)
		}
	}
	if cut > 0 {
		m.items = m.items[cut:]
	}
	return paths, nil
}
