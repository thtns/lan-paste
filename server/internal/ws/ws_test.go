package ws_test

// 集成测试：起一个与 main 等价的完整 http server（REST + ws + hub 联动），
// 用两个不同 device 的 ws 客户端验证广播排除逻辑。
//
// 注意：coder/websocket 中传入 Read 的 context 到期会直接关闭整条连接，
// 因此「断言没有收到消息」不能用带超时的 Read，而要用独立 goroutine 持续读，
// 配合 channel + time.After 做非阻塞断言。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"lanpaste/server/internal/api"
	"lanpaste/server/internal/auth"
	"lanpaste/server/internal/store"
	"lanpaste/server/internal/store/memstore"
	"lanpaste/server/internal/ws"
)

const testToken = "ws-test-token"

// wsMsg 宽松解析所有事件形态。
type wsMsg struct {
	Event       string      `json:"event"`
	Server      string      `json:"server"`
	DeviceCount int         `json:"device_count"`
	Clip        *store.Clip `json:"clip"`
	ID          int64       `json:"id"`
}

// client 包装一条测试连接：后台 goroutine 持续读，next() 在超时内取一条消息。
type client struct {
	conn  *websocket.Conn
	msgs  chan wsMsg
	hello wsMsg // 连接时收到的 hello 事件
}

// next 在 timeout 内取下一条消息；超时返回零值（用于断言「没有收到」）。
func (cl *client) next(t *testing.T, timeout time.Duration) wsMsg {
	t.Helper()
	select {
	case m, ok := <-cl.msgs:
		if !ok {
			t.Fatalf("连接已被对端关闭（读到错误），msgs 通道关闭")
		}
		return m
	case <-time.After(timeout):
		return wsMsg{}
	}
}

type env struct {
	srv   *httptest.Server
	token string // 测试用户的设备令牌
}

func newEnv(t *testing.T) *env {
	t.Helper()
	as, err := auth.Load(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := as.Register("tester", "tester66"); err != nil {
		t.Fatal(err)
	}
	tok, _, err := as.Login("tester", "tester66", "test")
	if err != nil {
		t.Fatal(err)
	}
	hub := ws.NewHub(testToken, as)
	apiSrv := api.New(memstore.New(), testToken, hub, nil, as)
	root := http.NewServeMux()
	root.Handle("/api/", apiSrv.Handler())
	root.Handle("/ws", hub)
	return &env{srv: httptest.NewServer(root), token: tok}
}

func (e *env) wsURL(params string) string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws" + params
}

// dial 连接、读取首条 hello、启动后台读取 goroutine。
func dial(t *testing.T, e *env, device string) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, e.wsURL("?token="+e.token+"&device="+device), nil)
	if err != nil {
		t.Fatalf("device %s 连接 /ws 失败: %v", device, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	cl := &client{conn: conn, msgs: make(chan wsMsg, 128)}
	go func() {
		for {
			// 必须用无超时的 context：Read 的 ctx 到期会关闭整条连接
			_, data, err := conn.Read(context.Background())
			if err != nil {
				close(cl.msgs)
				return
			}
			var m wsMsg
			if err := json.Unmarshal(data, &m); err != nil {
				t.Errorf("消息解析失败: %v 原文=%s", err, data)
				close(cl.msgs)
				return
			}
			cl.msgs <- m
		}
	}()

	hello := cl.next(t, 3*time.Second)
	if hello.Event != "hello" {
		t.Fatalf("首条消息应为 hello，得到 %+v", hello)
	}
	cl.hello = hello
	return cl
}

// postClip 通过 REST 上传一条剪贴板。
func postClip(t *testing.T, e *env, text, device string) store.Clip {
	t.Helper()
	c, ok := postClipQuiet(t, e, text, device)
	if !ok {
		t.FailNow()
	}
	return c
}

// postClipQuiet 与 postClip 相同，但用 Errorf 报错并返回 ok=false，
// 可在非测试 goroutine 中安全调用（t.Fatalf 禁止跨 goroutine 调用）。
func postClipQuiet(t *testing.T, e *env, text, device string) (store.Clip, bool) {
	body := strings.NewReader(`{"type":"text","text":"` + text + `","device":"` + device + `"}`)
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/clips", body)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("POST /api/clips 失败: %v", err)
		return store.Clip{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("POST 状态码 = %d, 期望 201", resp.StatusCode)
		return store.Clip{}, false
	}
	var c store.Clip
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Errorf("解析 POST 响应失败: %v", err)
		return store.Clip{}, false
	}
	return c, true
}

func TestHelloDeviceCount(t *testing.T) {
	e := newEnv(t)
	defer e.srv.Close()

	cA := dial(t, e, "devA")
	if cA.hello.Server != "lanpaste/v0.1" {
		t.Fatalf("hello.server = %q, 期望 lanpaste/v0.1", cA.hello.Server)
	}
	if cA.hello.DeviceCount != 1 {
		t.Fatalf("第一个连接的 device_count = %d, 期望 1", cA.hello.DeviceCount)
	}

	cB := dial(t, e, "devB")
	if cB.hello.DeviceCount != 2 {
		t.Fatalf("第二个连接的 device_count = %d, 期望 2", cB.hello.DeviceCount)
	}
}

func TestBroadcastNewExcludesSourceDevice(t *testing.T) {
	e := newEnv(t)
	defer e.srv.Close()

	cA := dial(t, e, "devA")
	cB := dial(t, e, "devB")
	cC := dial(t, e, "devC")

	clip := postClip(t, e, "hello-broadcast", "devA")

	// devB：应收到 new 事件且内容一致
	m := cB.next(t, 3*time.Second)
	if m.Event != "new" || m.Clip == nil {
		t.Fatalf("devB 应收到 new 事件，得到 %+v", m)
	}
	if m.Clip.ID != clip.ID || m.Clip.Text != "hello-broadcast" || m.Clip.SourceDevice != "devA" {
		t.Fatalf("广播内容与上传不符: %+v 期望 id=%d", m.Clip, clip.ID)
	}

	// devC：同样收到（来源跳过只匹配 devA）
	if m := cC.next(t, 3*time.Second); m.Event != "new" {
		t.Fatalf("devC 应收到 new 事件，得到 %+v", m)
	}

	// devA（来源设备）：300ms 内不应收到任何消息
	if m := cA.next(t, 300*time.Millisecond); m.Event != "" {
		t.Fatalf("来源设备 devA 不应收到广播，却得到 %+v", m)
	}

	// REST 上传来源为「没有对应 ws 连接的设备」时，所有连接都应收到
	clip2 := postClip(t, e, "from-unknown-device", "phone-x")
	mB := cB.next(t, 3*time.Second)
	mA := cA.next(t, 3*time.Second)
	if mB.Event != "new" || mB.Clip.ID != clip2.ID {
		t.Fatalf("devB 应收到 phone-x 上传的广播，得到 %+v", mB)
	}
	if mA.Event != "new" || mA.Clip.ID != clip2.ID {
		t.Fatalf("devA 也应收到 phone-x 上传的广播，得到 %+v", mA)
	}
}

func TestHashDedupNoBroadcast(t *testing.T) {
	e := newEnv(t)
	defer e.srv.Close()

	cA := dial(t, e, "devA")
	cB := dial(t, e, "devB")

	postClip(t, e, "dup-text", "devA")
	if m := cB.next(t, 3*time.Second); m.Event != "new" {
		t.Fatalf("devB 应收到首条广播，得到 %+v", m)
	}

	// 相同内容再次上传（即使来自别的设备）：返回 200 且不产生广播
	body := strings.NewReader(`{"type":"text","text":"dup-text","device":"devB"}`)
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/clips", body)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("重复 POST 失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重复内容应返回 200，得到 %d", resp.StatusCode)
	}

	// 两端都不应再收到消息
	if m := cB.next(t, 300*time.Millisecond); m.Event != "" {
		t.Fatalf("去重后 devB 不应收到广播，得到 %+v", m)
	}
	if m := cA.next(t, 300*time.Millisecond); m.Event != "" {
		t.Fatalf("去重后 devA 不应收到广播，得到 %+v", m)
	}
}

func TestBroadcastDeleted(t *testing.T) {
	e := newEnv(t)
	defer e.srv.Close()

	cA := dial(t, e, "devA")
	cB := dial(t, e, "devB")

	clip := postClip(t, e, "to-delete", "devZ") // 来源无连接：两端都收到 new
	_ = cA.next(t, 3*time.Second)
	_ = cB.next(t, 3*time.Second)

	req, _ := http.NewRequest("DELETE", e.srv.URL+"/api/clips/"+strconv.FormatInt(clip.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE 失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE 状态码 = %d, 期望 204", resp.StatusCode)
	}

	// 契约：删除事件广播给所有客户端（无排除）
	for name, cl := range map[string]*client{"devA": cA, "devB": cB} {
		m := cl.next(t, 3*time.Second)
		if m.Event != "deleted" || m.ID != clip.ID {
			t.Fatalf("%s 应收到 deleted(id=%d) 事件，得到 %+v", name, clip.ID, m)
		}
	}
}

func TestWSUnauthorized(t *testing.T) {
	e := newEnv(t)
	defer e.srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, e.wsURL("?token=wrong"), nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatalf("错误 token 不应握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("鉴权失败应返回 401，得到 resp=%v err=%v", resp, err)
	}

	// 缺 token 同样 401
	_, resp2, err2 := websocket.Dial(ctx, e.wsURL(""), nil)
	if err2 == nil {
		t.Fatalf("缺 token 不应握手成功")
	}
	if resp2 == nil || resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("缺 token 应返回 401，得到 resp=%v err=%v", resp2, err2)
	}
}

func TestBroadcastConcurrentSafety(t *testing.T) {
	// 并发广播 + 客户端断开，配合 -race 检查 hub 锁正确性
	e := newEnv(t)
	defer e.srv.Close()

	const n = 20
	conns := make([]*client, 0, n)
	for i := 0; i < n; i++ {
		conns = append(conns, dial(t, e, "dev"+strconv.Itoa(i)))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			if _, ok := postClipQuiet(t, e, "c"+strconv.Itoa(i), "nowhere-device"); !ok {
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	_ = conns[0].conn.CloseNow() // 广播进行中强行断开一个客户端
	<-done

	// 幸存客户端应能持续收到 new 事件（不断言精确条数，只验证不 panic / 不死锁 / 不断流）
	got := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && got < 50 {
		if m := conns[1].next(t, 2*time.Second); m.Event == "new" {
			got++
		} else {
			break
		}
	}
	if got == 0 {
		t.Fatalf("幸存连接应收到广播事件")
	}
}
