package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/store"
)

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(store.Config{DBPath: t.TempDir()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func pragmaString(t *testing.T, s *store.Store, name string) string {
	t.Helper()
	var v string
	if err := s.DB().Raw("PRAGMA " + name).Row().Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func pragmaInt(t *testing.T, s *store.Store, name string) int {
	t.Helper()
	var v int
	if err := s.DB().Raw("PRAGMA " + name).Row().Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

// 目录形式：库文件应为 <dir>/user-center.db
func TestOpenWithDirectoryPath(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(store.Config{DBPath: dir})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	want := filepath.Join(dir, store.FileName)
	if s.Path() != want {
		t.Fatalf("Path() = %q, want %q", s.Path(), want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("database file not created: %v", err)
	}
}

// 文件形式：以 .db 结尾时直接作为文件路径，并自动创建父目录
func TestOpenWithFilePath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "nested", "deeper", "custom.db")
	s, err := store.Open(store.Config{DBPath: file})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.Path() != file {
		t.Fatalf("Path() = %q, want %q", s.Path(), file)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("database file not created: %v", err)
	}
}

func TestOpenRequiresPath(t *testing.T) {
	for _, p := range []string{"", "   "} {
		if _, err := store.Open(store.Config{DBPath: p}); err == nil {
			t.Fatalf("expected error for DBPath=%q", p)
		}
	}
}

// 无效路径必须返回 error，而不是 panic —— 库 panic 会带走宿主进程（L3）
func TestOpenInvalidPathReturnsError(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "plain-file")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 把一个普通文件当作目录使用
	_, err := store.Open(store.Config{DBPath: filepath.Join(regular, "sub")})
	if err == nil {
		t.Fatal("expected error when a regular file is used as a directory")
	}
}

// §3.2 的连接参数必须真正生效
func TestConnectionPragmasApplied(t *testing.T) {
	s := openTemp(t)

	if got := strings.ToLower(pragmaString(t, s, "journal_mode")); got != "wal" {
		t.Errorf("journal_mode = %q, want wal", got)
	}
	if got := pragmaInt(t, s, "foreign_keys"); got != 1 {
		t.Errorf("foreign_keys = %d, want 1", got)
	}
	if got := pragmaInt(t, s, "busy_timeout"); got != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", got)
	}
}

// SkipMigration：只建连接，不建表
func TestSkipMigrationLeavesSchemaEmpty(t *testing.T) {
	s, err := store.Open(store.Config{DBPath: t.TempDir(), SkipMigration: true})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var count int
	if err := s.DB().Raw(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='users'`,
	).Row().Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("users table should not exist when SkipMigration=true, count=%d", count)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s, err := store.Open(store.Config{DBPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close should be a no-op: %v", err)
	}
	if !s.Closed() {
		t.Fatal("Closed() = false after Close")
	}
}

// L2：两个实例必须完全隔离
func TestInstancesAreIsolated(t *testing.T) {
	a := openTemp(t)
	b := openTemp(t)

	if a.Path() == b.Path() {
		t.Fatalf("two instances share the same file: %s", a.Path())
	}

	now := time.Now().UTC()
	if err := a.DB().Create(&domain.User{
		Email: "a@example.com", Status: domain.UserStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("insert into A: %v", err)
	}

	var countA, countB int64
	if err := a.DB().Model(&domain.User{}).Count(&countA).Error; err != nil {
		t.Fatal(err)
	}
	if err := b.DB().Model(&domain.User{}).Count(&countB).Error; err != nil {
		t.Fatal(err)
	}
	if countA != 1 || countB != 0 {
		t.Fatalf("isolation broken: countA=%d countB=%d", countA, countB)
	}
}

// 库文件含 PII，权限必须是 0600
func TestDatabaseFilePermissions(t *testing.T) {
	s := openTemp(t)
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db file mode = %o, want 600", perm)
	}
}

// GORM 模型映射与返回值形态
func TestModelRoundTrip(t *testing.T) {
	s := openTemp(t)
	now := time.Now().UTC().Truncate(time.Second)

	uid := "Alice"
	user := &domain.User{
		UID: &uid, Email: "alice@example.com", Status: domain.UserStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB().Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.ID == 0 {
		t.Fatal("auto increment id not populated")
	}

	var got domain.User
	if err := s.DB().Where("email = ?", "alice@example.com").First(&got).Error; err != nil {
		t.Fatalf("first: %v", err)
	}
	if got.UIDValue() != "Alice" {
		t.Errorf("UIDValue() = %q, want Alice", got.UIDValue())
	}
	if !got.IsActive() {
		t.Error("IsActive() = false")
	}
	if got.CreatedAt.UTC().Unix() != now.Unix() {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt.UTC(), now)
	}

	// 未激活用户：uid 为 NULL，UIDValue 返回空串
	invited := &domain.User{
		Email: "bob@example.com", Status: domain.UserStatusInvited,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.DB().Create(invited).Error; err != nil {
		t.Fatalf("create invited user: %v", err)
	}
	var gotInvited domain.User
	if err := s.DB().Where("email = ?", "bob@example.com").First(&gotInvited).Error; err != nil {
		t.Fatal(err)
	}
	if gotInvited.UID != nil {
		t.Errorf("uid should be NULL for invited user, got %v", *gotInvited.UID)
	}
	if gotInvited.UIDValue() != "" {
		t.Errorf("UIDValue() = %q, want empty", gotInvited.UIDValue())
	}
}
