package memstore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"lanpaste/server/internal/store"
)

func addClip(t *testing.T, m *MemStore, text string) *store.Clip {
	t.Helper()
	c := &store.Clip{Type: "text", Text: text, Hash: text, Size: int64(len(text)), SourceDevice: "test"}
	if err := m.Add(context.Background(), c); err != nil {
		t.Fatalf("Add(%q) 出错: %v", text, err)
	}
	return c
}

func TestMemStoreBasic(t *testing.T) {
	m := newWithCap(10)
	ctx := context.Background()

	// 空库：Latest / Get 均为 nil，List 为空
	if c, _ := m.Latest(ctx, 0); c != nil {
		t.Fatalf("空库 Latest 应为 nil，得到 %+v", c)
	}
	if c, _ := m.Get(ctx, 1); c != nil {
		t.Fatalf("空库 Get 应为 nil")
	}
	if clips, _ := m.List(ctx, 0, 50); len(clips) != 0 {
		t.Fatalf("空库 List 应为空")
	}

	// Add 回填 ID / CreatedAt
	c1 := addClip(t, m, "a")
	if c1.ID != 1 || c1.CreatedAt.IsZero() {
		t.Fatalf("Add 应回填 ID 与 CreatedAt，得到 id=%d created=%v", c1.ID, c1.CreatedAt)
	}
	c2 := addClip(t, m, "b")
	if c2.ID != 2 {
		t.Fatalf("ID 应递增，得到 %d", c2.ID)
	}

	// Latest 是最后一条
	if l, _ := m.Latest(ctx, 0); l.Text != "b" {
		t.Fatalf("Latest 应为 b，得到 %q", l.Text)
	}

	// List 降序
	clips, _ := m.List(ctx, 0, 10)
	if len(clips) != 2 || clips[0].Text != "b" || clips[1].Text != "a" {
		t.Fatalf("List 应按时间降序 [b a]，得到 %v", clips)
	}

	// List limit 生效
	if clips, _ := m.List(ctx, 0, 1); len(clips) != 1 || clips[0].Text != "b" {
		t.Fatalf("List(1) 应只返回最新一条，得到 %v", clips)
	}

	// Get
	if g, _ := m.Get(ctx, 1); g == nil || g.Text != "a" {
		t.Fatalf("Get(1) 应返回 a")
	}
	if g, _ := m.Get(ctx, 99); g != nil {
		t.Fatalf("Get(99) 应为 nil")
	}

	// Delete
	if ok, _ := m.Delete(ctx, 1); !ok {
		t.Fatalf("Delete(1) 应返回 true")
	}
	if ok, _ := m.Delete(ctx, 1); ok {
		t.Fatalf("重复 Delete(1) 应返回 false")
	}
	if g, _ := m.Get(ctx, 1); g != nil {
		t.Fatalf("删除后 Get(1) 应为 nil")
	}
}

func TestMemStoreEviction(t *testing.T) {
	m := newWithCap(5)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		addClip(t, m, fmt.Sprintf("clip-%d", i))
	}
	// 容量 5：应只剩 id 4..8（淘汰最旧 3 条）
	clips, _ := m.List(ctx, 0, 100)
	if len(clips) != 5 {
		t.Fatalf("容量 5 时应只剩 5 条，得到 %d 条", len(clips))
	}
	if clips[0].Text != "clip-7" || clips[4].Text != "clip-3" {
		t.Fatalf("淘汰后应保留最新 5 条（clip-3..clip-7），得到 %v", clips)
	}
	if c, _ := m.Get(ctx, 2); c != nil {
		t.Fatalf("最旧的 clip-0/1/2 应已被淘汰")
	}
}

func TestMemStoreDeleteOlderThan(t *testing.T) {
	m := newWithCap(100)
	ctx := context.Background()
	old := addClip(t, m, "old")
	// 手工把第一条的时间拨回 2 小时前
	m.mu.Lock()
	m.items[0].CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	m.mu.Unlock()
	addClip(t, m, "new")

	paths, err := m.DeleteOlderThan(ctx, time.Now().UTC().Add(-time.Hour))
	if err != nil || len(paths) != 0 {
		t.Fatalf("DeleteOlderThan 应删除 1 条（文本无附件，paths 应为空），得到 paths=%v err=%v", paths, err)
	}
	if c, _ := m.Get(ctx, old.ID); c != nil {
		t.Fatalf("旧记录应已被删除")
	}
	if l, _ := m.Latest(ctx, 0); l.Text != "new" {
		t.Fatalf("最新记录应保留")
	}
}

// TestMemStoreConcurrent 并发读写：500 goroutine 各写 20 条，同时读者不断 List/Latest。
// 由 `go test -race` 验证无数据竞争；结束后校验总数与容量上限。
func TestMemStoreConcurrent(t *testing.T) {
	const (
		writers = 500
		perG    = 20
	)
	m := newWithCap(1000)
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读者
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = m.Latest(ctx, 0)
					_, _ = m.List(ctx, 0, 50)
					_ = ctx.Err()
				}
			}
		}()
	}

	// 写者
	var wwg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wwg.Add(1)
		go func(g int) {
			defer wwg.Done()
			for i := 0; i < perG; i++ {
				c := &store.Clip{Type: "text", Text: fmt.Sprintf("g%d-i%d", g, i), SourceDevice: "bench"}
				if err := m.Add(ctx, c); err != nil {
					t.Errorf("并发 Add 出错: %v", err)
					return
				}
			}
		}(g)
	}
	wwg.Wait()
	close(stop)
	wg.Wait()

	clips, _ := m.List(ctx, 0, 2000)
	if len(clips) > 1000 {
		t.Fatalf("并发写入后条数 %d 超过容量上限 1000", len(clips))
	}
	if len(clips) != 1000 {
		t.Fatalf("并发写入 %d 条（容量 %d），应恰好保留 1000 条，得到 %d", writers*perG, 1000, len(clips))
	}
	// ID 单调无重复：降序列表上 id 应严格递减
	for i := 1; i < len(clips); i++ {
		if clips[i-1].ID <= clips[i].ID {
			t.Fatalf("ID 应严格降序且无重复：clips[%d].id=%d <= clips[%d].id=%d", i-1, clips[i-1].ID, i, clips[i].ID)
		}
	}
}
