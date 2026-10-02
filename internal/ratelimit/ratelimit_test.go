package ratelimit_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/ratelimit"
	"github.com/tangthinker/user-center/v2/internal/testsupport"
	"gorm.io/gorm"
)

func newService(t *testing.T, cfg ratelimit.Config) (*ratelimit.Service, *gorm.DB, *testsupport.Clock) {
	t.Helper()
	db := testsupport.OpenDB(t)
	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg.Now = clock.Now
	return ratelimit.New(db, cfg), db, clock
}

// unlimited 返回除指定维度外全部放开的配置。
//
// 注意：零值现在表示"取默认限额"（安全默认），关闭某维度需要显式传**负数**。
func unlimited() ratelimit.Config {
	return ratelimit.Config{
		EmailPerHour: -1, EmailPerDay: -1,
		IPPerHour: -1, IPPerDay: -1,
		VerifyPerIPPerHour: -1,
		GlobalDailyCap:     -1,
		AdminInvitePerHour: -1,
		AdminAPIPerMinute:  -1,
	}
}

func send(t *testing.T, svc *ratelimit.Service, email, ip string) *ratelimit.Decision {
	t.Helper()
	d, err := svc.AllowSend(context.Background(), ratelimit.SendParams{
		Email: email, Purpose: domain.PurposeLogin, IP: ip,
	})
	if err != nil {
		t.Fatalf("AllowSend: %v", err)
	}
	return d
}

func TestEmailHourlyLimit(t *testing.T) {
	cfg := unlimited()
	cfg.EmailPerHour = 5
	svc, _, _ := newService(t, cfg)

	for i := 1; i <= 5; i++ {
		if d := send(t, svc, "a@example.com", "1.1.1.1"); !d.Allowed {
			t.Fatalf("request #%d denied: %s", i, d.Reason)
		}
	}
	d := send(t, svc, "a@example.com", "1.1.1.1")
	if d.Allowed {
		t.Fatal("6th request should be denied")
	}
	if d.Reason != "email_hour" {
		t.Fatalf("reason = %q, want email_hour", d.Reason)
	}
}

func TestEmailDailyLimit(t *testing.T) {
	cfg := unlimited()
	cfg.EmailPerDay = 3
	svc, _, clock := newService(t, cfg)

	// 跨过小时窗口，确认日额仍在生效
	for i := 1; i <= 3; i++ {
		if d := send(t, svc, "a@example.com", "1.1.1.1"); !d.Allowed {
			t.Fatalf("request #%d denied: %s", i, d.Reason)
		}
		clock.Advance(61 * time.Minute)
	}
	d := send(t, svc, "a@example.com", "1.1.1.1")
	if d.Allowed || d.Reason != "email_day" {
		t.Fatalf("daily limit not enforced: allowed=%v reason=%q", d.Allowed, d.Reason)
	}
}

// 全局熔断跨邮箱共享——这是配额被刷光的最后一道保险
func TestGlobalDailyCapIsSharedAcrossEmails(t *testing.T) {
	cfg := unlimited()
	cfg.GlobalDailyCap = 3
	svc, _, _ := newService(t, cfg)

	for i, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if d := send(t, svc, email, "1.1.1.1"); !d.Allowed {
			t.Fatalf("request #%d denied: %s", i+1, d.Reason)
		}
	}
	d := send(t, svc, "d@example.com", "1.1.1.1")
	if d.Allowed || d.Reason != "global_day" {
		t.Fatalf("global cap not enforced: allowed=%v reason=%q", d.Allowed, d.Reason)
	}
}

func TestIPHourlyLimit(t *testing.T) {
	cfg := unlimited()
	cfg.IPPerHour = 2
	svc, _, _ := newService(t, cfg)

	if d := send(t, svc, "a@example.com", "9.9.9.9"); !d.Allowed {
		t.Fatal("first should pass")
	}
	if d := send(t, svc, "b@example.com", "9.9.9.9"); !d.Allowed {
		t.Fatal("second should pass")
	}
	d := send(t, svc, "c@example.com", "9.9.9.9")
	if d.Allowed || d.Reason != "ip_hour" {
		t.Fatalf("IP limit not enforced: allowed=%v reason=%q", d.Allowed, d.Reason)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Hour {
		t.Fatalf("RetryAfter = %v, want (0, 1h]", d.RetryAfter)
	}

	// 另一个 IP 不受影响
	if d := send(t, svc, "d@example.com", "8.8.8.8"); !d.Allowed {
		t.Fatal("a different IP must not be affected")
	}
}

// 被拒绝的请求要留痕（accepted=0），但不占用配额
func TestDeniedRequestsAreRecordedButDoNotConsumeQuota(t *testing.T) {
	cfg := unlimited()
	cfg.EmailPerHour = 1
	svc, db, clock := newService(t, cfg)

	if d := send(t, svc, "a@example.com", "1.1.1.1"); !d.Allowed {
		t.Fatal("first should pass")
	}
	if d := send(t, svc, "a@example.com", "1.1.1.1"); d.Allowed {
		t.Fatal("second should be denied")
	}

	var accepted, denied int64
	if err := db.Raw(`SELECT COUNT(*) FROM mail_log WHERE accepted = 1`).Row().Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if err := db.Raw(`SELECT COUNT(*) FROM mail_log WHERE accepted = 0`).Row().Scan(&denied); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || denied != 1 {
		t.Fatalf("accepted=%d denied=%d, want 1/1", accepted, denied)
	}

	// 窗口过去后恢复
	clock.Advance(time.Hour + time.Second)
	if d := send(t, svc, "a@example.com", "1.1.1.1"); !d.Allowed {
		t.Fatalf("should recover after the window: %s", d.Reason)
	}
}

// 负数 = 显式关闭限流
func TestNegativeLimitsMeanUnlimited(t *testing.T) {
	svc, _, _ := newService(t, unlimited())
	for i := 0; i < 50; i++ {
		if d := send(t, svc, "a@example.com", "1.1.1.1"); !d.Allowed {
			t.Fatalf("request #%d denied with reason %q; negative limits must mean unlimited", i+1, d.Reason)
		}
	}
}

// 零值必须落到**安全默认**，而不是"完全不限流"
func TestZeroConfigFallsBackToSafeDefaults(t *testing.T) {
	svc, _, _ := newService(t, ratelimit.Config{})

	allowed := 0
	for i := 0; i < 10; i++ {
		if d := send(t, svc, "a@example.com", "1.1.1.1"); d.Allowed {
			allowed++
		}
	}
	if allowed != ratelimit.DefaultConfig().EmailPerHour {
		t.Fatalf("allowed = %d, want %d（零值应取默认限额，而不是不限流）",
			allowed, ratelimit.DefaultConfig().EmailPerHour)
	}

	// IP 维度同样生效（默认 10/小时）
	ipAllowed := 0
	for i := 0; i < 20; i++ {
		if d := send(t, svc, fmt.Sprintf("u%d@example.com", i), "9.9.9.9"); d.Allowed {
			ipAllowed++
		}
	}
	if ipAllowed != ratelimit.DefaultConfig().IPPerHour {
		t.Fatalf("per-IP allowed = %d, want %d", ipAllowed, ratelimit.DefaultConfig().IPPerHour)
	}
}

func TestVerifyByIPLimit(t *testing.T) {
	cfg := unlimited()
	cfg.VerifyPerIPPerHour = 2
	svc, _, clock := newService(t, cfg)

	if d := svc.AllowVerifyByIP("1.2.3.4"); !d.Allowed {
		t.Fatal("first verify should pass")
	}
	if d := svc.AllowVerifyByIP("1.2.3.4"); !d.Allowed {
		t.Fatal("second verify should pass")
	}
	d := svc.AllowVerifyByIP("1.2.3.4")
	if d.Allowed || d.Reason != "verify_ip_hour" {
		t.Fatalf("verify limit not enforced: %+v", d)
	}

	clock.Advance(time.Hour + time.Second)
	if d := svc.AllowVerifyByIP("1.2.3.4"); !d.Allowed {
		t.Fatal("should recover after window")
	}
}

func TestAdminInviteLimit(t *testing.T) {
	cfg := unlimited()
	cfg.AdminInvitePerHour = 1
	svc, _, _ := newService(t, cfg)

	if d := svc.AllowAdminInvite(7); !d.Allowed {
		t.Fatal("first invite should pass")
	}
	if d := svc.AllowAdminInvite(7); d.Allowed || d.Reason != "admin_invite_hour" {
		t.Fatalf("admin invite limit not enforced: %+v", d)
	}
	// 另一个管理员不受影响
	if d := svc.AllowAdminInvite(8); !d.Allowed {
		t.Fatal("another admin must not be affected")
	}
}

func TestAdminAPILimit(t *testing.T) {
	cfg := unlimited()
	cfg.AdminAPIPerMinute = 2
	svc, _, clock := newService(t, cfg)

	for i := 0; i < 2; i++ {
		if d := svc.AllowAdminAPI("127.0.0.1"); !d.Allowed {
			t.Fatalf("request #%d should pass", i+1)
		}
	}
	if d := svc.AllowAdminAPI("127.0.0.1"); d.Allowed || d.Reason != "admin_api_minute" {
		t.Fatalf("admin API limit not enforced: %+v", d)
	}
	clock.Advance(time.Minute + time.Second)
	if d := svc.AllowAdminAPI("127.0.0.1"); !d.Allowed {
		t.Fatal("should recover after window")
	}
}

func TestSentInLast24h(t *testing.T) {
	cfg := unlimited()
	svc, _, clock := newService(t, cfg)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		send(t, svc, "a@example.com", "1.1.1.1")
	}
	n, err := svc.SentInLast24h(ctx)
	if err != nil {
		t.Fatalf("SentInLast24h: %v", err)
	}
	if n != 4 {
		t.Fatalf("SentInLast24h = %d, want 4", n)
	}

	clock.Advance(25 * time.Hour)
	n, err = svc.SentInLast24h(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("SentInLast24h after 25h = %d, want 0（滚动窗口）", n)
	}
}

func TestCleanupMailLog(t *testing.T) {
	svc, db, clock := newService(t, unlimited())
	send(t, svc, "a@example.com", "1.1.1.1")

	clock.Advance(200 * 24 * time.Hour)
	n, err := svc.CleanupMailLog(context.Background(), 180*24*time.Hour)
	if err != nil {
		t.Fatalf("CleanupMailLog: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d rows, want 1", n)
	}
	var left int64
	if err := db.Raw(`SELECT COUNT(*) FROM mail_log`).Row().Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("mail_log not cleaned, left=%d", left)
	}
}
