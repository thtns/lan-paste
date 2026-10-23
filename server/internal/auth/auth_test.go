package auth

import (
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Load(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func TestRegisterAndLogin(t *testing.T) {
	s := newStore(t)

	u, err := s.Register("alice", "secret6")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if u.ID != 1 {
		t.Fatalf("首个用户 ID 应为 1，得到 %d", u.ID)
	}

	// 正确密码登录 → 签发设备令牌
	tok, user, err := s.Login("alice", "secret6", "mac-1")
	if err != nil || len(tok) != 64 {
		t.Fatalf("登录应返回 64 位 hex 令牌: %v %q", err, tok)
	}
	if user.ID != u.ID {
		t.Fatalf("登录用户不符")
	}

	// 令牌校验 → 用户 ID
	uid, ok := s.VerifyToken(tok)
	if !ok || uid != u.ID {
		t.Fatalf("VerifyToken 应返回用户 %d，得到 %d/%v", u.ID, uid, ok)
	}

	// 第二台设备登录 → 令牌不同但同属一个用户
	tok2, _, err := s.Login("alice", "secret6", "mac-2")
	if err != nil || tok2 == tok {
		t.Fatalf("第二台设备应获得不同令牌")
	}
	if uid, _ := s.VerifyToken(tok2); uid != u.ID {
		t.Fatalf("第二台设备应属同一用户")
	}

	// 错误密码
	if _, _, err := s.Login("alice", "wrong!", "dev-x"); err == nil {
		t.Fatalf("错误密码应失败")
	}
	// 不存在的用户
	if _, _, err := s.Login("nobody", "secret6", "dev-x"); err == nil {
		t.Fatalf("不存在用户应失败")
	}
}

func TestRegisterValidation(t *testing.T) {
	s := newStore(t)
	if _, err := s.Register("ab", "secret6"); err != nil {
		t.Fatalf("2 位用户名应合法: %v", err)
	}
	if _, err := s.Register("", "secret6"); err == nil {
		t.Fatalf("空用户名应拒绝")
	}
	if _, err := s.Register("有中文", "secret6"); err == nil {
		t.Fatalf("中文用户名应拒绝")
	}
	if _, err := s.Register("alice", "12345"); err == nil {
		t.Fatalf("短密码应拒绝")
	}
	if _, err := s.Register("alice", "secret6"); err != nil {
		t.Fatalf("首次注册: %v", err)
	}
	if _, err := s.Register("alice", "other66"); err == nil {
		t.Fatalf("重复用户名应拒绝")
	}
}

func TestRemoveDeviceAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	s, _ := Load(path)
	if _, err := s.Register("u1", "secret6"); err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.Login("u1", "secret6", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !s.RemoveDevice(tok) {
		t.Fatalf("应成功移除设备令牌")
	}
	if _, ok := s.VerifyToken(tok); ok {
		t.Fatalf("移除后令牌应失效")
	}

	// 重新 Load:用户仍在,旧设备令牌已失效
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s2.Login("u1", "secret6", "dev2"); err != nil {
		t.Fatalf("持久化后应能登录: %v", err)
	}
	if _, ok := s2.VerifyToken(tok); ok {
		t.Fatalf("持久化后被移除的令牌应仍失效")
	}
}
