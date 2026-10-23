package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestSetup(t *testing.T) (*Manager, *Server) {
	t.Helper()
	dir := t.TempDir()
	mgr, err := Load(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var appliedStore, appliedToken string
	srv := &Server{
		Mgr:          mgr,
		DataDir:      dir,
		Version:      "test",
		CurrentToken: func() string { return "tok-1" },
		ApplyStore: func(_ context.Context, cfg Config, dsn string) (func(), error) {
			appliedStore = cfg.DB.Store
			return nil, nil
		},
		ApplyToken: func(tok string) error {
			appliedToken = tok
			return nil
		},
	}
	t.Cleanup(func() {
		_ = appliedStore
		_ = appliedToken
	})
	return mgr, srv
}

func req(t *testing.T, srv *Server, method, path, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.Register(mux)
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rdr)
	if auth != "" {
		r.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func TestUnconfiguredFlow(t *testing.T) {
	mgr, srv := newTestSetup(t)

	// 未配置：无 token 可读状态（首次引导放开）
	rec := req(t, srv, "GET", "/api/setup", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("未配置 GET /api/setup 应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	var st struct {
		Configured bool   `json:"configured"`
		Token      string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if st.Configured || st.Token != "tok-1" {
		t.Fatalf("状态不符: %+v", st)
	}

	// 保存内存配置（无需测试连接）
	rec = req(t, srv, "POST", "/api/setup", `{"listen":":9000","db":{"store":"memory"}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("保存 memory 配置应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	if !mgr.Configured() || mgr.StoreType() != StoreMemory {
		t.Fatalf("保存后应为 configured/memory")
	}

	// 配置文件确实落盘，且重新 Load 一致
	b, err := os.ReadFile(mgr.Path())
	if err != nil {
		t.Fatalf("config.json 应存在: %v", err)
	}
	if !strings.Contains(string(b), `":9000"`) {
		t.Fatalf("配置文件应含监听地址: %s", b)
	}
	mgr2, err := Load(mgr.Path())
	if err != nil || !mgr2.Configured() {
		t.Fatalf("重新加载配置失败: %v", err)
	}
}

func TestConfiguredRequiresAuth(t *testing.T) {
	mgr, srv := newTestSetup(t)
	if err := mgr.Save(Config{Listen: ":8080", DB: DBConfig{Store: StoreMemory}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 已配置后：无 token → 401
	if rec := req(t, srv, "GET", "/api/setup", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("已配置无 token 应 401，得到 %d", rec.Code)
	}
	if rec := req(t, srv, "POST", "/api/setup", `{"db":{"store":"memory"}}`, "wrong"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 token 应 401，得到 %d", rec.Code)
	}
	// 正确 token → 200
	if rec := req(t, srv, "GET", "/api/setup", "", "tok-1"); rec.Code != http.StatusOK {
		t.Fatalf("正确 token 应 200，得到 %d", rec.Code)
	}
}

func TestSaveValidatesDBFields(t *testing.T) {
	mgr, srv := newTestSetup(t)

	// postgres 缺数据库名 → 400
	rec := req(t, srv, "POST", "/api/setup",
		`{"db":{"store":"postgres","host":"10.0.0.8"}}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺库名应 400，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	// 非法库名 → 400
	rec = req(t, srv, "POST", "/api/setup",
		`{"db":{"store":"postgres","host":"h","database":"bad-name;drop"}}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法库名应 400，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	// 无法连接的 postgres → 测试失败 400（连接错误信息透出）
	rec = req(t, srv, "POST", "/api/setup",
		`{"db":{"store":"postgres","host":"127.0.0.1","port":59999,"database":"paste","user":"u","password":"p"}}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("连不上的库应 400，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "连接测试失败") {
		t.Fatalf("应带连接测试失败说明: %s", rec.Body.String())
	}
	if mgr.Configured() {
		t.Fatalf("保存失败不应进入 configured 状态")
	}
}

func TestRegenToken(t *testing.T) {
	_, srv := newTestSetup(t)
	rec := req(t, srv, "POST", "/api/setup/token", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("重新生成 token 应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Token) != 64 {
		t.Fatalf("应返回 64 位 hex token: %+v err=%v", out, err)
	}
}

func TestBuildDSN(t *testing.T) {
	dsn, err := buildDSN(DBConfig{Store: StorePostgres, Host: "10.0.0.8", Database: "paste",
		User: "paste", Password: "p@ss/w:rd", SSLMode: ""})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	// 密码里的特殊字符必须被正确转义（url.UserPassword）
	if !strings.Contains(dsn, "sslmode=disable") || !strings.HasPrefix(dsn, "postgres://paste:") {
		t.Fatalf("PG DSN 不符: %s", dsn)
	}
	dsn, err = buildDSN(DBConfig{Store: StoreMySQL, Host: "10.0.0.9", Database: "paste",
		User: "paste", Password: "pw"})
	if err != nil {
		t.Fatalf("buildDSN: %v", err)
	}
	if !strings.Contains(dsn, "tcp(10.0.0.9:3306)/paste?parseTime=true") {
		t.Fatalf("MySQL DSN 不符: %s", dsn)
	}
}

// 局部更新语义：retention_days 未传 = 沿用现有值；显式 0 = 关闭；负数拒绝。
func TestRetentionPartialUpdate(t *testing.T) {
	mgr, srv := newTestSetup(t)

	// 首次保存（不传 retention）→ 0
	if rec := req(t, srv, "POST", "/api/setup", `{"db":{"store":"memory"}}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("首次保存应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := mgr.RetentionDays(); got != 0 {
		t.Fatalf("默认保留天数应为 0，得到 %d", got)
	}

	// 设置页显式改为 7
	if rec := req(t, srv, "POST", "/api/setup", `{"db":{"store":"memory"},"retention_days":7}`, "tok-1"); rec.Code != http.StatusOK {
		t.Fatalf("设置 7 应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := mgr.RetentionDays(); got != 7 {
		t.Fatalf("保留天数应为 7，得到 %d", got)
	}

	// 向导再保存（不传 retention）→ 沿用 7 而不是归零
	if rec := req(t, srv, "POST", "/api/setup", `{"db":{"store":"memory"}}`, "tok-1"); rec.Code != http.StatusOK {
		t.Fatalf("向导再保存应 200，得到 %d", rec.Code)
	}
	if got := mgr.RetentionDays(); got != 7 {
		t.Fatalf("不传 retention 应沿用现有值 7，得到 %d", got)
	}

	// 负数拒绝
	if rec := req(t, srv, "POST", "/api/setup", `{"db":{"store":"memory"},"retention_days":-1}`, "tok-1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("负数应 400，得到 %d", rec.Code)
	}
}

// 密码继承：已配置后，密码留空的局部更新（设置页/向导重配）应沿用原密码。
func TestInheritPassword(t *testing.T) {
	mgr, _ := newTestSetup(t)
	if err := mgr.Save(Config{Listen: ":8080", DB: DBConfig{
		Store: StoreMySQL, Host: "10.0.0.9", Database: "paste",
		User: "root", Password: "real-secret",
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 设置页场景：密码留空 + 同存储类型 → 沿用
	cfg := Config{DB: DBConfig{Store: StoreMySQL, Host: "10.0.0.9", Database: "paste", User: "root"}}
	mgr.InheritPassword(&cfg)
	if cfg.DB.Password != "real-secret" {
		t.Fatalf("留空密码应沿用原值，得到 %q", cfg.DB.Password)
	}

	// 换了存储类型（重新配置）：空密码按字面处理，不继承
	cfg2 := Config{DB: DBConfig{Store: StorePostgres, Host: "h", Database: "paste"}}
	mgr.InheritPassword(&cfg2)
	if cfg2.DB.Password != "" {
		t.Fatalf("换存储类型不应继承密码，得到 %q", cfg2.DB.Password)
	}

	// 显式填了新密码：原样保留
	cfg3 := Config{DB: DBConfig{Store: StoreMySQL, Host: "10.0.0.9", Database: "paste", Password: "new-pass"}}
	mgr.InheritPassword(&cfg3)
	if cfg3.DB.Password != "new-pass" {
		t.Fatalf("显式密码不应被覆盖，得到 %q", cfg3.DB.Password)
	}

	// Save 后 config.json 里的密码仍是继承后的原值（设置页保存不丢密码）
	if err := mgr.Save(Config{DB: DBConfig{Store: StoreMySQL, Host: "10.0.0.9", Database: "paste", User: "root"}}); err != nil {
		t.Fatalf("局部保存应 200 级成功: %v", err)
	}
	if mgr.DSN() == "" || !contains(mgr.DSN(), "real-secret") {
		t.Fatalf("保存后的 DSN 应包含沿用下来的密码: %s", mgr.DSN())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
