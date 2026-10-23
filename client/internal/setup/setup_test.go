package setup

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeAuthServer 模拟服务端的注册/登录/健康检查/令牌校验。
type fakeAuthServer struct {
	registered  atomic.Bool
	registerErr string      // 非空：注册返回 400 + 该错误
	failLogin   atomic.Bool // 常开：登录一律 401
	failNLogin  int32       // 前 N 次登录返回 401（模拟一次性失败）
	loginCalls  atomic.Int32
	token       string
	regCalls    atomic.Int32
	lastLoginDe atomic.Value
}

func newFakeAuthServer() *fakeAuthServer {
	return &fakeAuthServer{token: "tok-123"}
}

func (f *fakeAuthServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/register", func(w http.ResponseWriter, r *http.Request) {
		f.regCalls.Add(1)
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("register 请求体不是 JSON: %v", err)
		}
		if req.Username == "" || len(req.Password) < 6 {
			writeErr(w, "密码至少 6 位")
			return
		}
		if f.registered.Load() {
			writeErr(w, "用户名已存在")
			return
		}
		if f.registerErr != "" {
			writeErr(w, f.registerErr)
			return
		}
		f.registered.Store(true)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Device   string `json:"device"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("login 请求体不是 JSON: %v", err)
		}
		f.lastLoginDe.Store(req.Device)
		n := f.loginCalls.Add(1)
		if f.failLogin.Load() || n <= f.failNLogin {
			writeErr(w, "用户名或密码错误")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": f.token, "user": req.Username})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/clips/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func writeErr(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// stubDialogs 替换平台交互函数；formCalls 回传 askForm 次数，alerts 收集提示。
func stubDialogs(t *testing.T, forms []Credentials, retries []retryAction) (formCalls *int, alerts *[]string) {
	t.Helper()
	origForm, origAlert, origRetry := askForm, alertOK, askRetryOrRegister
	calls := 0
	var msgs []string
	retryIdx := 0
	askForm = func(prefill Credentials) (Credentials, bool) {
		n := calls // 计数所有调用（含最后一次取消）
		calls++
		if n >= len(forms) {
			return Credentials{}, false // 脚本耗尽 = 用户取消
		}
		return forms[n], true
	}
	alertOK = func(title, message string) { msgs = append(msgs, title+": "+message) }
	askRetryOrRegister = func(string) retryAction {
		if retryIdx >= len(retries) {
			return actionCancel
		}
		a := retries[retryIdx]
		retryIdx++
		return a
	}
	t.Cleanup(func() {
		askForm, alertOK, askRetryOrRegister = origForm, origAlert, origRetry
	})
	return &calls, &msgs
}

func TestNormalizeServer(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://10.0.0.5:8080", "http://10.0.0.5:8080"},
		{"10.0.0.5:8080", "http://10.0.0.5:8080"},
		{"  https://paste.lan/  ", "https://paste.lan"},
		{"192.168.1.10:8080/", "http://192.168.1.10:8080"},
		{"", ""},
		{"not a url at all", ""}, // host 带空格 → 非法
		{"ftp://x", ""},          // 非 http(s) scheme
		{"http://", ""},          // 缺 host
	}
	for _, c := range cases {
		if got := NormalizeServer(c.in); got != c.want {
			t.Errorf("NormalizeServer(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestRunSuccess(t *testing.T) {
	fake := newFakeAuthServer()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	form := Credentials{Server: srv.URL, Username: "liu", Password: "secret123", Device: "mbp"}
	calls, alerts := stubDialogs(t, []Credentials{form}, nil)

	res, err := Run(Credentials{Device: "mbp"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Token != "tok-123" || res.Username != "liu" || res.Device != "mbp" || res.Server != srv.URL {
		t.Fatalf("结果不符: %+v", res)
	}
	if *calls != 1 {
		t.Fatalf("askForm 应只调一次，实际 %d", *calls)
	}
	if len(*alerts) != 0 {
		t.Fatalf("不应有提示: %v", *alerts)
	}
	if fake.lastLoginDe.Load() != "mbp" {
		t.Fatalf("登录应携带设备名，收到 %v", fake.lastLoginDe.Load())
	}
}

func TestRunInvalidServerAlertsThenRetries(t *testing.T) {
	fake := newFakeAuthServer()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	bad := Credentials{Server: "无效 地址", Username: "liu", Password: "secret123", Device: "mbp"}
	good := Credentials{Server: srv.URL, Username: "liu", Password: "secret123", Device: "mbp"}
	calls, alerts := stubDialogs(t, []Credentials{bad, good}, nil)

	res, err := Run(Credentials{Device: "mbp"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Token != "tok-123" {
		t.Fatalf("第二次应成功: %+v", res)
	}
	if *calls != 2 {
		t.Fatalf("应弹两次表单，实际 %d", *calls)
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "服务器地址无效") {
		t.Fatalf("应提示一次地址无效: %v", *alerts)
	}
}

// 表单勾选注册（Windows 表单路径）：直接注册 → 登录成功。
func TestRunRegisterViaCheckbox(t *testing.T) {
	fake := newFakeAuthServer()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	form := Credentials{Server: srv.URL, Username: "new", Password: "secret123", Device: "pc", Register: true}
	_, alerts := stubDialogs(t, []Credentials{form}, nil)

	res, err := Run(Credentials{Device: "pc"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.regCalls.Load() != 1 || res.Token != "tok-123" {
		t.Fatalf("应注册一次并登录成功: reg=%d res=%+v", fake.regCalls.Load(), res)
	}
	if len(*alerts) != 0 {
		t.Fatalf("不应有提示: %v", *alerts)
	}
}

// 勾选注册但服务端拒绝：提示后回到表单（脚本耗尽 → 取消）。
func TestRunRegisterFailAlerts(t *testing.T) {
	fake := newFakeAuthServer()
	fake.registerErr = "用户名含非法字符"
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	form := Credentials{Server: srv.URL, Username: "bad!", Password: "secret123", Device: "pc", Register: true}
	_, alerts := stubDialogs(t, []Credentials{form}, nil)

	if _, err := Run(Credentials{Device: "pc"}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("脚本耗尽应取消，得到 %v", err)
	}
	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "注册失败") {
		t.Fatalf("应提示注册失败: %v", *alerts)
	}
}

// 登录失败 → 选「注册并登录」→ 同一凭据原地注册重试（不再重弹表单）。
func TestRunLoginFailThenRegisterInPlace(t *testing.T) {
	fake := newFakeAuthServer()
	fake.failNLogin = 1
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	form := Credentials{Server: srv.URL, Username: "newuser", Password: "secret123", Device: ""}
	calls, _ := stubDialogs(t, []Credentials{form}, []retryAction{actionRegister})

	res, err := Run(Credentials{Device: "host-default"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.regCalls.Load() != 1 {
		t.Fatalf("应注册一次，实际 %d", fake.regCalls.Load())
	}
	if res.Token != "tok-123" {
		t.Fatalf("注册后应登录成功: %+v", res)
	}
	if res.Device != "host-default" {
		t.Fatalf("设备名空应回退 prefill: %+v", res)
	}
	if *calls != 1 {
		t.Fatalf("原地注册不应重弹表单，实际弹了 %d 次", *calls)
	}
}

// 登录失败 → 选注册，但用户名其实已存在：跳过注册直接登录。
func TestRunRegisterExistingFallsThroughToLogin(t *testing.T) {
	fake := newFakeAuthServer()
	fake.registered.Store(true)
	fake.failNLogin = 1
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	form := Credentials{Server: srv.URL, Username: "liu", Password: "secret123", Device: "mbp"}
	calls, alerts := stubDialogs(t, []Credentials{form}, []retryAction{actionRegister})

	res, err := Run(Credentials{Device: "mbp"})
	if err != nil {
		t.Fatalf("「已存在」应继续登录: %v", err)
	}
	if res.Token != "tok-123" || *calls != 1 || len(*alerts) != 0 {
		t.Fatalf("结果不符: res=%+v calls=%d alerts=%v", res, *calls, *alerts)
	}
}

// 登录失败 → 选「重新填写」→ 第二次成功。
func TestRunLoginFailRetryForm(t *testing.T) {
	fake := newFakeAuthServer()
	fake.failLogin.Store(true)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	form := Credentials{Server: srv.URL, Username: "liu", Password: "wrong", Device: "mbp"}
	calls, _ := stubDialogs(t, []Credentials{form}, []retryAction{actionRetry})

	if _, err := Run(Credentials{Device: "mbp"}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("第二次表单耗尽应取消，得到 %v", err)
	}
	if *calls != 2 {
		t.Fatalf("应重弹表单一次，实际 %d", *calls)
	}
}

func TestRunCancel(t *testing.T) {
	calls, _ := stubDialogs(t, nil, nil)
	if _, err := Run(Credentials{Device: "mbp"}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("期望 ErrCancelled，得到 %v", err)
	}
	if *calls != 1 {
		t.Fatalf("askForm 应被调用一次，实际 %d", *calls)
	}
}

func TestVerifyRejectsBadToken(t *testing.T) {
	fake := newFakeAuthServer()
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	if err := verify(srv.URL, "tok-123"); err != nil {
		t.Fatalf("正确 token 应通过: %v", err)
	}
	if err := verify(srv.URL, "wrong"); err == nil {
		t.Fatalf("错误 token 应失败")
	}
	if err := verify("http://127.0.0.1:1", "tok-123"); err == nil {
		t.Fatalf("不可达服务器应失败")
	}
}
