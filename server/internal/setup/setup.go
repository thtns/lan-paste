// Package setup 实现服务端的「引导配置」：
// 首次部署时无需手工编辑配置文件/环境变量，浏览器打开服务端地址即可在向导页里
// 选择存储后端（内存 / PostgreSQL / MySQL）、填写连接参数、测试连接并保存。
//
// 配置持久化为 <data-dir>/config.json；保存成功后通过 main 注入的回调热切换存储后端，
// 仅监听地址变更需要 systemctl restart 生效。
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lanpaste/server/internal/store/mysqlstore"
)

// 支持的存储后端。
const (
	StoreMemory   = "memory"
	StorePostgres = "postgres"
	StoreMySQL    = "mysql"
)

// DefaultListen 默认监听地址。
const DefaultListen = ":8080"

// DBConfig 数据库连接参数（向导表单字段）。
type DBConfig struct {
	Store    string `json:"store"` // memory | postgres | mysql
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"` // 0 = 各后端默认端口
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"` // 永不回显给前端
	Database string `json:"database,omitempty"`
	SSLMode  string `json:"sslmode,omitempty"` // 仅 postgres：disable（默认）/ require / …
}

// Config 完整配置文件结构。
type Config struct {
	Listen string `json:"listen"`
	// RetentionDays 历史保留天数：超过即连同附件一起清理。0 = 永不清理。
	// 用指针区分「未传」（局部更新时沿用现有值，如部署向导不关心此字段）与「显式 0」。
	// 负值视为非法。
	RetentionDays *int     `json:"retention_days,omitempty"`
	DB            DBConfig `json:"db"`
}

// retentionValue 返回保留天数（未设置时按 0 处理）。
func (c *Config) retentionValue() int {
	if c.RetentionDays == nil {
		return 0
	}
	return *c.RetentionDays
}

var dbNameRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// Manager 配置管理器：加载 / 测试 / 持久化，并发安全。
type Manager struct {
	mu         sync.Mutex
	path       string // config.json 绝对路径
	configured bool
	cfg        Config
	dsn        string // 由 cfg 派生（含密码），只供内部建立连接使用
}

// Load 读取配置文件；文件不存在时返回未配置状态的 Manager（memory + 默认端口）。
func Load(path string) (*Manager, error) {
	m := &Manager{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, nil // 首次启动：未配置
		}
		return nil, fmt.Errorf("setup: 读取配置 %s 失败: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("setup: 解析配置 %s 失败: %w（可删除该文件重新走向导）", path, err)
	}
	dsn, err := buildDSN(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("setup: 配置 %s 无效: %w", path, err)
	}
	m.cfg = cfg
	m.dsn = dsn
	m.configured = true
	return m, nil
}

// Configured 是否已完成过引导（config.json 存在且合法）。
func (m *Manager) Configured() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.configured
}

// Listen 返回配置的监听地址（未配置返回空串，由 main 决定默认值）。
func (m *Manager) Listen() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Listen
}

// RetentionDays 返回配置的历史保留天数（0 = 永不清理）。
// TTL 清理任务每次 tick 都重新读取，向导热更新后无需重启即生效。
func (m *Manager) RetentionDays() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.retentionValue()
}

// StoreType 返回配置的存储类型（未配置返回 memory）。
func (m *Manager) StoreType() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg.DB.Store == "" {
		return StoreMemory
	}
	return m.cfg.DB.Store
}

// DSN 返回当前配置派生的连接串（含密码，勿外泄）。
func (m *Manager) DSN() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dsn
}

// ---- 连接串构造 ----

// buildDSN 由表单参数构造各后端的 DSN。
func buildDSN(db DBConfig) (string, error) {
	switch db.Store {
	case "", StoreMemory:
		return "", nil
	case StorePostgres:
		if db.Host == "" || db.Database == "" {
			return "", errors.New("PostgreSQL 需要填写主机与数据库名")
		}
		port := db.Port
		if port == 0 {
			port = 5432
		}
		ssl := db.SSLMode
		if ssl == "" {
			ssl = "disable" // 局域网内网默认不加密（TLS 由部署层提供）
		}
		u := url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(db.User, db.Password),
			Host:   fmt.Sprintf("%s:%d", db.Host, port),
			Path:   "/" + db.Database,
		}
		q := url.Values{"sslmode": {ssl}}
		u.RawQuery = q.Encode()
		return u.String(), nil
	case StoreMySQL:
		if db.Host == "" || db.Database == "" {
			return "", errors.New("MySQL 需要填写主机与数据库名")
		}
		port := db.Port
		if port == 0 {
			port = 3306
		}
		// parseTime=true：created_at DATETIME 扫描进 time.Time 必需
		return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&loc=Local",
			db.User, db.Password, db.Host, port, db.Database), nil
	default:
		return "", fmt.Errorf("不支持的存储类型 %q（memory / postgres / mysql）", db.Store)
	}
}

// Normalize 补齐默认值并校验（保存与测试共用）。
func (c *Config) Normalize() error {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen // 允许只填端口 8080
	}
	if c.RetentionDays != nil && *c.RetentionDays < 0 {
		return errors.New("历史保留天数不能为负（0 表示永不清理）")
	}
	switch c.DB.Store {
	case StoreMemory:
		c.DB = DBConfig{Store: StoreMemory}
		return nil
	case StorePostgres, StoreMySQL:
	default:
		return fmt.Errorf("不支持的存储类型 %q", c.DB.Store)
	}
	if !dbNameRe.MatchString(c.DB.Database) {
		return errors.New("数据库名只允许字母、数字与下划线")
	}
	return nil
}

// Test 尝试建立数据库连接；库不存在且账号有权限时自动 CREATE DATABASE 后重试。
// 任何一步都带 5s 超时，避免向导页长时间转圈。
func (m *Manager) Test(ctx context.Context, db DBConfig) error {
	if db.Store == StoreMemory {
		return nil
	}
	dsn, err := buildDSN(db)
	if err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = pingStore(tctx, db, dsn)
	if err == nil {
		return nil
	}
	// 库不存在 → 尝试用同一账号在服务器上建库（PVE 自建实例通常允许），成功则重试一次
	if !isMissingDB(db.Store, err) {
		return err
	}
	if err2 := createDatabase(tctx, db); err2 != nil {
		return fmt.Errorf("%w（且自动建库失败：%v）", err, err2)
	}
	tctx2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	return pingStore(tctx2, db, dsn)
}

func pingStore(ctx context.Context, db DBConfig, dsn string) error {
	switch db.Store {
	case StorePostgres:
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return err
		}
		defer pool.Close()
		return pool.Ping(ctx)
	case StoreMySQL:
		s, err := mysqlstore.New(ctx, dsn) // New 内含 ping + 幂等建表
		if err != nil {
			return err
		}
		s.Close()
		return nil
	}
	return fmt.Errorf("不支持的存储类型 %q", db.Store)
}

func isMissingDB(storeType string, err error) bool {
	msg := err.Error()
	switch storeType {
	case StorePostgres:
		return strings.Contains(msg, "3D000") || strings.Contains(msg, "does not exist")
	case StoreMySQL:
		return strings.Contains(msg, "1049") || strings.Contains(msg, "Unknown database")
	}
	return false
}

// createDatabase 库不存在时自动建库（尽力而为，失败返回错误由调用方拼进提示）。
func createDatabase(ctx context.Context, db DBConfig) error {
	switch db.Store {
	case StorePostgres:
		admin, err := buildDSN(DBConfig{Store: StorePostgres, Host: db.Host, Port: db.Port,
			User: db.User, Password: db.Password, Database: "postgres", SSLMode: db.SSLMode})
		if err != nil {
			return err
		}
		pool, err := pgxpool.New(ctx, admin)
		if err != nil {
			return err
		}
		defer pool.Close()
		_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, db.Database))
		return err
	case StoreMySQL:
		server, err := buildDSN(DBConfig{Store: StoreMySQL, Host: db.Host, Port: db.Port,
			User: db.User, Password: db.Password, Database: ""})
		if err != nil {
			return err
		}
		return mysqlstore.CreateDatabase(ctx, server, db.Database)
	}
	return fmt.Errorf("不支持的存储类型 %q", db.Store)
}

// InheritPassword 用现有配置补齐留空的密码（页面不回显密码，局部更新时沿用原值）。
// 必须在连接测试之前调用——否则测试会拿空密码去 ping，必然失败。
// 仅当存储类型未变时继承；换了数据库类型说明用户在重新配置，空密码按字面处理。
func (m *Manager) InheritPassword(cfg *Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cfg.DB.Password == "" && m.configured && m.cfg.DB.Password != "" &&
		cfg.DB.Store == m.cfg.DB.Store {
		cfg.DB.Password = m.cfg.DB.Password
	}
}

// Save 校验 → 原子写入 config.json（0600）→ 更新内存状态。不负责热切换（由 main 回调完成）。
// 局部更新约定：db.password 留空 = 沿用现有密码；retention_days 未传（nil）= 沿用现有值。
func (m *Manager) Save(cfg Config) error {
	if err := cfg.Normalize(); err != nil {
		return err
	}
	m.InheritPassword(&cfg)
	m.mu.Lock()
	if cfg.RetentionDays == nil {
		if m.configured {
			v := m.cfg.retentionValue()
			cfg.RetentionDays = &v
		} else {
			v := 0
			cfg.RetentionDays = &v
		}
	}
	dsn, err := buildDSN(cfg.DB)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		m.mu.Unlock()
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("setup: 写入配置失败: %w", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		_ = os.Remove(tmp)
		m.mu.Unlock()
		return fmt.Errorf("setup: 保存配置失败: %w", err)
	}
	m.cfg = cfg
	m.dsn = dsn
	m.configured = true
	m.mu.Unlock()
	return nil
}

// Status 返回给前端的状态视图（不含密码）。
type Status struct {
	Configured    bool     `json:"configured"`
	Listen        string   `json:"listen"`
	Store         string   `json:"store"`
	RetentionDays int      `json:"retention_days"`
	DB            DBConfig `json:"db"` // Password 字段恒为空
}

// View 生成当前状态（供 GET /api/setup）。内部直接读字段，避免嵌套加锁。
func (m *Manager) View() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	store := m.cfg.DB.Store
	listen := m.cfg.Listen
	if store == "" {
		store = StoreMemory
	}
	if listen == "" {
		listen = DefaultListen
	}
	return Status{
		Configured:    m.configured,
		Listen:        listen,
		Store:         store,
		RetentionDays: m.cfg.retentionValue(),
		DB: DBConfig{
			Store:    m.cfg.DB.Store,
			Host:     m.cfg.DB.Host,
			Port:     m.cfg.DB.Port,
			User:     m.cfg.DB.User,
			Database: m.cfg.DB.Database,
			SSLMode:  m.cfg.DB.SSLMode,
		},
	}
}

// Path 配置文件路径（main 打日志用）。
func (m *Manager) Path() string { return m.path }

// Dir 配置文件所在目录。
func (m *Manager) Dir() string { return filepath.Dir(m.path) }
