package syncer

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lanpaste/client/internal/clipboard"
)

// fakeClipboard 内存剪贴板：SetText 模拟用户手动复制（不计入 writes）。
type fakeClipboard struct {
	mu     sync.Mutex
	text   string
	writes int // WriteText 调用次数（引擎写入）
}

func (f *fakeClipboard) ReadText() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text, nil
}

func (f *fakeClipboard) WriteText(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text = s
	f.writes++
	return nil
}

func (f *fakeClipboard) SetText(s string) { // 模拟用户在别处复制
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text = s
}

func (f *fakeClipboard) Text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text
}

func (f *fakeClipboard) WriteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// 上传失败（服务端 500）时 prevPoll 不推进，下一轮自动重试。
func TestPollOnceRetriesAfterUploadFailure(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		fail := calls == 1
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(&Clip{ID: 1, Hash: HashText("retry me")})
	}))
	defer srv.Close()

	clip := &fakeClipboard{}
	clip.SetText("retry me")
	e := New(Options{ServerURL: srv.URL, Token: "t", Device: "test", PollInterval: time.Millisecond, Logger: quietLogger()}, clip)

	e.pollOnce(context.Background()) // 第一次：500，失败
	mu.Lock()
	if calls != 1 {
		mu.Unlock()
		t.Fatalf("应有 1 次调用，得到 %d", calls)
	}
	mu.Unlock()
	if e.guard.LastSeenHash() != "" {
		t.Fatalf("失败后不应更新 lastSeen")
	}

	e.pollOnce(context.Background()) // 第二次：重试成功
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("失败后应重试，期望 2 次调用，得到 %d", got)
	}
	if e.guard.LastSeenHash() != HashText("retry me") {
		t.Fatalf("成功后 lastSeen 应更新")
	}

	// 内容未再变化：不再上传
	e.pollOnce(context.Background())
	mu.Lock()
	got = calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("内容未变化不应再上传，期望仍为 2 次调用，得到 %d", got)
	}
}

// 收到与本地内容相同的推送 → 忽略，不写剪贴板，但更新 lastSeen。
func TestHandleRemoteIgnoresDuplicate(t *testing.T) {
	clip := &fakeClipboard{}
	clip.SetText("already here")
	e := New(Options{ServerURL: "http://127.0.0.1:1", Token: "t", Device: "test", PollInterval: time.Millisecond, Logger: quietLogger()}, clip)

	c := &Clip{ID: 7, Type: "text", Text: "already here", Hash: HashText("already here"), SourceDevice: "other"}
	e.handleRemote(context.Background(), c)

	if clip.WriteCount() != 0 {
		t.Fatalf("重复内容不应写入剪贴板")
	}
	if e.guard.LastSeenHash() != HashText("already here") {
		t.Fatalf("duplicate 分支应更新 lastSeen")
	}
}

// 收到不同内容的推送 → 写入并记录 lastWritten/prevPoll。
func TestHandleRemoteWritesNewContent(t *testing.T) {
	clip := &fakeClipboard{}
	clip.SetText("old local")
	e := New(Options{ServerURL: "http://127.0.0.1:1", Token: "t", Device: "test", PollInterval: time.Millisecond, Logger: quietLogger()}, clip)

	c := &Clip{ID: 8, Type: "text", Text: "fresh from server", Hash: HashText("fresh from server"), SourceDevice: "peer"}
	e.handleRemote(context.Background(), c)

	if clip.Text() != "fresh from server" {
		t.Fatalf("应写入新内容，得到 %q", clip.Text())
	}
	if e.guard.LastWrittenHash() != HashText("fresh from server") {
		t.Fatalf("应记录 lastWrittenHash")
	}
	// 写入后紧接一轮轮询：应命中 self-write 分支，不上传
	e.pollOnce(context.Background())
	if e.guard.LastSeenHash() != HashText("fresh from server") {
		t.Fatalf("self-write 分支应更新 lastSeen")
	}
}

// waitFor 轮询等待条件成立，超时失败。
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（%.1fs）：%s", d.Seconds(), what)
}

// ---- 图片通道（v0.2）----

// testPNG 一张 1x1 红色 PNG（合法图片）。
var testPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xde, 0x00, 0x00, 0x00,
	0x0c, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
	0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d, 0xb0, 0x00, 0x00, 0x00,
	0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

// imageClipboard 在文本剪贴板之上叠加图片能力（模拟支持图片的平台）。
type imageClipboard struct {
	fakeClipboard
	img    []byte // 当前剪贴板里的 PNG；nil 表示无图片
	writes int
}

func (f *imageClipboard) ReadImage() (*clipboard.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.img == nil {
		return nil, nil
	}
	return &clipboard.Image{Data: f.img, Width: 1, Height: 1}, nil
}

func (f *imageClipboard) WriteImage(img *clipboard.Image) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.img = img.Data
	f.writes++
	return nil
}

// 本地出现图片 → 轮询上传；内容不变不再传；远端图片事件 → 下载并写入。
func TestImagePollAndRemote(t *testing.T) {
	var mu sync.Mutex
	uploads, downloads := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/clips/image" && r.Method == http.MethodPost:
			uploads++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(&Clip{ID: 100, Type: "image", Hash: HashBytes(testPNG), BlobURL: "/api/clips/100/blob"})
		case strings.HasSuffix(r.URL.Path, "/blob") && r.Method == http.MethodGet:
			downloads++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(testPNG)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	clip := &imageClipboard{}
	e := New(Options{ServerURL: srv.URL, Token: "t", Device: "test", PollInterval: time.Millisecond, Logger: quietLogger()}, clip)
	if e.imgClip == nil {
		t.Fatalf("imageClipboard 应启用图片通道")
	}

	// 1) 用户复制了一张图片 → 轮询上传
	clip.img = testPNG
	e.pollOnce(context.Background())
	mu.Lock()
	if uploads != 1 {
		mu.Unlock()
		t.Fatalf("图片应上传一次，得到 %d", uploads)
	}
	mu.Unlock()

	// 2) 内容未变 → 不再上传
	e.pollOnce(context.Background())
	mu.Lock()
	if uploads != 1 {
		mu.Unlock()
		t.Fatalf("图片未变化不应重复上传，得到 %d", uploads)
	}
	mu.Unlock()

	// 3) 远端图片事件（同内容，different device）→ 本地已是该图，忽略
	e.handleRemote(context.Background(), &Clip{ID: 100, Type: "image", Hash: HashBytes(testPNG), BlobURL: "/api/clips/100/blob", SourceDevice: "peer"})
	mu.Lock()
	if downloads != 0 {
		mu.Unlock()
		t.Fatalf("本地已有同图不应下载")
	}
	mu.Unlock()

	// 4) 远端新图片事件 → 下载并写入剪贴板
	clip.img = nil
	e.handleRemote(context.Background(), &Clip{ID: 101, Type: "image", Hash: HashBytes(testPNG), BlobURL: "/api/clips/101/blob", SourceDevice: "peer"})
	mu.Lock()
	w := clip.writes
	mu.Unlock()
	if w != 1 {
		t.Fatalf("远端图片应写入一次剪贴板，得到 %d", w)
	}
	if e.guard.LastWrittenHash() != HashBytes(testPNG) {
		t.Fatalf("应记录 lastWrittenHash")
	}

	// 5) 写入后轮询：命中 self-write 分支，不再上传
	e.pollOnce(context.Background())
	mu.Lock()
	if uploads != 1 {
		mu.Unlock()
		t.Fatalf("self-write 不应上传，得到 %d 次上传", uploads)
	}
	mu.Unlock()
}

// 暂停状态下：轮询不上传、远端推送不写入（托盘「暂停同步」语义）。
func TestPausedStopsBothDirections(t *testing.T) {
	var mu sync.Mutex
	uploads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uploads++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(&Clip{ID: 1, Hash: HashText("x")})
	}))
	defer srv.Close()

	clip := &imageClipboard{}
	e := New(Options{ServerURL: srv.URL, Token: "t", Device: "test", PollInterval: time.Millisecond, Logger: quietLogger()}, clip)

	e.SetPaused(true)
	if !e.Paused() {
		t.Fatalf("应处于暂停状态")
	}
	clip.SetText("should not upload")
	e.pollOnce(context.Background())
	mu.Lock()
	n := uploads
	mu.Unlock()
	if n != 0 {
		t.Fatalf("暂停时不应上传")
	}
	e.handleRemote(context.Background(), &Clip{ID: 2, Type: "text", Text: "remote", Hash: HashText("remote"), SourceDevice: "peer"})
	if clip.Text() != "should not upload" {
		t.Fatalf("暂停时不应写入远端内容")
	}

	e.SetPaused(false)
	e.pollOnce(context.Background())
	mu.Lock()
	n = uploads
	mu.Unlock()
	if n != 1 {
		t.Fatalf("恢复后应正常上传，得到 %d", n)
	}
}
