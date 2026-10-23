// Package pgstore 提供 store.Store 的 PostgreSQL 实现（基于 jackc/pgx v5 连接池）。
// 首次启动自动建表 / 建索引（幂等，可重复执行）。
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lanpaste/server/internal/store"
)

// schema 建表 SQL（PG 方言）。字段与 PROTOCOL.md 的 clip 对象对应；
// v1.1 起图片使用 blob_path / mime / width / height，文本行这些列恒为 NULL / 0。
const schema = `
CREATE TABLE IF NOT EXISTS clips (
  id            BIGSERIAL PRIMARY KEY,
  hash          TEXT NOT NULL,
  type          TEXT NOT NULL,
  text          TEXT,
  blob_path     TEXT,
  mime          TEXT,
  width         INTEGER,
  height        INTEGER,
  size          BIGINT,
  source_device TEXT NOT NULL,
  user_id       BIGINT NOT NULL DEFAULT 0,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_clips_created_at ON clips (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_clips_hash ON clips (hash);
CREATE INDEX IF NOT EXISTS idx_clips_user ON clips (user_id, created_at DESC);
`

// migrations 对 v1.0 已建表的老库做幂等补列（CREATE TABLE IF NOT EXISTS 不会补列）。
var migrations = []string{
	`ALTER TABLE clips ADD COLUMN IF NOT EXISTS mime TEXT`,
	`ALTER TABLE clips ADD COLUMN IF NOT EXISTS width INTEGER`,
	`ALTER TABLE clips ADD COLUMN IF NOT EXISTS height INTEGER`,
	`ALTER TABLE clips ADD COLUMN IF NOT EXISTS user_id BIGINT DEFAULT 0`,
}

// 迁移后补 v1.2 用户隔离索引（CREATE INDEX IF NOT EXISTS 已在 schema 中，老库走这里）
var indexMigrations = []string{
	`CREATE INDEX IF NOT EXISTS idx_clips_user ON clips (user_id, created_at DESC)`,
}

// Store PostgreSQL 存储实现。
type Store struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*Store)(nil)

// New 建立连接池并幂等建表。dsn 示例：
// postgres://paste:pass@10.0.0.x:5432/paste?sslmode=disable
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: 创建连接池失败: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: ping 失败: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: 建表失败: %w", err)
	}
	for _, m := range migrations {
		if _, err := pool.Exec(ctx, m); err != nil {
			pool.Close()
			return nil, fmt.Errorf("pgstore: 迁移失败（%s）: %w", m, err)
		}
	}
	for _, m := range indexMigrations {
		if _, err := pool.Exec(ctx, m); err != nil {
			pool.Close()
			return nil, fmt.Errorf("pgstore: 索引迁移失败（%s）: %w", m, err)
		}
	}
	return &Store{pool: pool}, nil
}

// Close 关闭连接池。
func (s *Store) Close() { s.pool.Close() }

const cols = `id, hash, type, text, blob_path, mime, width, height, size, source_device, user_id, created_at`

func scanClip(row pgx.Row) (*store.Clip, error) {
	var c store.Clip
	err := row.Scan(&c.ID, &c.Hash, &c.Type, &c.Text, &c.BlobPath, &c.Mime, &c.Width, &c.Height,
		&c.Size, &c.SourceDevice, &c.UserID, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Add 插入记录，ID 与 created_at 由数据库生成后回填。
func (s *Store) Add(ctx context.Context, c *store.Clip) error {
	return s.pool.QueryRow(ctx,
		`INSERT INTO clips (hash, type, text, blob_path, mime, width, height, size, source_device, user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id, created_at`,
		c.Hash, c.Type, c.Text, c.BlobPath, c.Mime, c.Width, c.Height, c.Size, c.SourceDevice, c.UserID,
	).Scan(&c.ID, &c.CreatedAt)
}

// Latest 返回最新一条；无记录返回 (nil, nil)。
func (s *Store) Latest(ctx context.Context, userID int64) (*store.Clip, error) {
	c, err := scanClip(s.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM clips WHERE ($1 = 0 OR user_id = $1) ORDER BY created_at DESC, id DESC LIMIT 1`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pgstore: Latest: %w", err)
	}
	return c, nil
}

// List 按时间降序返回最多 limit 条。
func (s *Store) List(ctx context.Context, userID int64, limit int) ([]store.Clip, error) {
	if limit <= 0 {
		return []store.Clip{}, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+cols+` FROM clips WHERE ($1 = 0 OR user_id = $1) ORDER BY created_at DESC, id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("pgstore: List: %w", err)
	}
	defer rows.Close()

	out := make([]store.Clip, 0, limit)
	for rows.Next() {
		var c store.Clip
		if err := rows.Scan(&c.ID, &c.Hash, &c.Type, &c.Text, &c.BlobPath, &c.Mime, &c.Width, &c.Height,
			&c.Size, &c.SourceDevice, &c.UserID, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("pgstore: List scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get 按 ID 查询；不存在返回 (nil, nil)。
func (s *Store) Get(ctx context.Context, id int64) (*store.Clip, error) {
	c, err := scanClip(s.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM clips WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pgstore: Get: %w", err)
	}
	return c, nil
}

// Delete 删除单条；目标不存在返回 false。
func (s *Store) Delete(ctx context.Context, id int64) (bool, error) {
	ct, err := s.pool.Exec(ctx, `DELETE FROM clips WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("pgstore: Delete: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

// AdoptLegacy 把 UserID=0 的历史记录归入指定用户。
func (s *Store) AdoptLegacy(ctx context.Context, userID int64) error {
	ct, err := s.pool.Exec(ctx, `UPDATE clips SET user_id = $1 WHERE user_id = 0`, userID)
	if err != nil {
		return fmt.Errorf("pgstore: AdoptLegacy: %w", err)
	}
	_ = ct
	return nil
}

// DeleteOlderThan 删除早于 t 的记录，返回被删记录的附件路径（连带删文件用）。
func (s *Store) DeleteOlderThan(ctx context.Context, t time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`DELETE FROM clips WHERE created_at < $1 RETURNING blob_path`, t)
	if err != nil {
		return nil, fmt.Errorf("pgstore: DeleteOlderThan: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p *string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("pgstore: DeleteOlderThan scan: %w", err)
		}
		if p != nil && *p != "" {
			paths = append(paths, *p)
		}
	}
	return paths, rows.Err()
}
