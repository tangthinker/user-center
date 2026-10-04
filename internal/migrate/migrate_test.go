package migrate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/migrate"
	"github.com/tangthinker/user-center/v2/internal/store"
	"gorm.io/gorm"
)

// rawDB 返回一个未执行迁移的空库，便于单独测试迁移行为。
func rawDB(t *testing.T) *gorm.DB {
	t.Helper()
	s, err := store.Open(store.Config{DBPath: t.TempDir(), SkipMigration: true})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.DB()
}

func migratedDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := rawDB(t)
	if err := migrate.Run(context.Background(), db); err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}
	return db
}

func tableExists(t *testing.T, db *gorm.DB, name string) bool {
	t.Helper()
	var count int
	if err := db.Raw(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Row().Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count > 0
}

func TestRunCreatesAllTables(t *testing.T) {
	db := migratedDB(t)
	for _, name := range []string{
		"schema_migrations", "users", "invitations", "otp_codes",
		"sessions", "mail_outbox", "mail_log", "admin_audit",
	} {
		if !tableExists(t, db, name) {
			t.Errorf("table %q not created", name)
		}
	}
}

func TestRunRecordsLatestVersion(t *testing.T) {
	db := migratedDB(t)

	var version int
	if err := db.Raw(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Row().Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != migrate.LatestVersion() {
		t.Fatalf("applied version = %d, want %d", version, migrate.LatestVersion())
	}
}

// 幂等且可重入：重复执行不得报错、不得重复记录版本
func TestRunIsIdempotent(t *testing.T) {
	db := migratedDB(t)

	var before int
	if err := db.Raw(`SELECT COUNT(*) FROM schema_migrations`).Row().Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("首次迁移没有记录任何版本")
	}

	for i := 0; i < 3; i++ {
		if err := migrate.Run(context.Background(), db); err != nil {
			t.Fatalf("run #%d: %v", i+2, err)
		}
	}

	// 断言"版本行数没变 + 无重复版本"，而不是断言行数等于某个具体数字：
	// 原先写成 len([]int{migrate.LatestVersion()})（恒为 1），在只有 1 个迁移时
	// 侥幸通过，追加第 2 个迁移后就会报"rows = 2, want 2"这种自相矛盾的失败。
	var rows, distinct int
	if err := db.Raw(`SELECT COUNT(*), COUNT(DISTINCT version) FROM schema_migrations`).
		Row().Scan(&rows, &distinct); err != nil {
		t.Fatal(err)
	}
	if rows != before {
		t.Fatalf("重复执行后 schema_migrations 行数 = %d, want %d", rows, before)
	}
	if rows != distinct {
		t.Fatalf("schema_migrations 出现重复版本：rows=%d distinct=%d", rows, distinct)
	}
}

// 迁移纪律（§12.2）：只做加法，且**旧数据必须原样保留**。
//
// 这里手工搭一个"已经跑到 v1"的库（含一条历史会话），再执行迁移，
// 验证 ALTER TABLE 不会丢数据、旧会话的设备字段为空但不影响读取。
func TestUpgradeKeepsExistingSessions(t *testing.T) {
	db := rawDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := db.Exec(`CREATE TABLE schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (1, 'init', ?)`,
		now).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE sessions (
		token_hash          BLOB PRIMARY KEY,
		user_id             INTEGER NOT NULL,
		uid                 TEXT    NOT NULL,
		scope               TEXT    NOT NULL,
		issued_at           DATETIME NOT NULL,
		idle_expires_at     DATETIME NOT NULL,
		absolute_expires_at DATETIME NOT NULL,
		last_seen_at        DATETIME NOT NULL,
		ip                  TEXT    NULL,
		ua_hash             TEXT    NULL,
		revoked_at          DATETIME NULL,
		revoke_reason       TEXT    NULL
	)`).Error; err != nil {
		t.Fatal(err)
	}
	// token_hash 用 SQL 字面量写入：GORM 会把 []byte 参数当成切片展开成多个占位符。
	if err := db.Exec(`INSERT INTO sessions
		(token_hash, user_id, uid, scope, issued_at, idle_expires_at, absolute_expires_at, last_seen_at, ip)
		VALUES (X'010203', 1, 'alice', 'user', ?, ?, ?, ?, '203.0.113.7')`,
		now, now.Add(time.Hour), now.Add(24*time.Hour), now).Error; err != nil {
		t.Fatal(err)
	}

	if err := migrate.Run(ctx, db); err != nil {
		t.Fatalf("migrate.Run: %v", err)
	}
	if got := migrate.LatestVersion(); got < 2 {
		t.Fatalf("LatestVersion = %d, 期望至少 2（本测试需要 v2 的会话设备列）", got)
	}

	var (
		uid      string
		ip       string
		deviceID *string
	)
	if err := db.Raw(`SELECT uid, ip, device_id FROM sessions`).Row().Scan(&uid, &ip, &deviceID); err != nil {
		t.Fatalf("升级后读不回历史会话：%v", err)
	}
	if uid != "alice" || ip != "203.0.113.7" {
		t.Fatalf("升级改动了历史数据：uid=%q ip=%q", uid, ip)
	}
	if deviceID != nil {
		t.Fatalf("历史会话的 device_id 应为 NULL（未知设备），得到 %q", *deviceID)
	}
}

// 核心保护：旧版本的库遇到新 schema 必须拒绝启动（§12.2）
func TestRunRejectsNewerSchema(t *testing.T) {
	db := migratedDB(t)

	if err := db.Exec(
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, CURRENT_TIMESTAMP)`,
		migrate.LatestVersion()+1000, "from-the-future",
	).Error; err != nil {
		t.Fatal(err)
	}

	err := migrate.Run(context.Background(), db)
	if !errors.Is(err, migrate.ErrSchemaTooNew) {
		t.Fatalf("err = %v, want ErrSchemaTooNew", err)
	}
	if !strings.Contains(err.Error(), "newer than this library version") {
		t.Errorf("error message should explain the cause, got: %v", err)
	}
}

func TestRunRejectsNilDB(t *testing.T) {
	if err := migrate.Run(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil db")
	}
}

// ux_otp_active：同一 (email, purpose) 至多一个未消费的码（不变量 I4）
func TestOTPActiveCodeUniqueness(t *testing.T) {
	db := migratedDB(t)
	now := time.Now().UTC()
	exp := now.Add(10 * time.Minute)

	insert := func(consumed bool) error {
		var consumedAt any
		if consumed {
			consumedAt = now
		}
		return db.Exec(
			`INSERT INTO otp_codes (email, purpose, code_hmac, issued_at, expires_at, attempts, consumed_at)
			 VALUES (?, ?, ?, ?, ?, 0, ?)`,
			"a@example.com", "login", []byte{1, 2, 3}, now, exp, consumedAt,
		).Error
	}

	if err := insert(false); err != nil {
		t.Fatalf("first active code: %v", err)
	}
	if err := insert(false); err == nil {
		t.Fatal("second active code for same (email,purpose) must be rejected")
	}
	// 不同 purpose 互不影响
	if err := db.Exec(
		`INSERT INTO otp_codes (email, purpose, code_hmac, issued_at, expires_at, attempts)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		"a@example.com", "admin_login", []byte{4}, now, exp,
	).Error; err != nil {
		t.Fatalf("different purpose should be allowed: %v", err)
	}

	// 已消费的码不占用唯一性
	if err := insert(true); err != nil {
		t.Fatalf("consumed code should not block insertion: %v", err)
	}
}

// ux_users_uid：允许多个 NULL，非 NULL 唯一且**大小写敏感**
func TestUIDUniquenessAndCaseSensitivity(t *testing.T) {
	db := migratedDB(t)
	now := time.Now().UTC()

	newUser := func(email string, uid *string) error {
		return db.Exec(
			`INSERT INTO users (uid, email, status, is_admin, created_at, updated_at)
			 VALUES (?, ?, 'active', 0, ?, ?)`,
			uid, email, now, now,
		).Error
	}

	// 多个待激活用户（uid IS NULL）必须可以共存
	if err := newUser("a@example.com", nil); err != nil {
		t.Fatalf("invited user A: %v", err)
	}
	if err := newUser("b@example.com", nil); err != nil {
		t.Fatalf("invited user B: %v", err)
	}

	alice := "Alice"
	if err := newUser("alice@example.com", &alice); err != nil {
		t.Fatalf("create Alice: %v", err)
	}
	// 大小写不同 = 不同用户（设计 §5.2：大小写敏感）
	lower := "alice"
	if err := newUser("alice2@example.com", &lower); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	// 完全相同则冲突
	if err := newUser("alice3@example.com", &alice); err == nil {
		t.Fatal("duplicate uid must be rejected")
	}
}

func TestEmailUniqueness(t *testing.T) {
	db := migratedDB(t)
	now := time.Now().UTC()

	insert := func(email string) error {
		return db.Exec(
			`INSERT INTO users (email, status, is_admin, created_at, updated_at)
			 VALUES (?, 'invited', 0, ?, ?)`, email, now, now,
		).Error
	}

	if err := insert("dup@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := insert("dup@example.com"); err == nil {
		t.Fatal("duplicate email must be rejected")
	}
}

// 外键与级联删除必须真正生效（依赖 foreign_keys=on）
func TestForeignKeysAndCascade(t *testing.T) {
	db := migratedDB(t)
	now := time.Now().UTC()

	if err := db.Exec(
		`INSERT INTO invitations (token_hash, user_id, email, purpose, issued_at, expires_at, created_by)
		 VALUES (?, 99999, 'x@example.com', 'invite', ?, ?, 1)`,
		[]byte{9}, now, now.Add(time.Hour),
	).Error; err == nil {
		t.Fatal("insert with dangling user_id must be rejected by foreign key")
	}

	// 正常插入后删除用户，邀请应被级联删除
	if err := db.Exec(
		`INSERT INTO users (id, email, status, is_admin, created_at, updated_at)
		 VALUES (1, 'y@example.com', 'invited', 0, ?, ?)`, now, now,
	).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(
		`INSERT INTO invitations (token_hash, user_id, email, purpose, issued_at, expires_at, created_by)
		 VALUES (?, 1, 'y@example.com', 'invite', ?, ?, 1)`,
		[]byte{8}, now, now.Add(time.Hour),
	).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`DELETE FROM users WHERE id = 1`).Error; err != nil {
		t.Fatal(err)
	}

	var left int
	if err := db.Raw(`SELECT COUNT(*) FROM invitations WHERE user_id = 1`).Row().Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("invitations not cascaded on user delete, left=%d", left)
	}
}

// 发件箱的幂等键（设计 §7.4）
func TestOutboxDedupeKeyIsUnique(t *testing.T) {
	db := migratedDB(t)
	now := time.Now().UTC()

	insert := func() error {
		return db.Exec(
			`INSERT INTO mail_outbox (dedupe_key, to_email, template, priority, status, attempts, next_attempt_at, created_at)
			 VALUES ('otp:1', 'a@example.com', 'otp_code', 100, 'pending', 0, ?, ?)`, now, now,
		).Error
	}
	if err := insert(); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("duplicate dedupe_key must be rejected (幂等)")
	}
}
