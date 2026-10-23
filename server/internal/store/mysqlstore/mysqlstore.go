// Package mysqlstore 提供 store.Store 的 MySQL 实现（基于 go-sql-driver/mysql）。
// 与 pgstore 同构；差异仅在方言：AUTO_INCREMENT 主键、DATETIME(6)、无 RETURNING
// （插入后用 LastInsertId 回查 created_at）。首次启动自动建表 / 建索引（幂等）。
package mysqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql" // database/sql 驱动注册

	"lanpaste/server/internal/store"
)

// schema 建表 SQL（MySQL 方言）。
const schema = `
CREATE TABLE IF NOT EXISTS clips (
  id            BIGINT AUTO_INCREMENT PRIMARY KEY,
  hash          VARCHAR(64) NOT NULL,
  type          VARCHAR(16) NOT NULL,
  text          MEDIUMTEXT NULL,
  blob_path     VARCHAR(512) NULL,
  mime          VARCHAR(64) NULL,
  width         INT NULL,
  height        INT NULL,
  size          BIGINT NULL,
  source_device VARCHAR(128) NOT NULL,
  user_id       BIGINT NOT NULL DEFAULT 0,
  created_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  INDEX idx_clips_created_at (created_at DESC),
  INDEX idx_clips_hash (hash),
  INDEX idx_clips_user (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
`

// ensureColumns 为老版本建的表补列（MySQL 8 不支持 ADD COLUMN IF NOT EXISTS，
// 通过 information_schema 判断后再 ALTER；幂等）。
func (s *Store) ensureColumns(ctx context.Context) error {
	type col struct{ name, ddl string }
	for _, c := range []col{
		{"mime", "ALTER TABLE clips ADD COLUMN mime VARCHAR(64) NULL"},
		{"width", "ALTER TABLE clips ADD COLUMN width INT NULL"},
		{"height", "ALTER TABLE clips ADD COLUMN height INT NULL"},
		{"user_id", "ALTER TABLE clips ADD COLUMN user_id BIGINT NOT NULL DEFAULT 0"},
	} {
		var count int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.COLUMNS `+
				`WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'clips' AND COLUMN_NAME = ?`,
			c.name).Scan(&count); err != nil {
			return fmt.Errorf("mysqlstore: 检查列 %s 失败: %w", c.name, err)
		}
		if count > 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx, c.ddl); err != nil {
			return fmt.Errorf("mysqlstore: 补列 %s 失败: %w", c.name, err)
		}
	}
	return nil
}

// Store MySQL 存储实现。
type Store struct {
	db *sql.DB
}

var _ store.Store = (*Store)(nil)

// New 打开连接并幂等建表。dsn 示例（parseTime 必开，否则 created_at 扫描失败）：
// user:pass@tcp(10.0.0.x:3306)/paste?parseTime=true&loc=UTC
func New(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: 打开连接失败: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("mysqlstore: ping 失败: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("mysqlstore: 建表失败: %w", err)
	}
	st := &Store{db: db}
	if err := st.ensureColumns(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

// CreateDatabase 在指定 MySQL 服务器上建库（库不存在时的向导自动建库，尽力而为）。
// serverDSN 不含库名（以 .../?... 结尾）。
func CreateDatabase(ctx context.Context, serverDSN, name string) error {
	db, err := sql.Open("mysql", serverDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	// 名称已由 setup.Normalize 用 ^[A-Za-z0-9_]+$ 校验，反引号包裹是双保险
	_, err = db.ExecContext(ctx,
		fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4", name))
	return err
}

// Close 关闭连接池。
func (s *Store) Close() { _ = s.db.Close() }

const cols = `id, hash, type, text, blob_path, mime, width, height, size, source_device, user_id, created_at`

func scanClip(row interface{ Scan(...any) error }) (*store.Clip, error) {
	var c store.Clip
	err := row.Scan(&c.ID, &c.Hash, &c.Type, &c.Text, &c.BlobPath, &c.Mime, &c.Width, &c.Height,
		&c.Size, &c.SourceDevice, &c.UserID, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Add 插入记录并回填 ID / CreatedAt（MySQL 无 RETURNING：先取自增 ID 再回查时间戳）。
func (s *Store) Add(ctx context.Context, c *store.Clip) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO clips (hash, type, text, blob_path, mime, width, height, size, source_device, user_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Hash, c.Type, c.Text, c.BlobPath, c.Mime, c.Width, c.Height, c.Size, c.SourceDevice, c.UserID)
	if err != nil {
		return fmt.Errorf("mysqlstore: Add: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("mysqlstore: LastInsertId: %w", err)
	}
	c.ID = id
	if err := s.db.QueryRowContext(ctx,
		`SELECT created_at FROM clips WHERE id = ?`, id).Scan(&c.CreatedAt); err != nil {
		return fmt.Errorf("mysqlstore: 回查 created_at: %w", err)
	}
	return nil
}

// Latest 返回指定用户的最新一条（userID == 0 时跨全部用户）；无记录返回 (nil, nil)。
func (s *Store) Latest(ctx context.Context, userID int64) (*store.Clip, error) {
	c, err := scanClip(s.db.QueryRowContext(ctx,
		`SELECT `+cols+` FROM clips WHERE (? = 0 OR user_id = ?) ORDER BY created_at DESC, id DESC LIMIT 1`,
		userID, userID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: Latest: %w", err)
	}
	return c, nil
}

// List 按时间降序返回最多 limit 条。
func (s *Store) List(ctx context.Context, userID int64, limit int) ([]store.Clip, error) {
	if limit <= 0 {
		return []store.Clip{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cols+` FROM clips WHERE (? = 0 OR user_id = ?) ORDER BY created_at DESC, id DESC LIMIT ?`,
		userID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: List: %w", err)
	}
	defer rows.Close()

	out := make([]store.Clip, 0, limit)
	for rows.Next() {
		var c store.Clip
		if err := rows.Scan(&c.ID, &c.Hash, &c.Type, &c.Text, &c.BlobPath, &c.Mime, &c.Width, &c.Height,
			&c.Size, &c.SourceDevice, &c.UserID, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("mysqlstore: List scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get 按 ID 查询；不存在返回 (nil, nil)。
func (s *Store) Get(ctx context.Context, id int64) (*store.Clip, error) {
	c, err := scanClip(s.db.QueryRowContext(ctx,
		`SELECT `+cols+` FROM clips WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: Get: %w", err)
	}
	return c, nil
}

// Delete 删除单条；目标不存在返回 false。
func (s *Store) Delete(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM clips WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("mysqlstore: Delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mysqlstore: Delete RowsAffected: %w", err)
	}
	return n > 0, nil
}

// AdoptLegacy 把 UserID=0 的历史记录归入指定用户。
func (s *Store) AdoptLegacy(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE clips SET user_id = ? WHERE user_id = 0`, userID)
	if err != nil {
		return fmt.Errorf("mysqlstore: AdoptLegacy: %w", err)
	}
	return nil
}

// DeleteOlderThan 删除早于 t 的记录，返回被删记录的附件路径（连带删文件用）。
// MySQL 无 DELETE...RETURNING：先查路径再删除（同一连接内两步，规模小无一致性风险）。
func (s *Store) DeleteOlderThan(ctx context.Context, t time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT blob_path FROM clips WHERE created_at < ?`, t)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: DeleteOlderThan 查询: %w", err)
	}
	var paths []string
	for rows.Next() {
		var p sql.NullString
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, fmt.Errorf("mysqlstore: DeleteOlderThan scan: %w", err)
		}
		if p.Valid && p.String != "" {
			paths = append(paths, p.String)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysqlstore: DeleteOlderThan rows: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM clips WHERE created_at < ?`, t); err != nil {
		return nil, fmt.Errorf("mysqlstore: DeleteOlderThan 删除: %w", err)
	}
	return paths, nil
}
