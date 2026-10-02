// Package migrate 负责显式的、版本化的 schema 迁移。
//
// 关键约束（docs/auth-redesign.md §12.2）：
//   - 同一个主版本内，schema 变更**只做加法**；
//   - 旧版本的库遇到由更新版本创建的数据库时，必须**拒绝启动**，
//     而不是照常读写（否则会静默损坏数据）。
package migrate

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// ErrSchemaTooNew 表示数据库的 schema 版本高于本库已知的最高版本。
//
// 触发场景：宿主先用 v2.3.0 的库跑了迁移，之后回退到 v2.1.0 的库启动。
// 正确处置是拒绝启动，由运维决定是恢复备份还是升级库（设计 §9.2/§12.2）。
var ErrSchemaTooNew = errors.New("migrate: database schema is newer than this library version")

// Migration 是一个不可变的 schema 版本。
type Migration struct {
	Version    int
	Name       string
	Statements []string
}

// LatestVersion 返回本库已知的最高 schema 版本；没有任何迁移时返回 0。
func LatestVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].Version
}

// Run 按版本顺序应用尚未执行的迁移。
//
// 它是幂等且可重入的：已应用的版本会被跳过；每个迁移在独立事务中执行，
// 失败即回滚且不记录版本号。
func Run(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("migrate: nil db")
	}
	if err := ensureVersionTable(ctx, db); err != nil {
		return err
	}

	applied, err := maxAppliedVersion(ctx, db)
	if err != nil {
		return err
	}
	if latest := LatestVersion(); applied > latest {
		return fmt.Errorf("%w (db=%d, lib=%d)", ErrSchemaTooNew, applied, latest)
	}

	for _, m := range migrations {
		if m.Version <= applied {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func ensureVersionTable(ctx context.Context, db *gorm.DB) error {
	const ddl = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at DATETIME NOT NULL
	)`
	if err := db.WithContext(ctx).Exec(ddl).Error; err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}
	return nil
}

func maxAppliedVersion(ctx context.Context, db *gorm.DB) (int, error) {
	var version int
	row := db.WithContext(ctx).Raw(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Row()
	if err := row.Scan(&version); err != nil {
		return 0, fmt.Errorf("migrate: read schema version: %w", err)
	}
	return version, nil
}

func applyOne(ctx context.Context, db *gorm.DB, m Migration) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for i, stmt := range m.Statements {
			if err := tx.Exec(stmt).Error; err != nil {
				return fmt.Errorf("migrate: v%d (%s) statement %d: %w", m.Version, m.Name, i+1, err)
			}
		}
		if err := tx.Exec(
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, CURRENT_TIMESTAMP)`,
			m.Version, m.Name,
		).Error; err != nil {
			return fmt.Errorf("migrate: record v%d: %w", m.Version, err)
		}
		return nil
	})
}
