package otp_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/otp"
	"github.com/tangthinker/user-center/v2/internal/testsupport"
	"gorm.io/gorm"
)

const testEmail = "alice@example.com"

func newService(t *testing.T) (*otp.Service, *gorm.DB, *testsupport.Clock) {
	t.Helper()
	db := testsupport.OpenDB(t)
	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	svc, err := otp.New(db, otp.Config{
		CodeLength:       6,
		TTL:              10 * time.Minute,
		AdminTTL:         5 * time.Minute,
		MaxAttempts:      5,
		AdminMaxAttempts: 3,
		Cooldown:         60 * time.Second,
		HMACKey:          []byte("unit-test-key"),
		Now:              clock.Now,
	})
	if err != nil {
		t.Fatalf("otp.New: %v", err)
	}
	return svc, db, clock
}

func TestNewRequiresHMACKey(t *testing.T) {
	db := testsupport.OpenDB(t)
	if _, err := otp.New(db, otp.Config{}); err == nil {
		t.Fatal("otp.New must refuse an empty HMACKey (绝不使用默认弱密钥)")
	}
}

func TestIssueAndVerifyOnce(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin, IP: "1.2.3.4"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issued.Reused || len(issued.Code) != 6 {
		t.Fatalf("unexpected issue result: reuse=%v code=%q", issued.Reused, issued.Code)
	}

	if err := svc.Verify(ctx, testEmail, domain.PurposeLogin, issued.Code); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// 单次使用
	if err := svc.Verify(ctx, testEmail, domain.PurposeLogin, issued.Code); !errors.Is(err, otp.ErrInvalid) {
		t.Fatalf("second Verify = %v, want ErrInvalid (单次使用)", err)
	}
}

// 明文码不得落库
func TestCodeIsNotStoredInPlaintext(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin})
	if err != nil {
		t.Fatal(err)
	}

	var count int64
	if err := db.Raw(`SELECT COUNT(*) FROM otp_codes WHERE CAST(code_hmac AS TEXT) = ?`, issued.Code).
		Row().Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("plaintext code found in database")
	}
}

// 冷却期复用：不生成新码、不发新邮件，且原码仍有效（省配额 + 防骚扰）
func TestCooldownReusesExistingCode(t *testing.T) {
	svc, _, clock := newService(t)
	ctx := context.Background()

	first, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin})
	if err != nil {
		t.Fatal(err)
	}

	// 冷却窗口内重复请求
	clock.Advance(30 * time.Second)
	second, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused {
		t.Fatal("expected Reused=true inside cooldown")
	}
	if second.Code != "" {
		t.Fatalf("reused issue must not return a code, got %q", second.Code)
	}
	if !second.OTP.IssuedAt.Equal(first.OTP.IssuedAt) {
		t.Fatal("reused issue must return the same record")
	}
	// 原码仍然有效
	if err := svc.Verify(ctx, testEmail, domain.PurposeLogin, first.Code); err != nil {
		t.Fatalf("original code should still work: %v", err)
	}
}

// 冷却期后签发新码时，旧码必须立即失效
func TestNewCodeInvalidatesPrevious(t *testing.T) {
	svc, db, clock := newService(t)
	ctx := context.Background()

	first, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute) // 越过冷却

	second, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin})
	if err != nil {
		t.Fatal(err)
	}
	if second.Reused || second.Code == "" {
		t.Fatalf("expected a fresh code, got reuse=%v", second.Reused)
	}

	// 数据库中恰好一个未消费的码，且不是第一个
	var active int64
	if err := db.Raw(`SELECT COUNT(*) FROM otp_codes WHERE consumed_at IS NULL`).Row().Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active codes = %d, want exactly 1（只有最新码有效）", active)
	}
	var oldConsumed int64
	if err := db.Raw(
		`SELECT COUNT(*) FROM otp_codes WHERE issued_at = ? AND consumed_at IS NOT NULL`,
		first.OTP.IssuedAt,
	).Row().Scan(&oldConsumed); err != nil {
		t.Fatal(err)
	}
	if oldConsumed != 1 {
		t.Fatal("previous code must be marked consumed")
	}

	if err := svc.Verify(ctx, testEmail, domain.PurposeLogin, second.Code); err != nil {
		t.Fatalf("new code should work: %v", err)
	}
}

func TestVerifyRejectsWrongCodeAndExpiresAfterMaxAttempts(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin})
	if err != nil {
		t.Fatal(err)
	}

	wrong := "000000"
	if issued.Code == wrong {
		wrong = "111111"
	}
	for i := 0; i < 5; i++ {
		if err := svc.Verify(ctx, testEmail, domain.PurposeLogin, wrong); !errors.Is(err, otp.ErrInvalid) {
			t.Fatalf("attempt %d = %v, want ErrInvalid", i+1, err)
		}
	}

	// 达到上限后该码被作废：即使输入正确也不再通过
	if err := svc.Verify(ctx, testEmail, domain.PurposeLogin, issued.Code); !errors.Is(err, otp.ErrInvalid) {
		t.Fatalf("code should be invalidated after max attempts, got %v", err)
	}

	var consumedAt *time.Time
	if err := db.Raw(`SELECT consumed_at FROM otp_codes ORDER BY id DESC LIMIT 1`).Row().Scan(&consumedAt); err != nil {
		t.Fatal(err)
	}
	if consumedAt == nil {
		t.Fatal("code should be marked consumed after max attempts")
	}
}

// 并发试码不能突破次数上限（设计验收 #13）
func TestConcurrentWrongAttemptsCannotExceedLimit(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin})
	if err != nil {
		t.Fatal(err)
	}
	wrong := "000000"
	if issued.Code == wrong {
		wrong = "111111"
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = svc.Verify(ctx, testEmail, domain.PurposeLogin, wrong)
		}()
	}
	wg.Wait()

	var attempts int
	var consumedAt *time.Time
	if err := db.Raw(`SELECT attempts, consumed_at FROM otp_codes ORDER BY id DESC LIMIT 1`).
		Row().Scan(&attempts, &consumedAt); err != nil {
		t.Fatal(err)
	}
	if attempts > 5 {
		t.Fatalf("attempts = %d, must never exceed 5", attempts)
	}
	if consumedAt == nil {
		t.Fatal("code must be consumed once the limit is reached")
	}
}

// 用途隔离：login 的码不能用于 admin_login（否则普通用户可越权换管理员会话）
func TestPurposeIsolation(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Verify(ctx, testEmail, domain.PurposeAdminLogin, issued.Code); !errors.Is(err, otp.ErrInvalid) {
		t.Fatalf("login code must not verify as admin_login, got %v", err)
	}
	// 原码在正确用途下仍然可用（校验失败不应消耗它之外的用途）
	if err := svc.Verify(ctx, testEmail, domain.PurposeLogin, issued.Code); err != nil {
		t.Fatalf("code should still be valid for its own purpose: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	svc, _, clock := newService(t)
	ctx := context.Background()

	login, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeAdminLogin})
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(6 * time.Minute) // 超过 admin TTL(5m)，未到 login TTL(10m)

	if err := svc.Verify(ctx, testEmail, domain.PurposeAdminLogin, admin.Code); !errors.Is(err, otp.ErrInvalid) {
		t.Fatalf("admin code should be expired, got %v", err)
	}
	if err := svc.Verify(ctx, testEmail, domain.PurposeLogin, login.Code); err != nil {
		t.Fatalf("login code should still be valid: %v", err)
	}

	clock.Advance(5 * time.Minute)
	if err := svc.Verify(ctx, testEmail, domain.PurposeLogin, login.Code); !errors.Is(err, otp.ErrInvalid) {
		t.Fatalf("login code should be expired, got %v", err)
	}
}

func TestAdminUsesTighterAttemptLimit(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	issued, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeAdminLogin})
	if err != nil {
		t.Fatal(err)
	}
	wrong := "000000"
	if issued.Code == wrong {
		wrong = "111111"
	}
	for i := 0; i < 3; i++ {
		_ = svc.Verify(ctx, testEmail, domain.PurposeAdminLogin, wrong)
	}
	if err := svc.Verify(ctx, testEmail, domain.PurposeAdminLogin, issued.Code); !errors.Is(err, otp.ErrInvalid) {
		t.Fatal("admin code should be invalid after 3 failed attempts")
	}
}

func TestDeleteExpired(t *testing.T) {
	svc, db, clock := newService(t)
	ctx := context.Background()

	if _, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail, Purpose: domain.PurposeLogin}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(3 * time.Hour)

	n, err := svc.DeleteExpired(ctx)
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d rows, want 1", n)
	}
	var left int64
	if err := db.Raw(`SELECT COUNT(*) FROM otp_codes`).Row().Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("otp_codes not cleaned, left=%d", left)
	}
}

func TestIssueRequiresEmailAndPurpose(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if _, err := svc.Issue(ctx, otp.IssueParams{Purpose: domain.PurposeLogin}); err == nil {
		t.Fatal("expected error for empty email")
	}
	if _, err := svc.Issue(ctx, otp.IssueParams{Email: testEmail}); err == nil {
		t.Fatal("expected error for empty purpose")
	}
}
