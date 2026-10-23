// Package auth 实现多用户账号体系：注册 / 登录 / 设备令牌。
//
// 设计：
//   - 用户与设备绑定关系持久化在 <data-dir>/users.json（0600），与剪贴板存储
//     解耦——内存模式下账号依然可用，也不依赖 PG/MySQL 的表结构
//   - 密码使用 bcrypt 加盐哈希，明文不落盘
//   - 每台设备登录后签发独立的设备令牌（32 字节随机 hex）；服务端只存令牌的
//     sha256，泄露 users.json 也无法直接伪造令牌
//   - 设备令牌 = 该用户的身份：同一账号的所有设备共享同一份剪贴板
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// User 一个账号。
type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Hash      []byte    `json:"hash"` // bcrypt
	CreatedAt time.Time `json:"created_at"`
}

// Device 一台已登录设备（令牌 → 用户 的绑定）。
type Device struct {
	TokenHash string    `json:"token_hash"` // sha256(token) hex
	UserID    int64     `json:"user_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// fileData users.json 的磁盘结构。
type fileData struct {
	NextID  int64     `json:"next_id"`
	Users   []*User   `json:"users"`
	Devices []*Device `json:"devices"`
}

var (
	usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_\-]{2,32}$`)
	// 常见错误（API 层直接透出给登录页）
	ErrBadUsername    = errors.New("用户名只能是 2-32 位字母、数字、下划线或中划线")
	ErrBadPassword    = errors.New("密码至少 6 位")
	ErrUserExists     = errors.New("用户名已被注册")
	ErrBadCredentials = errors.New("用户名或密码错误")
)

// Store 账号存储，并发安全。
type Store struct {
	mu      sync.Mutex
	path    string
	data    fileData
	byName  map[string]*User   // key: username
	devices map[string]*Device // key: token hash
}

// Load 读取 users.json；文件不存在时返回空存储（首次启动）。
func Load(path string) (*Store, error) {
	s := &Store{
		path:    path,
		devices: map[string]*Device{},
		byName:  map[string]*User{},
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("auth: 读取 %s 失败: %w", path, err)
	}
	var data fileData
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("auth: 解析 %s 失败: %w（可删除该文件重建账号体系）", path, err)
	}
	s.data = data
	for _, u := range data.Users {
		s.byName[u.Username] = u
	}
	for _, d := range data.Devices {
		s.devices[d.TokenHash] = d
	}
	return s, nil
}

// save 原子写盘（调用方须持锁）。
func (s *Store) save() error {
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("auth: 写入失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("auth: 保存失败: %w", err)
	}
	return nil
}

// Register 创建账号；首个账号与其他账号权限一致（局域网自用，无角色体系）。
func (s *Store) Register(username, password string) (*User, error) {
	if !usernameRe.MatchString(username) {
		return nil, ErrBadUsername
	}
	if len(password) < 6 {
		return nil, ErrBadPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byName[username]; ok {
		return nil, ErrUserExists
	}
	s.data.NextID++
	u := &User{
		ID:        s.data.NextID,
		Username:  username,
		Hash:      hash,
		CreatedAt: time.Now().UTC(),
	}
	s.data.Users = append(s.data.Users, u)
	s.byName[username] = u
	if err := s.save(); err != nil {
		s.data.NextID--
		s.data.Users = s.data.Users[:len(s.data.Users)-1]
		delete(s.byName, username)
		return nil, err
	}
	return u, nil
}

// Login 校验用户名密码，为设备签发新令牌。device 为设备名（记录用）。
func (s *Store) Login(username, password, device string) (token string, u *User, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.byName[username]
	if user == nil {
		// 用户不存在也走一次 bcrypt 比较，避免时序侧信道探测用户名
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5B0X1wXbFfGvJ2A2GnHh.pBNuQk6W"), []byte(password))
		return "", nil, ErrBadCredentials
	}
	if err := bcrypt.CompareHashAndPassword(user.Hash, []byte(password)); err != nil {
		return "", nil, ErrBadCredentials
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	token = hex.EncodeToString(buf)
	now := time.Now().UTC()
	d := &Device{
		TokenHash: hashToken(token),
		UserID:    user.ID,
		Name:      device,
		CreatedAt: now,
		LastSeen:  now,
	}
	s.data.Devices = append(s.data.Devices, d)
	if err := s.save(); err != nil {
		s.data.Devices = s.data.Devices[:len(s.data.Devices)-1]
		return "", nil, err
	}
	s.devices[d.TokenHash] = d
	return token, user, nil
}

// ResetPassword 管理员重置用户密码，并注销该用户的所有设备令牌（强制重新登录）。
func (s *Store) ResetPassword(username, newPassword string) error {
	if len(newPassword) < 6 {
		return ErrBadPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.byName[username]
	if !ok {
		return errors.New("用户不存在")
	}
	user.Hash = hash
	// 注销该用户全部设备令牌
	kept := s.data.Devices[:0]
	for _, d := range s.data.Devices {
		if d.UserID != user.ID {
			kept = append(kept, d)
		} else {
			delete(s.devices, d.TokenHash)
		}
	}
	s.data.Devices = kept
	if err := s.save(); err != nil {
		return err
	}
	return nil
}

// VerifyToken 校验设备令牌，返回所属用户 ID；同时刷新 last_seen。
func (s *Store) VerifyToken(token string) (int64, bool) {
	if token == "" {
		return 0, false
	}
	h := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[h]
	if !ok {
		return 0, false
	}
	d.LastSeen = time.Now().UTC()
	return d.UserID, true
}

// RemoveDevice 注销设备令牌（登出）。
func (s *Store) RemoveDevice(token string) bool {
	h := hashToken(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[h]
	if !ok {
		return false
	}
	delete(s.devices, h)
	for i, x := range s.data.Devices {
		if x == d {
			s.data.Devices = append(s.data.Devices[:i], s.data.Devices[i+1:]...)
			break
		}
	}
	_ = s.save()
	return true
}

// UserCount 返回已注册账号数（0 = 尚无任何用户）。
func (s *Store) UserCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data.Users)
}

// Username 返回用户 ID 对应的用户名（日志/展示用，找不到返回空）。
func (s *Store) Username(userID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.data.Users {
		if u.ID == userID {
			return u.Username
		}
	}
	return ""
}

// hashToken 令牌的 sha256 hex（存储形式）。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
