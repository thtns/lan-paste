package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lanpaste/server/internal/auth"
	"lanpaste/server/internal/blob"
	"lanpaste/server/internal/store"
	"lanpaste/server/internal/store/memstore"
)

const testToken = "test-token-123" // 管理令牌:仅 /api/setup 可用,不能访问剪贴板

// testerCredentials 惰性创建账号体系并登录测试用户,返回 (authStore, 设备令牌)。
// 多个测试共享同一账号体系（注册/登录本身不是被测逻辑的测试点时复用）。
var (
	testerOnce   sync.Once
	testerAuth   *auth.Store
	serverTokens sync.Map // *Server -> 设备令牌（按实例惰性登录）
)

// testerAuthFor 惰性创建共享账号体系并注册测试用户。
func testerAuthFor() *auth.Store {
	testerOnce.Do(func() {
		dir, _ := os.MkdirTemp("", "lanpaste-auth-test")
		testerAuth, _ = auth.Load(filepath.Join(dir, "users.json"))
		_, _ = testerAuth.Register("tester", "tester66")
	})
	return testerAuth
}

// deviceTokenFor 返回该实例上测试用户的设备令牌（惰性登录,按实例缓存）。
func deviceTokenFor(t *testing.T, s *Server) string {
	t.Helper()
	if s.auth == nil {
		return ""
	}
	if v, ok := serverTokens.Load(s); ok {
		return v.(string)
	}
	if _, err := s.auth.Register("tester", "tester66"); err != nil && err != auth.ErrUserExists {
		t.Fatalf("注册测试用户失败: %v", err)
	}
	tok, _, err := s.auth.Login("tester", "tester66", "test-device")
	if err != nil {
		t.Fatalf("登录测试用户失败: %v", err)
	}
	serverTokens.Store(s, tok)
	return tok
}

// fakeBC 记录广播调用，用于断言广播行为。
type fakeBC struct {
	mu       sync.Mutex
	newCalls []store.Clip
	newExcl  []string
	delCalls []int64
}

func (f *fakeBC) BroadcastNew(c store.Clip, excludeDevice string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCalls = append(f.newCalls, c)
	f.newExcl = append(f.newExcl, excludeDevice)
}

func (f *fakeBC) BroadcastDeleted(id int64, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delCalls = append(f.delCalls, id)
	_ = userID
}

func newTestServer() (*Server, *fakeBC) {
	bc := &fakeBC{}
	return New(memstore.New(), testToken, bc, nil, testerAuthFor()), bc
}

// newAuthedServer 带账号体系与临时图片目录的测试实例；预注册 alice/bob 两个用户
// 并各自登录一台设备，返回其设备令牌。
func newAuthedServer(t *testing.T) (*Server, *fakeBC, aliceBobTokens) {
	t.Helper()
	s, bc := newTestServerWithBlobs(t)
	as, err := auth.Load(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.auth = as
	if _, err := as.Register("alice", "alice66"); err != nil {
		t.Fatal(err)
	}
	if _, err := as.Register("bob", "bob66666"); err != nil {
		t.Fatal(err)
	}
	at, _, err := as.Login("alice", "alice66", "alice-dev")
	if err != nil {
		t.Fatal(err)
	}
	bt, _, err := as.Login("bob", "bob66666", "bob-dev")
	if err != nil {
		t.Fatal(err)
	}
	return s, bc, aliceBobTokens{alice: at, bob: bt}
}

type aliceBobTokens struct{ alice, bob string }

// authedReq 携带指定 Bearer 令牌发请求。
func authedReq(t *testing.T, s *Server, method, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// 多用户隔离：alice/bob 各传各的，互相看不见；管理令牌不能访问剪贴板。
func TestMultiUserIsolation(t *testing.T) {
	s, bc, tok := newAuthedServer(t)

	if rec := authedReq(t, s, "POST", "/api/clips",
		`{"type":"text","text":"alice secret","device":"a"}`, tok.alice); rec.Code != http.StatusCreated {
		t.Fatalf("alice 上传应 201，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := authedReq(t, s, "POST", "/api/clips",
		`{"type":"text","text":"bob secret","device":"b"}`, tok.bob); rec.Code != http.StatusCreated {
		t.Fatalf("bob 上传应 201，得到 %d body=%s", rec.Code, rec.Body.String())
	}

	// alice 只看到自己的
	rec := authedReq(t, s, "GET", "/api/clips", "", tok.alice)
	if !strings.Contains(rec.Body.String(), "alice secret") || strings.Contains(rec.Body.String(), "bob secret") {
		t.Fatalf("alice 不应看到 bob 的内容: %s", rec.Body.String())
	}
	// bob 只看到自己的
	rec = authedReq(t, s, "GET", "/api/clips", "", tok.bob)
	if !strings.Contains(rec.Body.String(), "bob secret") || strings.Contains(rec.Body.String(), "alice secret") {
		t.Fatalf("bob 不应看到 alice 的内容: %s", rec.Body.String())
	}
	// 管理令牌已不能访问剪贴板（v1.2 移除兼容）
	rec = authedReq(t, s, "GET", "/api/clips", "", testToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("管理令牌访问剪贴板应 401，得到 %d", rec.Code)
	}

	// alice 无法读取/删除 bob 的记录 #2（按 404 处理，不泄露存在性）
	if rec := authedReq(t, s, "GET", "/api/clips/2", "", tok.alice); rec.Code != http.StatusNotFound {
		t.Fatalf("alice 读 bob 记录应 404，得到 %d", rec.Code)
	}
	if rec := authedReq(t, s, "DELETE", "/api/clips/2", "", tok.alice); rec.Code != http.StatusNotFound {
		t.Fatalf("alice 删 bob 记录应 404，得到 %d", rec.Code)
	}
	// bob 自己能删
	if rec := authedReq(t, s, "DELETE", "/api/clips/2", "", tok.bob); rec.Code != http.StatusNoContent {
		t.Fatalf("bob 删自己记录应 204，得到 %d", rec.Code)
	}

	// 去重按用户隔离：bob 传与 alice 相同文本 → 不命中 alice 的去重，正常新建
	rec = authedReq(t, s, "POST", "/api/clips",
		`{"type":"text","text":"alice secret","device":"b"}`, tok.bob)
	if rec.Code != http.StatusCreated {
		t.Fatalf("跨用户相同内容不应去重，应 201，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	_ = bc
}

// 注册/登录端点：注册 → 登录 → 用设备令牌访问。
func TestAuthEndpoints(t *testing.T) {
	s, _, _ := newAuthedServer(t)

	// 注册（开放注册，局域网自用）
	rec := authedReq(t, s, "POST", "/api/auth/register",
		`{"username":"carol","password":"carol66"}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("注册应 201，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	// 重复注册被拒
	rec = authedReq(t, s, "POST", "/api/auth/register",
		`{"username":"carol","password":"carol66"}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("重复注册应 400，得到 %d", rec.Code)
	}
	// 登录
	rec = authedReq(t, s, "POST", "/api/auth/login",
		`{"username":"carol","password":"carol66","device":"web-1"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("登录应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
		User  string `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Token == "" || out.User != "carol" {
		t.Fatalf("登录应返回令牌与用户名: %+v", out)
	}
	// 设备令牌可访问业务接口
	rec = authedReq(t, s, "GET", "/api/clips", "", out.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("设备令牌应可访问列表，得到 %d", rec.Code)
	}
	// 登出后令牌失效
	authedReq(t, s, "POST", "/api/auth/logout", "", out.Token)
	rec = authedReq(t, s, "GET", "/api/clips", "", out.Token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("登出后应 401，得到 %d", rec.Code)
	}
}

func newTestServerWithBlobs(t *testing.T) (*Server, *fakeBC) {
	t.Helper()
	bc := &fakeBC{}
	bm, err := blob.New(t.TempDir())
	if err != nil {
		t.Fatalf("创建 blob 管理器失败: %v", err)
	}
	return New(memstore.New(), testToken, bc, bm, testerAuthFor()), bc
}

// do 带认证发请求（测试用户的设备令牌）。
func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+deviceTokenFor(t, s))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func postClip(t *testing.T, s *Server, text, device string) (int, store.Clip) {
	t.Helper()
	body := fmt.Sprintf(`{"type":"text","text":%q,"hash":"whatever","device":%q}`, text, device)
	rec := do(t, s, "POST", "/api/clips", body)
	var c store.Clip
	if rec.Code != http.StatusNoContent {
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
			t.Fatalf("POST 响应解析失败 (%d): %v body=%s", rec.Code, err, rec.Body.String())
		}
	}
	return rec.Code, c
}

func TestAuth(t *testing.T) {
	s, _ := newTestServer()

	devTok := deviceTokenFor(t, s)
	cases := []struct {
		name, header string
		want         int
	}{
		{"缺少 Authorization", "", http.StatusUnauthorized},
		{"格式错误", "Basic abc", http.StatusUnauthorized},
		{"错误 token", "Bearer wrong", http.StatusUnauthorized},
		{"管理令牌不能访问剪贴板", "Bearer " + testToken, http.StatusUnauthorized},
		{"设备令牌", "Bearer " + devTok, http.StatusNoContent}, // latest 空库返回 204
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", "/api/clips/latest", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: 状态码 = %d, 期望 %d, body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
		}
		if tc.want == http.StatusUnauthorized {
			if !strings.Contains(rec.Body.String(), `"unauthorized"`) {
				t.Errorf("%s: 错误体应为 unauthorized，得到 %s", tc.name, rec.Body.String())
			}
		}
	}
}

func TestCreate(t *testing.T) {
	s, bc := newTestServer()

	// 中文文本：size 为 UTF-8 字节数（"你好" = 6 字节）；hash 由服务端重算
	code, c := postClip(t, s, "你好", "macbook-pro")
	if code != http.StatusCreated {
		t.Fatalf("首次 POST 状态码 = %d, 期望 201, body=%s", code, "")
	}
	if c.ID == 0 || c.Type != "text" || c.Text != "你好" || c.SourceDevice != "macbook-pro" {
		t.Fatalf("回显字段不符: %+v", c)
	}
	if c.Size != 6 {
		t.Fatalf("size = %d, 期望 6（UTF-8 字节数）", c.Size)
	}
	if want := sha256Hex("你好"); c.Hash != want {
		t.Fatalf("hash = %s, 期望服务端计算的 %s", c.Hash, want)
	}
	if c.CreatedAt.IsZero() {
		t.Fatalf("created_at 未回填")
	}
	if len(bc.newCalls) != 1 || bc.newExcl[0] != "macbook-pro" {
		t.Fatalf("新记录应广播一次并排除来源设备，得到 %v excl=%v", bc.newCalls, bc.newExcl)
	}
	if got := bc.newCalls[0].ID; got != c.ID {
		t.Fatalf("广播的 clip id = %d, 期望 %d", got, c.ID)
	}
}

func TestCreateHashDedup(t *testing.T) {
	s, bc := newTestServer()

	code1, c1 := postClip(t, s, "hello", "devA")
	if code1 != http.StatusCreated {
		t.Fatalf("首次创建应为 201，得到 %d", code1)
	}

	// 相同内容（即使来自别的设备、上报了错误 hash）→ 返回已有记录、200、不广播、不新增
	req := `{"type":"text","text":"hello","hash":"bad-hash","device":"devB"}`
	rec := do(t, s, "POST", "/api/clips", req)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复内容应为 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	var c2 store.Clip
	if err := json.Unmarshal(rec.Body.Bytes(), &c2); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c2.ID != c1.ID {
		t.Fatalf("去重应返回已有记录 id=%d，得到 %d", c1.ID, c2.ID)
	}
	if n := len(bc.newCalls); n != 1 {
		t.Fatalf("重复内容不应广播，广播次数 = %d", n)
	}

	// 内容变化后恢复正常新建
	code3, _ := postClip(t, s, "world", "devA")
	if code3 != http.StatusCreated {
		t.Fatalf("新内容应为 201，得到 %d", code3)
	}
	if n := len(bc.newCalls); n != 2 {
		t.Fatalf("应有第二次广播，得到 %d", n)
	}

	// 与「最新一条」不同但与更早一条相同 → 正常新建（契约只对最新一条去重）
	code4, _ := postClip(t, s, "hello", "devA")
	if code4 != http.StatusCreated {
		t.Fatalf("非最新重复应为 201，得到 %d", code4)
	}
}

func TestCreateValidation(t *testing.T) {
	s, _ := newTestServer()

	cases := []struct {
		name, body string
		want       int
	}{
		{"非法 JSON", `{not json`, http.StatusBadRequest},
		{"保留类型 image", `{"type":"image","text":"x","device":"d"}`, http.StatusBadRequest},
		{"空 type", `{"text":"x","device":"d"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		rec := do(t, s, "POST", "/api/clips", tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: 状态码 = %d, 期望 %d, body=%s", tc.name, rec.Code, tc.want, rec.Body.String())
		}
		var e map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e["error"] == "" {
			t.Errorf("%s: 错误体应含 error 字段，得到 %s", tc.name, rec.Body.String())
		}
	}

	// device 缺省时落为 unknown
	rec := do(t, s, "POST", "/api/clips", `{"type":"text","text":"x"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("无 device 应仍可创建，得到 %d", rec.Code)
	}
	var c store.Clip
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	if c.SourceDevice != "unknown" {
		t.Fatalf("缺省 device 应为 unknown，得到 %q", c.SourceDevice)
	}
}

func TestList(t *testing.T) {
	s, _ := newTestServer()
	for i := 0; i < 3; i++ {
		postClip(t, s, fmt.Sprintf("t%d", i), "dev")
	}

	// 默认 limit，降序
	rec := do(t, s, "GET", "/api/clips", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/clips = %d", rec.Code)
	}
	var resp struct {
		Clips []store.Clip `json:"clips"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Clips) != 3 || resp.Clips[0].Text != "t2" {
		t.Fatalf("应按时间降序返回 3 条，得到 %+v", resp.Clips)
	}

	// limit 生效
	rec = do(t, s, "GET", "/api/clips?limit=1", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Clips) != 1 || resp.Clips[0].Text != "t2" {
		t.Fatalf("limit=1 应只返回最新 1 条，得到 %+v", resp.Clips)
	}

	// 非法 limit 回退默认值
	rec = do(t, s, "GET", "/api/clips?limit=abc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("非法 limit 应回退默认值而非报错，得到 %d", rec.Code)
	}

	// 空库时 clips 应为 [] 而非 null
	s2, _ := newTestServer()
	rec = do(t, s2, "GET", "/api/clips", "")
	if strings.Contains(rec.Body.String(), "null") {
		t.Fatalf("空列表应为 []，得到 %s", rec.Body.String())
	}
}

func TestLatest(t *testing.T) {
	s, _ := newTestServer()

	// 空库 → 204 无 body
	rec := do(t, s, "GET", "/api/clips/latest", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("空库 latest 应 204，得到 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 应无 body，得到 %q", rec.Body.String())
	}

	postClip(t, s, "only", "dev")
	rec = do(t, s, "GET", "/api/clips/latest", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("有记录 latest 应 200，得到 %d", rec.Code)
	}
	var c store.Clip
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c.Text != "only" {
		t.Fatalf("latest 应返回 only，得到 %q", c.Text)
	}
}

func TestGetAndDelete(t *testing.T) {
	s, bc := newTestServer()
	_, c := postClip(t, s, "to-be-fetched", "dev")

	// 单条查询；latest 是字面路径，优先于 {id}，不会被数字 id 遮蔽
	rec := do(t, s, "GET", fmt.Sprintf("/api/clips/%d", c.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET by id = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got store.Clip
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.ID != c.ID {
		t.Fatalf("GET by id 应返回 %d，得到 %d", c.ID, got.ID)
	}

	// 不存在 / 非法 id → 404
	if rec := do(t, s, "GET", "/api/clips/99999", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404，得到 %d", rec.Code)
	}
	if rec := do(t, s, "GET", "/api/clips/abc", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("非法 id 应 404，得到 %d", rec.Code)
	}

	// 删除 → 204 + 广播 deleted
	rec = do(t, s, "DELETE", fmt.Sprintf("/api/clips/%d", c.ID), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE 应 204，得到 %d", rec.Code)
	}
	if len(bc.delCalls) != 1 || bc.delCalls[0] != c.ID {
		t.Fatalf("删除应广播一次 deleted(id=%d)，得到 %v", c.ID, bc.delCalls)
	}
	// 再删 → 404，不再广播
	if rec := do(t, s, "DELETE", fmt.Sprintf("/api/clips/%d", c.ID), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，得到 %d", rec.Code)
	}
	if len(bc.delCalls) != 1 {
		t.Fatalf("404 删除不应广播，得到 %v", bc.delCalls)
	}

	// 方法不允许 → 405（mux 自动处理）
	if rec := do(t, s, "PUT", "/api/clips", `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /api/clips 应 405，得到 %d", rec.Code)
	}
}

// sha256Hex 与服务端相同的哈希算法，供测试断言。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// tinyPNG 一个 1x1 红色 PNG 的完整字节（合法图片，供图片端点测试）。
var tinyPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xde, 0x00, 0x00, 0x00,
	0x0c, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
	0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d, 0xb0, 0x00, 0x00, 0x00,
	0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func postImage(t *testing.T, s *Server, device string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"data":%q,"device":%q,"width":1,"height":1}`,
		base64.StdEncoding.EncodeToString(data), device)
	rec := do(t, s, "POST", "/api/clips/image", body)
	return rec
}

func TestCreateImage(t *testing.T) {
	s, bc := newTestServerWithBlobs(t)

	rec := postImage(t, s, "iphone", tinyPNG)
	if rec.Code != http.StatusCreated {
		t.Fatalf("图片上传应 201，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	var c store.Clip
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c.Type != "image" || c.Text != "" {
		t.Fatalf("图片条目 type=image text 空，得到 %+v", c)
	}
	if c.Mime != "image/png" || c.Width != 1 || c.Height != 1 {
		t.Fatalf("应嗅探出 png 与服务端解码尺寸 1x1，得到 %s %dx%d", c.Mime, c.Width, c.Height)
	}
	if c.Size != int64(len(tinyPNG)) {
		t.Fatalf("size 应为文件字节数 %d，得到 %d", len(tinyPNG), c.Size)
	}
	if want := sha256Hex(string(tinyPNG)); c.Hash != want {
		t.Fatalf("hash 应为文件字节 sha256")
	}
	if c.BlobURL == "" || !strings.HasPrefix(c.BlobURL, "/api/clips/") || !strings.HasSuffix(c.BlobURL, "/blob") {
		t.Fatalf("blob_url 未生成: %q", c.BlobURL)
	}
	if len(bc.newCalls) != 1 || bc.newExcl[0] != "iphone" {
		t.Fatalf("图片应广播一次并排除来源设备")
	}

	// blob 下载：内容一致、MIME 正确
	rec = do(t, s, "GET", c.BlobURL, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("blob 下载应 200，得到 %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("Content-Type 应为 image/png，得到 %s", rec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(rec.Body.Bytes(), tinyPNG) {
		t.Fatalf("blob 内容应与上传一致")
	}

	// 同一张图再传：去重 200，不新增不广播
	rec = postImage(t, s, "macbook", tinyPNG)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复图片应去重返回 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	if n := len(bc.newCalls); n != 1 {
		t.Fatalf("重复图片不应广播，得到 %d", n)
	}

	// 删除连带清理落盘文件
	blobURL := c.BlobURL
	if rec := do(t, s, "DELETE", fmt.Sprintf("/api/clips/%d", c.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("删除图片应 204，得到 %d", rec.Code)
	}
	if rec := do(t, s, "GET", blobURL, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("删除后 blob 应 404，得到 %d", rec.Code)
	}
}

func TestCreateImageValidation(t *testing.T) {
	s, _ := newTestServerWithBlobs(t)

	// 非图片内容
	rec := postImage(t, s, "dev", []byte("this is not an image"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非图片应 415，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	// 非法 base64
	rec = do(t, s, "POST", "/api/clips/image", `{"data":"@@@not-base64@@@","device":"d"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 base64 应 400，得到 %d", rec.Code)
	}
	// 空数据
	rec = do(t, s, "POST", "/api/clips/image", `{"data":"","device":"d"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空数据应 400，得到 %d", rec.Code)
	}
	// data URL 前缀兼容
	rec = do(t, s, "POST", "/api/clips/image",
		fmt.Sprintf(`{"data":"data:image/png;base64,%s","device":"d"}`,
			base64.StdEncoding.EncodeToString(tinyPNG)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("data URL 前缀应被兼容，得到 %d body=%s", rec.Code, rec.Body.String())
	}
}

// SetStore 存储热切换:切换后新写入进新库,与旧库数据互不可见。
func TestSetStoreHotSwap(t *testing.T) {
	s, _ := newTestServerWithBlobs(t)

	// 旧库写入一条
	if rec := do(t, s, "POST", "/api/clips", `{"type":"text","text":"old-store","device":"d"}`); rec.Code != http.StatusCreated {
		t.Fatalf("旧库上传应 201，得到 %d", rec.Code)
	}
	s.SetStore(memstore.New())
	// 新库为空:列表应为空
	rec := do(t, s, "GET", "/api/clips", "")
	if !strings.Contains(rec.Body.String(), `"clips":[]`) {
		t.Fatalf("切换后新库应为空: %s", rec.Body.String())
	}
	// 新库写入
	if rec := do(t, s, "POST", "/api/clips", `{"type":"text","text":"new-store","device":"d"}`); rec.Code != http.StatusCreated {
		t.Fatalf("新库上传应 201，得到 %d", rec.Code)
	}
}
