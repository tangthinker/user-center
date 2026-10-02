// Package store 提供「按实例隔离」的 SQLite 存储。
//
// 设计约束（docs/auth-redesign.md §0.1）：
//   - L2 状态按实例隔离：每个 Store 独占一个数据库文件，包级无任何全局变量；
//   - L3 初始化失败返回 error，**绝不 panic**（库 panic 会带走宿主进程）；
//   - L4 配置由调用方注入，不读取任何环境变量；
//   - L5 不写全局日志：GORM 日志被丢弃，需要观测请使用宿主自己的 Hooks。
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tangthinker/user-center/v2/internal/migrate"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// FileName 是数据目录下的默认库文件名。
const FileName = "user-center.db"

// DefaultMaxOpenConns 是默认连接数上限。
//
// SQLite 是单写者模型：把连接数压到 1 可以从根本上消除 SQLITE_BUSY，
// 对"个位数用户 + 单实例"的场景是最省心的选择。宿主可自行调高。
const DefaultMaxOpenConns = 1

// ErrClosed 表示在已关闭的 Store 上执行操作。
var ErrClosed = errors.New("store: already closed")

// Config 是存储配置。
type Config struct {
	// DBPath 以 ".db" 结尾时视为数据库文件路径，否则视为目录，
	// 库文件为 <DBPath>/user-center.db。
	DBPath string

	// SkipMigration 为 true 时不执行 schema 迁移。
	// 仅用于只读副本、演练或由运维单独执行迁移的场景。
	SkipMigration bool

	// FileMode 是库文件权限，0 时使用 0600（库内含 PII）。
	FileMode os.FileMode

	// MaxOpenConns 为 0 时使用 DefaultMaxOpenConns。
	MaxOpenConns int
}

// Store 是一个独立的存储实例。
type Store struct {
	db   *gorm.DB
	sql  interface{ Close() error }
	path string

	mu     sync.Mutex
	closed bool
}

// Open 打开（必要时创建）数据库，并在需要时执行 schema 迁移。
func Open(cfg Config) (*Store, error) {
	path, err := resolvePath(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	gdb, err := gorm.Open(sqlite.Open(dsn(path)), &gorm.Config{
		// 库不接管宿主的日志：丢弃 GORM 的 SQL 日志（L5）。
		Logger: logger.Discard,
		// 时间统一用 UTC，避免字符串比较与夏令时带来的边界问题。
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("store: obtain sql.DB: %w", err)
	}

	maxConns := cfg.MaxOpenConns
	if maxConns <= 0 {
		maxConns = DefaultMaxOpenConns
	}
	sqlDB.SetMaxOpenConns(maxConns)
	sqlDB.SetMaxIdleConns(maxConns)

	// 库内含邮箱等个人信息，收敛文件权限。
	mode := cfg.FileMode
	if mode == 0 {
		mode = 0o600
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, statErr := os.Stat(p); statErr == nil {
			_ = os.Chmod(p, mode)
		}
	}

	if !cfg.SkipMigration {
		if err := migrate.Run(context.Background(), gdb); err != nil {
			_ = sqlDB.Close()
			return nil, err
		}
	}

	return &Store{db: gdb, sql: sqlDB, path: path}, nil
}

// DB 返回 GORM 句柄。**在事务内必须使用事务句柄**，否则会因为
// MaxOpenConns=1 而自锁。
func (s *Store) DB() *gorm.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// Path 返回实际的数据库文件路径。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Close 关闭底层连接。可重复调用。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.sql == nil {
		return nil
	}
	if err := s.sql.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// Closed 报告实例是否已关闭。
func (s *Store) Closed() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// resolvePath 把 Config.DBPath 解析为数据库文件路径。
func resolvePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("store: Config.DBPath is required")
	}
	if strings.HasSuffix(strings.ToLower(p), ".db") {
		dir := filepath.Dir(p)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("store: create dir %s: %w", dir, err)
		}
		return p, nil
	}
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", fmt.Errorf("store: create dir %s: %w", p, err)
	}
	return filepath.Join(p, FileName), nil
}

// dsn 组装 mattn/go-sqlite3 的连接串。
//
// 参数含义与取舍见 docs/auth-redesign.md §3.2：
//   - WAL：跨进程一写多读，且读不阻塞写；
//   - busy_timeout：写锁竞争时等待而非立刻报错；
//   - synchronous=NORMAL：WAL 下的常规折中；
//   - foreign_keys=on：让 ON DELETE CASCADE 真正生效；
//   - txlock=immediate：写事务一开始就取写锁，避免锁升级失败。
func dsn(path string) string {
	return "file:" + path +
		"?_journal_mode=WAL" +
		"&_busy_timeout=5000" +
		"&_synchronous=NORMAL" +
		"&_foreign_keys=on" +
		"&_txlock=immediate"
}
