package syncer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// fakeServer 是一个符合 PROTOCOL.md 契约的最小服务端：
// POST /api/clips（Bearer 认证、hash 校验、与最新一条相同则 200 去重不广播，
// 否则 201 并向「其他设备」的 WebSocket 连接广播 new 事件）、
// GET /api/clips/latest、GET /ws?token=&device=（先推欢迎消息 hello）。
type fakeServer struct {
	*httptest.Server
	token string

	mu      sync.Mutex
	nextID  int64
	latest  *Clip
	posts   int                        // 收到的 POST /api/clips 总数
	newRecs int                        // 实际新建记录数（未被去重）
	conns   map[*websocket.Conn]string // conn → device
}

func newFakeServer(t *testing.T, token string) *fakeServer {
	t.Helper()
	s := &fakeServer{token: token, conns: map[*websocket.Conn]string{}}
	s.Server = httptest.NewServer(s)
	t.Cleanup(s.Server.Close)
	return s
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/clips" && r.Method == http.MethodPost:
		if !s.checkAuth(w, r) {
			return
		}
		var req uploadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad json")
			return
		}
		if req.Hash != HashText(req.Text) { // 服务端校验 hash 与 text 一致
			writeErr(w, http.StatusBadRequest, "hash mismatch")
			return
		}

		s.mu.Lock()
		s.posts++
		if s.latest != nil && s.latest.Hash == req.Hash { // 去重：200 且不广播
			existing := *s.latest
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, &existing)
			return
		}
		s.nextID++
		s.newRecs++
		clip := &Clip{
			ID: s.nextID, Type: "text", Text: req.Text, Hash: req.Hash,
			SourceDevice: req.Device, Size: int64(len(req.Text)),
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
		s.latest = clip
		conns := make(map[*websocket.Conn]string, len(s.conns))
		for c, d := range s.conns {
			conns[c] = d
		}
		s.mu.Unlock()

		// 向所有「其他」已连接客户端广播
		for conn, dev := range conns {
			if dev == req.Device {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = wsjson.Write(ctx, conn, wsMessage{Event: "new", Clip: clip})
			cancel()
		}
		writeJSON(w, http.StatusCreated, clip)

	case r.URL.Path == "/api/clips/latest" && r.Method == http.MethodGet:
		if !s.checkAuth(w, r) {
			return
		}
		s.mu.Lock()
		latest := s.latest
		s.mu.Unlock()
		if latest == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, latest)

	case r.URL.Path == "/ws":
		if r.URL.Query().Get("token") != s.token {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		device := r.URL.Query().Get("device")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = wsjson.Write(ctx, conn, wsMessage{Event: "hello", Server: "lanpaste/fake", DeviceCount: 1})
		cancel()

		s.mu.Lock()
		s.conns[conn] = device
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
			conn.CloseNow()
		}()
		for { // MVP 客户端不发消息；持续读以维持连接并检测断开
			if _, _, err := conn.Read(context.Background()); err != nil {
				return
			}
		}

	default:
		writeErr(w, http.StatusNotFound, "not found")
	}
}

func (s *fakeServer) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *fakeServer) connectedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// stats 返回（POST 总数, 新建记录数）。
func (s *fakeServer) stats() (posts, newRecs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts, s.newRecs
}

// TestEndToEndSync 走完契约主链路：
// 客户端 A 剪贴板变化 → POST /api/clips → 服务端广播 → 客户端 B 收到并写入；
// 验证 B 不回传刚写入的内容（防回环）、反向同步、以及并发上传同内容时服务端去重不广播。
func TestEndToEndSync(t *testing.T) {
	fs := newFakeServer(t, "testtoken")

	clipA := &fakeClipboard{}
	clipB := &fakeClipboard{}
	eA := New(Options{ServerURL: fs.URL, Token: "testtoken", Device: "device-a", PollInterval: 10 * time.Millisecond, Logger: quietLogger()}, clipA)
	eB := New(Options{ServerURL: fs.URL, Token: "testtoken", Device: "device-b", PollInterval: 10 * time.Millisecond, Logger: quietLogger()}, clipB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eA.Run(ctx)
	go eB.Run(ctx)

	waitFor(t, "两个客户端均连上 /ws", 5*time.Second, func() bool { return fs.connectedCount() >= 2 })

	// A 方向：用户在 A 上复制
	clipA.SetText("hello from A")
	waitFor(t, "B 收到推送并写入剪贴板", 5*time.Second, func() bool { return clipB.Text() == "hello from A" })

	// 防回环：B 刚写入的内容不得触发 B 的再次上传
	time.Sleep(300 * time.Millisecond)
	if posts, newRecs := fs.stats(); posts != 1 || newRecs != 1 {
		t.Fatalf("B 写入后不应产生额外上传，期望 (posts=1,new=1)，得到 (%d,%d)", posts, newRecs)
	}
	// A 也不应把自己上传的内容再写一遍（服务端只广播给其他设备）
	if clipA.WriteCount() != 0 {
		t.Fatalf("服务端不应把内容广播回上传方 A")
	}

	// B 方向：用户在 B 上复制
	clipB.SetText("reply from B")
	waitFor(t, "A 收到反向推送并写入剪贴板", 5*time.Second, func() bool { return clipA.Text() == "reply from B" })
	time.Sleep(300 * time.Millisecond)
	if posts, newRecs := fs.stats(); posts != 2 || newRecs != 2 {
		t.Fatalf("期望 (posts=2,new=2)，得到 (%d,%d)", posts, newRecs)
	}

	// 并发上传相同内容：两端几乎同时复制同一文本，后到的一方被服务端去重（200，不广播），
	// 任一端都不应因此产生新的本地写入
	writesA, writesB := clipA.WriteCount(), clipB.WriteCount()
	clipA.SetText("race content")
	clipB.SetText("race content")
	waitFor(t, "双方都完成上传（其中一方被去重）", 5*time.Second, func() bool {
		posts, newRecs := fs.stats()
		return posts == 4 && newRecs == 3
	})
	// 广播到达后：接收方本地已是相同内容 → duplicate 忽略，不写入
	time.Sleep(300 * time.Millisecond)
	if clipA.WriteCount() != writesA || clipB.WriteCount() != writesB {
		t.Fatalf("去重/重复推送不应产生新的本地写入：A %d→%d，B %d→%d",
			writesA, clipA.WriteCount(), writesB, clipB.WriteCount())
	}
	if clipA.Text() != "race content" || clipB.Text() != "race content" {
		t.Fatalf("两端剪贴板内容应保持不变，得到 A=%q B=%q", clipA.Text(), clipB.Text())
	}
}

// TestWSTokenRejected 验证 token 错误时服务端按契约返回 401，客户端进入重连循环。
func TestWSTokenRejected(t *testing.T) {
	fs := newFakeServer(t, "testtoken")
	clip := &fakeClipboard{}
	e := New(Options{ServerURL: fs.URL, Token: "wrong-token", Device: "device-x", PollInterval: 10 * time.Millisecond, Logger: quietLogger()}, clip)

	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	e.Run(ctx) // 会在 ctx 超时后返回
	if fs.connectedCount() != 0 {
		t.Fatalf("错误 token 不应建立起 WebSocket 连接")
	}
}
