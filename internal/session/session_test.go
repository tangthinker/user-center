package session_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/secure"
	"github.com/tangthinker/user-center/v2/internal/session"
	"github.com/tangthinker/user-center/v2/internal/testsupport"
	"gorm.io/gorm"
)

const (
	testIdle = 7 * 24 * time.Hour
	testAbs  = 30 * 24 * time.Hour
)

func newService(t *testing.T) (*session.Service, *gorm.DB, *testsupport.Clock) {
	t.Helper()
	db := testsupport.OpenDB(t)
	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	svc := session.New(db, session.Config{
		IdleTTL:          testIdle,
		AbsoluteTTL:      testAbs,
		AdminIdleTTL:     30 * time.Minute,
		AdminAbsoluteTTL: 8 * time.Hour,
		Now:              clock.Now,
	})
	return svc, db, clock
}

func seedUser(t *testing.T, db *gorm.DB) *domain.User {
	t.Helper()
	uid := "alice"
	return testsupport.CreateUser(t, db, "alice@example.com", domain.UserStatusActive, &uid)
}

func TestIssueAndVerify(t *testing.T) {
	svc, db, _ := newService(t)
	user := seedUser(t, db)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, session.IssueParams{
		UserID: user.ID, UID: "alice", Scope: domain.ScopeUser, IP: "1.2.3.4", UAHash: "ua",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issued.Plain == "" {
		t.Fatal("empty plain token")
	}

	got, err := svc.Verify(ctx, issued.Plain)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.UID != "alice" || got.Scope != domain.ScopeUser || got.UserID != user.ID {
		t.Fatalf("unexpected session: %+v", got)
	}
}

// 不变量 I3：数据库里绝不能出现明文 token
func TestTokenIsNotStoredInPlaintext(t *testing.T) {
	svc, db, _ := newService(t)
	user := seedUser(t, db)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	var stored []byte
	if err := db.Raw(`SELECT token_hash FROM sessions LIMIT 1`).Row().Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(stored, []byte(issued.Plain)) {
		t.Fatal("token stored in plaintext")
	}
	if !bytes.Equal(stored, secure.HashToken(issued.Plain)) {
		t.Fatal("stored hash does not match sha256(plain)")
	}

	var count int64
	if err := db.Raw(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, []byte(issued.Plain)).
		Row().Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("plain token must never be a stored key")
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	svc, db, _ := newService(t)
	seedUser(t, db)
	ctx := context.Background()

	for _, token := range []string{"", "not-a-token", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := svc.Verify(ctx, token); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("Verify(%q) = %v, want ErrInvalid", token, err)
		}
	}
}

func TestRevokeSingleSession(t *testing.T) {
	svc, db, _ := newService(t)
	user := seedUser(t, db)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, issued.Plain, "logout"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := svc.Verify(ctx, issued.Plain); !errors.Is(err, session.ErrRevoked) {
		t.Fatalf("Verify after revoke = %v, want ErrRevoked", err)
	}
	// 幂等
	if err := svc.Revoke(ctx, issued.Plain, "logout"); err != nil {
		t.Fatalf("Revoke twice should be idempotent: %v", err)
	}
}

func TestRevokeAllForUser(t *testing.T) {
	svc, db, _ := newService(t)
	user := seedUser(t, db)
	other := testsupport.CreateUser(t, db, "bob@example.com", domain.UserStatusActive, strPtr("bob"))
	ctx := context.Background()

	var mine []string
	for i := 0; i < 3; i++ {
		issued, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		mine = append(mine, issued.Plain)
	}
	keep, err := svc.Issue(ctx, session.IssueParams{UserID: other.ID, UID: "bob"})
	if err != nil {
		t.Fatal(err)
	}

	n, err := svc.RevokeAllForUser(ctx, user.ID, "email_changed")
	if err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}
	if n != 3 {
		t.Fatalf("revoked %d sessions, want 3", n)
	}
	for _, tok := range mine {
		if _, err := svc.Verify(ctx, tok); !errors.Is(err, session.ErrRevoked) {
			t.Errorf("session %s should be revoked, got %v", tok[:8], err)
		}
	}
	// 其他用户的会话不受影响
	if _, err := svc.Verify(ctx, keep.Plain); err != nil {
		t.Fatalf("unrelated session must survive: %v", err)
	}
}

func TestIdleExpiry(t *testing.T) {
	svc, db, clock := newService(t)
	user := seedUser(t, db)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(testIdle - time.Hour) // 仍有 1 小时，但已进入续期窗口
	if _, err := svc.Verify(ctx, issued.Plain); err != nil {
		t.Fatalf("session should still be valid: %v", err)
	}
	// 已被续期
	clock.Advance(testIdle - time.Hour)
	if _, err := svc.Verify(ctx, issued.Plain); err != nil {
		t.Fatalf("session should have been renewed: %v", err)
	}

	// 无人使用 → 空闲过期
	clock.Advance(testIdle + time.Hour)
	if _, err := svc.Verify(ctx, issued.Plain); !errors.Is(err, session.ErrExpired) {
		t.Fatalf("Verify = %v, want ErrExpired", err)
	}
}

// 绝对值不可被续期突破（§5.4）
func TestRenewalNeverExceedsAbsoluteExpiry(t *testing.T) {
	db := testsupport.OpenDB(t)
	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	svc := session.New(db, session.Config{
		IdleTTL:     7 * 24 * time.Hour,
		AbsoluteTTL: 8 * 24 * time.Hour, // 只比 idle 长 1 天
		Now:         clock.Now,
	})
	user := seedUser(t, db)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	// 第 6 天：进入续期窗口，续期但被绝对值截断
	clock.Advance(6 * 24 * time.Hour)
	if _, err := svc.Verify(ctx, issued.Plain); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	var idle, abs time.Time
	if err := db.Raw(`SELECT idle_expires_at, absolute_expires_at FROM sessions LIMIT 1`).
		Row().Scan(&idle, &abs); err != nil {
		t.Fatal(err)
	}
	if idle.After(abs) {
		t.Fatalf("idle (%v) must not exceed absolute (%v)", idle, abs)
	}
	if !idle.Equal(abs) {
		t.Fatalf("idle should be clamped to absolute, idle=%v abs=%v", idle, abs)
	}

	// 越过绝对值 → 过期
	clock.Advance(2 * 24 * time.Hour)
	if _, err := svc.Verify(ctx, issued.Plain); !errors.Is(err, session.ErrExpired) {
		t.Fatalf("Verify past absolute = %v, want ErrExpired", err)
	}
}

// 续期只在窗口内发生，避免每个请求都写库
func TestRenewalOnlyInsideWindow(t *testing.T) {
	svc, db, clock := newService(t)
	user := seedUser(t, db)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	var before, after time.Time
	if err := db.Raw(`SELECT idle_expires_at FROM sessions LIMIT 1`).Row().Scan(&before); err != nil {
		t.Fatal(err)
	}

	clock.Advance(1 * time.Hour) // 远未进入窗口
	if _, err := svc.Verify(ctx, issued.Plain); err != nil {
		t.Fatal(err)
	}
	if err := db.Raw(`SELECT idle_expires_at FROM sessions LIMIT 1`).Row().Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !before.Equal(after) {
		t.Fatalf("idle expiry should not change outside renew window: %v → %v", before, after)
	}
}

func TestAdminScopeUsesShorterTTL(t *testing.T) {
	svc, db, clock := newService(t)
	admin := testsupport.CreateUser(t, db, "ops@example.com", domain.UserStatusActive, strPtr("ops"))
	ctx := context.Background()

	issued, err := svc.Issue(ctx, session.IssueParams{
		UserID: admin.ID, UID: "ops", Scope: domain.ScopeAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}
	var idle, abs time.Time
	if err := db.Raw(`SELECT idle_expires_at, absolute_expires_at FROM sessions LIMIT 1`).
		Row().Scan(&idle, &abs); err != nil {
		t.Fatal(err)
	}
	if d := idle.Sub(clock.Now()); d != 30*time.Minute {
		t.Fatalf("admin idle TTL = %v, want 30m", d)
	}
	if d := abs.Sub(clock.Now()); d != 8*time.Hour {
		t.Fatalf("admin absolute TTL = %v, want 8h", d)
	}

	clock.Advance(31 * time.Minute)
	if _, err := svc.Verify(ctx, issued.Plain); !errors.Is(err, session.ErrExpired) {
		t.Fatalf("admin session should expire after 30m idle, got %v", err)
	}
}

func TestListActiveDevicesAndDeleteExpired(t *testing.T) {
	svc, db, clock := newService(t)
	user := seedUser(t, db)
	ctx := context.Background()

	keep, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	_ = old

	// 没有设备信息的两条会话必须归并成**一台**未知设备，而不是两台
	// （验证码登录每次都签发新会话，直接按会话数显示就会把一台手机说成十台）。
	devices, err := svc.ListActiveDevices(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListActiveDevices: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("ListActiveDevices = %d devices, want 1", len(devices))
	}
	if devices[0].Sessions != 2 {
		t.Fatalf("device sessions = %d, want 2", devices[0].Sessions)
	}

	// 越过绝对有效期后清理
	clock.Advance(testAbs + time.Hour)
	n, err := svc.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 2 {
		t.Fatalf("DeleteExpired removed %d, want 2", n)
	}
	if _, err := svc.Verify(ctx, keep.Plain); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("deleted session should be invalid, got %v", err)
	}
}

func TestIssueRequiresUserID(t *testing.T) {
	svc, _, _ := newService(t)
	if _, err := svc.Issue(context.Background(), session.IssueParams{}); err == nil {
		t.Fatal("expected error for missing UserID")
	}
}

func strPtr(s string) *string { return &s }

// 续期阈值必须按 scope 推导。
//
// 回归背景：阈值一度全局取 IdleTTL/2（3.5 天），而管理会话 idle 只有 30 分钟，
// 于是"剩余不足 3.5 天"永远成立 ⇒ 管理会话**每次校验都写一次库**。
func TestRenewWindowIsPerScope(t *testing.T) {
	db := testsupport.OpenDB(t)
	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	svc := session.New(db, session.Config{Now: clock.Now})
	ctx := context.Background()

	admin := testsupport.CreateUser(t, db, "ops@example.com", domain.UserStatusActive, strPtr("ops"))
	user := testsupport.CreateUser(t, db, "alice@example.com", domain.UserStatusActive, strPtr("alice"))

	adminSess, err := svc.Issue(ctx, session.IssueParams{UserID: admin.ID, UID: "ops", Scope: domain.ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	userSess, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice", Scope: domain.ScopeUser})
	if err != nil {
		t.Fatal(err)
	}

	lastSeen := func(tokenHash []byte) time.Time {
		var ts time.Time
		if err := db.Raw(`SELECT last_seen_at FROM sessions WHERE token_hash = ?`, tokenHash).
			Row().Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	adminHash := secure.HashToken(adminSess.Plain)
	userHash := secure.HashToken(userSess.Plain)

	// 管理会话：剩余 29 分钟（阈值 15 分钟）→ 不该写库
	clock.Advance(time.Minute)
	before := lastSeen(adminHash)
	if _, err := svc.Verify(ctx, adminSess.Plain); err != nil {
		t.Fatal(err)
	}
	if got := lastSeen(adminHash); !got.Equal(before) {
		t.Errorf("管理会话剩余 29 分钟时不应续期（阈值 15 分钟），实际写库了")
	}

	// 管理会话：推进到剩余 10 分钟 → 该续期，且 idle 被推到 now+30m
	clock.Advance(19 * time.Minute)
	if _, err := svc.Verify(ctx, adminSess.Plain); err != nil {
		t.Fatal(err)
	}
	if got := lastSeen(adminHash); got.Equal(before) {
		t.Errorf("管理会话剩余 10 分钟时应续期，实际没写库")
	}

	// 用户会话：剩余 6 天（阈值 3.5 天）→ 不该写库
	userBefore := lastSeen(userHash)
	clock.Advance(24 * time.Hour)
	if _, err := svc.Verify(ctx, userSess.Plain); err != nil {
		t.Fatal(err)
	}
	if got := lastSeen(userHash); !got.Equal(userBefore) {
		t.Errorf("用户会话剩余 6 天时不应续期（阈值 3.5 天）")
	}

	// 用户会话：推进到剩余 3 天 → 该续期
	clock.Advance(3 * 24 * time.Hour)
	if _, err := svc.Verify(ctx, userSess.Plain); err != nil {
		t.Fatal(err)
	}
	if got := lastSeen(userHash); got.Equal(userBefore) {
		t.Errorf("用户会话剩余 3 天时应续期")
	}
}

// 自定义阈值同样按 scope 生效。
func TestCustomRenewWindows(t *testing.T) {
	db := testsupport.OpenDB(t)
	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	svc := session.New(db, session.Config{
		IdleTTL:          24 * time.Hour,
		AbsoluteTTL:      48 * time.Hour,
		RenewWindow:      2 * time.Hour,
		AdminIdleTTL:     2 * time.Hour,
		AdminAbsoluteTTL: 8 * time.Hour,
		AdminRenewWindow: 30 * time.Minute,
		Now:              clock.Now,
	})
	user := testsupport.CreateUser(t, db, "alice@example.com", domain.UserStatusActive, strPtr("alice"))
	ctx := context.Background()

	sess, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	// 剩余 3 小时 > 2 小时阈值 → 不续期
	clock.Advance(21 * time.Hour)
	var before time.Time
	if err := db.Raw(`SELECT last_seen_at FROM sessions LIMIT 1`).Row().Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(ctx, sess.Plain); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := db.Raw(`SELECT last_seen_at FROM sessions LIMIT 1`).Row().Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Error("剩余 3 小时 > 阈值 2 小时，不应续期")
	}
}

// 设备的聚合规则：同一设备重复登录算一台，不同设备/不同 scope 各算一台。
func TestListActiveDevicesGroupsByFingerprint(t *testing.T) {
	svc, db, clock := newService(t)
	user := seedUser(t, db)
	ctx := context.Background()

	phone := session.DeviceInfo{ID: "fp-phone", Type: "mobile", Model: "iPhone", OS: "iOS 17.5", Browser: "Safari 17.5"}
	laptop := session.DeviceInfo{ID: "fp-laptop", Type: "desktop", Model: "Mac", OS: "macOS", Browser: "Chrome 126"}

	issue := func(scope, ip string, dev session.DeviceInfo) {
		t.Helper()
		if _, err := svc.Issue(ctx, session.IssueParams{
			UserID: user.ID, UID: "alice", Scope: scope, IP: ip, Device: dev,
		}); err != nil {
			t.Fatalf("Issue: %v", err)
		}
		// 让下次签发的 issued_at 严格变晚，聚合里的"最近登入"才有区分度
		clock.Advance(time.Minute)
	}

	issue(domain.ScopeUser, "1.1.1.1", phone)
	issue(domain.ScopeUser, "1.1.1.2", phone) // 同一台手机换了网络、又登录了一次
	issue(domain.ScopeUser, "2.2.2.2", laptop)
	issue(domain.ScopeAdmin, "1.1.1.1", phone) // 同一台手机上的管理会话：另一个 scope

	devices, err := svc.ListActiveDevices(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListActiveDevices: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("devices = %d, want 3（手机 1 台 + 电脑 1 台 + 管理会话 1 台）", len(devices))
	}

	// 最新登入的排在最前（管理会话是最后签发的）
	if devices[0].Scope != domain.ScopeAdmin {
		t.Fatalf("first device scope = %q, want admin", devices[0].Scope)
	}

	var phoneDevice *session.ActiveDevice
	for i := range devices {
		if devices[i].DeviceID == "fp-phone" && devices[i].Scope == domain.ScopeUser {
			phoneDevice = &devices[i]
		}
	}
	if phoneDevice == nil {
		t.Fatal("找不到那台手机")
	}
	if phoneDevice.Sessions != 2 {
		t.Errorf("手机上的会话数 = %d, want 2", phoneDevice.Sessions)
	}
	// IP 取最近一次会话的地址，而不是第一次的
	if phoneDevice.IP != "1.1.1.2" {
		t.Errorf("IP = %q, want 1.1.1.2（最近一次会话）", phoneDevice.IP)
	}
	if phoneDevice.Model != "iPhone" || phoneDevice.OS != "iOS 17.5" {
		t.Errorf("展示字段丢失：%+v", phoneDevice)
	}
	if !phoneDevice.FirstLoginAt.Before(phoneDevice.LoginAt) {
		t.Errorf("FirstLoginAt (%v) 应早于 LoginAt (%v)", phoneDevice.FirstLoginAt, phoneDevice.LoginAt)
	}

	// 列表与计数必须一致：界面上的徽标与展开后的行数不能对不上
	counts, err := svc.CountActiveDevicesByUsers(ctx, []int64{user.ID})
	if err != nil {
		t.Fatalf("CountActiveDevicesByUsers: %v", err)
	}
	if counts[user.ID] != len(devices) {
		t.Fatalf("counts = %d, devices = %d", counts[user.ID], len(devices))
	}
}

// "在线"必须排除已过期与已吊销的会话：会话是按需清理的，库里可能躺着
// 早就闲置过期的行。
func TestListActiveDevicesExcludesInactive(t *testing.T) {
	svc, db, clock := newService(t)
	user := seedUser(t, db)
	ctx := context.Background()

	dev := session.DeviceInfo{ID: "fp-phone", Type: "mobile", Model: "iPhone"}
	if _, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice", Device: dev}); err != nil {
		t.Fatal(err)
	}
	revoked, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice", Device: session.DeviceInfo{ID: "fp-old"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, revoked.Plain, "test"); err != nil {
		t.Fatal(err)
	}

	// 第一条只越过 idle 有效期（绝对有效期尚未到）：它必须被视为"不在线"
	clock.Advance(testIdle + time.Hour)

	live, err := svc.Issue(ctx, session.IssueParams{UserID: user.ID, UID: "alice", Device: dev})
	if err != nil {
		t.Fatal(err)
	}

	var raw int
	if err := db.Raw(`SELECT COUNT(*) FROM sessions`).Row().Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 3 {
		t.Fatalf("库里的会话数 = %d, want 3（过期/已吊销的行仍在，只是不算在线）", raw)
	}

	devices, err := svc.ListActiveDevices(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("在线设备 = %d, want 1（只有最后一条还活着）", len(devices))
	}
	if devices[0].Sessions != 1 {
		t.Errorf("会话数 = %d, want 1", devices[0].Sessions)
	}
	if _, err := svc.Verify(ctx, live.Plain); err != nil {
		t.Errorf("最后一条会话必须仍然有效：%v", err)
	}
}

func TestCountActiveDevicesByUsersEmptyInput(t *testing.T) {
	svc, _, _ := newService(t)
	counts, err := svc.CountActiveDevicesByUsers(context.Background(), nil)
	if err != nil {
		t.Fatalf("空列表不该报错：%v", err)
	}
	if len(counts) != 0 {
		t.Fatalf("counts = %v, want 空", counts)
	}
}
