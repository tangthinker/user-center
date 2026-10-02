// Package testsupport 提供跨包复用的测试辅助。
//
// 它位于 internal/ 之下，宿主无法导入；只在测试中被引用。
package testsupport

import (
	"sync"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/store"
	"gorm.io/gorm"
)

// OpenDB 在临时目录中打开一个已迁移的空库，并在测试结束时关闭。
func OpenDB(t *testing.T) *gorm.DB {
	t.Helper()
	s, err := store.Open(store.Config{DBPath: t.TempDir()})
	if err != nil {
		t.Fatalf("testsupport: open db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.DB()
}

// CreateUser 插入一个用户并返回它。
// uid 为 nil 表示该用户仍处于 invited（待激活）状态。
func CreateUser(t *testing.T, db *gorm.DB, email, status string, uid *string) *domain.User {
	t.Helper()
	now := time.Now().UTC()
	u := &domain.User{
		UID:       uid,
		Email:     email,
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("testsupport: create user %s: %v", email, err)
	}
	return u
}

// Clock 是可手动推进的时钟，用于验证过期与续期。
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock 返回一个停在 t 的时钟。
func NewClock(t time.Time) *Clock { return &Clock{t: t.UTC()} }

// Now 实现可注入的时钟接口。
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance 向前推进 d。
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Set 直接设置时间。
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t.UTC()
}
