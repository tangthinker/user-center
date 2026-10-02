package invite_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/invite"
	"github.com/tangthinker/user-center/v2/internal/secure"
	"github.com/tangthinker/user-center/v2/internal/testsupport"
	"gorm.io/gorm"
)

const (
	testTTL        = 7 * 24 * time.Hour
	testCheckLimit = 20
)

func newService(t *testing.T) (*invite.Service, *gorm.DB, *testsupport.Clock) {
	t.Helper()
	db := testsupport.OpenDB(t)
	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	svc := invite.New(db, invite.Config{
		TTL:        testTTL,
		CheckLimit: testCheckLimit,
		Now:        clock.Now,
	})
	return svc, db, clock
}

// seedInvited 建一个 invited 用户并签发邀请，返回用户与明文 token。
func seedInvited(t *testing.T, svc *invite.Service, db *gorm.DB, email string) (*domain.User, string) {
	t.Helper()
	user := testsupport.CreateUser(t, db, email, domain.UserStatusInvited, nil)
	plain, _, err := svc.Create(context.Background(), invite.CreateParams{
		UserID: user.ID, Email: email, Purpose: domain.PurposeInvite, CreatedBy: 1,
	})
	if err != nil {
		t.Fatalf("invite.Create: %v", err)
	}
	return user, plain
}

// 明文 token 不得落库
func TestCreateStoresHashOnly(t *testing.T) {
	svc, db, _ := newService(t)
	_, plain := seedInvited(t, svc, db, "alice@example.com")

	var stored []byte
	if err := db.Raw(`SELECT token_hash FROM invitations LIMIT 1`).Row().Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(stored, []byte(plain)) {
		t.Fatal("invitation token stored in plaintext")
	}
	if !bytes.Equal(stored, secure.HashToken(plain)) {
		t.Fatal("stored value is not sha256(token)")
	}
}

// 验收 #1：GET 落地页预取不消费 token
func TestLookupDoesNotConsume(t *testing.T) {
	svc, db, _ := newService(t)
	user, plain := seedInvited(t, svc, db, "alice@example.com")
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		inv, got, err := svc.Lookup(ctx, plain)
		if err != nil {
			t.Fatalf("Lookup #%d: %v", i+1, err)
		}
		if got.ID != user.ID || inv.ConsumedAt != nil {
			t.Fatalf("unexpected lookup result: inv=%+v user=%+v", inv, got)
		}
	}

	// 仍然可以正常消费
	if _, err := svc.Consume(ctx, plain, "alice"); err != nil {
		t.Fatalf("Consume after lookups: %v", err)
	}
}

func TestLookupRejectsInvalidExpiredConsumed(t *testing.T) {
	svc, db, clock := newService(t)
	ctx := context.Background()

	_, plain := seedInvited(t, svc, db, "alice@example.com")
	if _, _, err := svc.Lookup(ctx, "bogus"); !errors.Is(err, invite.ErrInvalid) {
		t.Errorf("bogus token = %v, want ErrInvalid", err)
	}
	if _, _, err := svc.Lookup(ctx, ""); !errors.Is(err, invite.ErrInvalid) {
		t.Errorf("empty token = %v, want ErrInvalid", err)
	}

	clock.Advance(testTTL + time.Minute)
	if _, _, err := svc.Lookup(ctx, plain); !errors.Is(err, invite.ErrInvalid) {
		t.Errorf("expired token = %v, want ErrInvalid", err)
	}
}

func TestConsumeActivatesUser(t *testing.T) {
	svc, db, _ := newService(t)
	user, plain := seedInvited(t, svc, db, "alice@example.com")
	ctx := context.Background()

	got, err := svc.Consume(ctx, plain, "Alice")
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if got.UIDValue() != "Alice" {
		t.Errorf("uid = %q, want Alice（保持大小写）", got.UIDValue())
	}
	if got.Status != domain.UserStatusActive {
		t.Errorf("status = %q, want active", got.Status)
	}
	if got.ActivatedAt == nil {
		t.Error("activated_at not set")
	}

	var consumedAt *time.Time
	if err := db.Raw(`SELECT consumed_at FROM invitations WHERE user_id = ?`, user.ID).
		Row().Scan(&consumedAt); err != nil {
		t.Fatal(err)
	}
	if consumedAt == nil {
		t.Error("invitation not marked consumed")
	}
}

// 验收 #2：同一 token 只能用一次
func TestConsumeTwiceFails(t *testing.T) {
	svc, db, _ := newService(t)
	_, plain := seedInvited(t, svc, db, "alice@example.com")
	ctx := context.Background()

	if _, err := svc.Consume(ctx, plain, "alice"); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	if _, err := svc.Consume(ctx, plain, "alice2"); !errors.Is(err, invite.ErrInvalid) {
		t.Fatalf("second Consume = %v, want ErrInvalid", err)
	}
}

// 验收 #3：重发（重新创建）后旧链接立即失效
func TestReCreateInvalidatesPrevious(t *testing.T) {
	svc, db, _ := newService(t)
	user, first := seedInvited(t, svc, db, "alice@example.com")
	ctx := context.Background()

	second, _, err := svc.Create(ctx, invite.CreateParams{
		UserID: user.ID, Email: "alice@example.com", CreatedBy: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := svc.Lookup(ctx, first); !errors.Is(err, invite.ErrInvalid) {
		t.Fatalf("old token should be invalid, got %v", err)
	}
	if _, err := svc.Consume(ctx, second, "alice"); err != nil {
		t.Fatalf("new token should work: %v", err)
	}
}

// 验收 #4：过期链接
func TestConsumeRejectsExpired(t *testing.T) {
	svc, db, clock := newService(t)
	_, plain := seedInvited(t, svc, db, "alice@example.com")

	clock.Advance(testTTL + time.Second)
	if _, err := svc.Consume(context.Background(), plain, "alice"); !errors.Is(err, invite.ErrInvalid) {
		t.Fatalf("expired = %v, want ErrInvalid", err)
	}
}

// 验收 #5：uid 被占用 → ErrUIDTaken，且不泄漏数据库错误
func TestConsumeRejectsTakenUID(t *testing.T) {
	svc, db, _ := newService(t)
	existing := "Alice"
	testsupport.CreateUser(t, db, "existing@example.com", domain.UserStatusActive, &existing)
	_, plain := seedInvited(t, svc, db, "new@example.com")
	ctx := context.Background()

	_, err := svc.Consume(ctx, plain, "Alice")
	if !errors.Is(err, invite.ErrUIDTaken) {
		t.Fatalf("err = %v, want ErrUIDTaken", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte("UNIQUE")) ||
		bytes.Contains([]byte(err.Error()), []byte("constraint")) {
		t.Fatalf("error must not leak SQL details: %v", err)
	}

	// uid 冲突时邀请**不应**被消费掉，用户仍可换名激活
	if _, err := svc.Consume(ctx, plain, "alice"); err != nil {
		t.Fatalf("retry with a free uid should work: %v", err)
	}
}

// 大小写敏感：Alice 存在时 alice 仍可注册
func TestUIDIsCaseSensitive(t *testing.T) {
	svc, db, _ := newService(t)
	existing := "Alice"
	testsupport.CreateUser(t, db, "existing@example.com", domain.UserStatusActive, &existing)
	_, plain := seedInvited(t, svc, db, "new@example.com")

	got, err := svc.Consume(context.Background(), plain, "alice")
	if err != nil {
		t.Fatalf("alice should be available when Alice exists: %v", err)
	}
	if got.UIDValue() != "alice" {
		t.Fatalf("uid = %q, want alice", got.UIDValue())
	}
}

func TestConsumeRejectsAlreadyActiveUser(t *testing.T) {
	svc, db, _ := newService(t)
	// 用户已是 active（uid 已设），此时邀请不再可用
	uid := "bob"
	user := testsupport.CreateUser(t, db, "bob@example.com", domain.UserStatusActive, &uid)
	plain, _, err := svc.Create(context.Background(), invite.CreateParams{
		UserID: user.ID, Email: user.Email, CreatedBy: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Consume(context.Background(), plain, "bob2"); !errors.Is(err, invite.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

// 并发：同一 token 只有一个能成功
func TestConcurrentConsumeSameToken(t *testing.T) {
	svc, db, _ := newService(t)
	_, plain := seedInvited(t, svc, db, "alice@example.com")
	ctx := context.Background()

	const n = 5
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCount int
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := svc.Consume(ctx, plain, "alice"); err == nil {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if okCount != 1 {
		t.Fatalf("successful consumes = %d, want exactly 1", okCount)
	}
}

// 并发：两个邀请抢同一个 uid，只有一个成功
func TestConcurrentConsumeSameUID(t *testing.T) {
	svc, db, _ := newService(t)
	_, tokenA := seedInvited(t, svc, db, "a@example.com")
	_, tokenB := seedInvited(t, svc, db, "b@example.com")
	ctx := context.Background()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errsA error
		errsB error
	)
	wg.Add(2)
	go func() { defer wg.Done(); _, errsA = svc.Consume(ctx, tokenA, "shared"); mu.Lock(); mu.Unlock() }()
	go func() { defer wg.Done(); _, errsB = svc.Consume(ctx, tokenB, "shared"); mu.Lock(); mu.Unlock() }()
	wg.Wait()

	successes := 0
	taken := 0
	for _, err := range []error{errsA, errsB} {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, invite.ErrUIDTaken):
			taken++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 || taken != 1 {
		t.Fatalf("successes=%d taken=%d, want 1/1", successes, taken)
	}

	var count int64
	if err := db.Raw(`SELECT COUNT(*) FROM users WHERE uid = 'shared'`).Row().Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("users with uid=shared = %d, want 1", count)
	}
}

func TestUIDAvailable(t *testing.T) {
	svc, db, _ := newService(t)
	uid := "alice"
	testsupport.CreateUser(t, db, "alice@example.com", domain.UserStatusActive, &uid)
	ctx := context.Background()

	ok, err := svc.UIDAvailable(ctx, "alice")
	if err != nil || ok {
		t.Fatalf("UIDAvailable(alice) = %v, %v; want false, nil", ok, err)
	}
	if ok, err := svc.UIDAvailable(ctx, "Alice"); err != nil || !ok {
		t.Fatalf("UIDAvailable(Alice) = %v, %v; want true, nil（大小写敏感）", ok, err)
	}
}

// uid 检查次数上限（每 token 20 次，且绝不超限）
func TestRecordUIDCheckLimit(t *testing.T) {
	svc, db, _ := newService(t)
	_, plain := seedInvited(t, svc, db, "alice@example.com")
	ctx := context.Background()

	for i := 0; i < testCheckLimit; i++ {
		if err := svc.RecordUIDCheck(ctx, plain); err != nil {
			t.Fatalf("check #%d: %v", i+1, err)
		}
	}
	if err := svc.RecordUIDCheck(ctx, plain); !errors.Is(err, invite.ErrCheckLimit) {
		t.Fatalf("check #%d = %v, want ErrCheckLimit", testCheckLimit+1, err)
	}

	var count int
	if err := db.Raw(`SELECT check_count FROM invitations LIMIT 1`).Row().Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != testCheckLimit {
		t.Fatalf("check_count = %d, want %d", count, testCheckLimit)
	}

	// 无效 token 返回 ErrInvalid 而不是 ErrCheckLimit
	if err := svc.RecordUIDCheck(ctx, "bogus"); !errors.Is(err, invite.ErrInvalid) {
		t.Fatalf("bogus token = %v, want ErrInvalid", err)
	}
}

func TestConsumeRequiresArguments(t *testing.T) {
	svc, db, _ := newService(t)
	_, plain := seedInvited(t, svc, db, "alice@example.com")
	ctx := context.Background()

	if _, err := svc.Consume(ctx, "", "alice"); !errors.Is(err, invite.ErrInvalid) {
		t.Error("empty token must be rejected")
	}
	if _, err := svc.Consume(ctx, plain, ""); !errors.Is(err, invite.ErrInvalid) {
		t.Error("empty uid must be rejected")
	}
}

func TestCreateRequiresUserID(t *testing.T) {
	svc, _, _ := newService(t)
	if _, _, err := svc.Create(context.Background(), invite.CreateParams{}); err == nil {
		t.Fatal("expected error for missing UserID")
	}
}

func TestDeleteExpired(t *testing.T) {
	svc, db, clock := newService(t)
	seedInvited(t, svc, db, "alice@example.com")
	clock.Advance(testTTL + 31*24*time.Hour)

	n, err := svc.DeleteExpired(context.Background())
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d, want 1", n)
	}
}

// --- 「尚未命名」规则的三种情形 ---

// active 但还没有用户名（bootstrap 出来的管理员）也可以凭邀请设置用户名。
func TestConsumeAllowsActiveUserWithoutUID(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	// 直接建一个 active 且 uid 为 NULL 的用户（正是引导管理员的形态）
	user := testsupport.CreateUser(t, db, "ops@example.com", domain.UserStatusActive, nil)
	plain, _, err := svc.Create(ctx, invite.CreateParams{
		UserID: user.ID, Email: user.Email, Purpose: domain.PurposeAdminUID, CreatedBy: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.Consume(ctx, plain, "ops")
	if err != nil {
		t.Fatalf("Consume(active & uid=NULL) = %v，应当允许", err)
	}
	if got.UIDValue() != "ops" || got.Status != domain.UserStatusActive {
		t.Fatalf("user = %s/%s", got.UIDValue(), got.Status)
	}
	if got.ActivatedAt == nil {
		t.Error("activated_at 应当被填上")
	}
}

// 停用用户不应当能改身份。
func TestConsumeRejectsDisabledUser(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	user := testsupport.CreateUser(t, db, "gone@example.com", domain.UserStatusDisabled, nil)
	plain, _, err := svc.Create(ctx, invite.CreateParams{UserID: user.ID, Email: user.Email, CreatedBy: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Consume(ctx, plain, "gone"); !errors.Is(err, invite.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid（停用期间不应改身份）", err)
	}
}

// Lookup 对 active 且未命名的用户同样有效——落地页要能渲染"设置用户名"。
func TestLookupAllowsActiveUserWithoutUID(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	user := testsupport.CreateUser(t, db, "ops@example.com", domain.UserStatusActive, nil)
	plain, _, err := svc.Create(ctx, invite.CreateParams{UserID: user.ID, Email: user.Email, CreatedBy: 0})
	if err != nil {
		t.Fatal(err)
	}
	inv, got, err := svc.Lookup(ctx, plain)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ID != user.ID || inv.ConsumedAt != nil {
		t.Fatalf("unexpected: %+v", got)
	}
}

// HasActive：用于启动时判断"要不要重发设置链接"。
func TestHasActive(t *testing.T) {
	svc, db, clock := newService(t)
	ctx := context.Background()

	user := testsupport.CreateUser(t, db, "ops@example.com", domain.UserStatusActive, nil)

	has, err := svc.HasActive(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("尚无邀请时不应报告存在有效邀请")
	}

	plain, _, err := svc.Create(ctx, invite.CreateParams{UserID: user.ID, Email: user.Email, CreatedBy: 0})
	if err != nil {
		t.Fatal(err)
	}
	if has, _ := svc.HasActive(ctx, user.ID); !has {
		t.Fatal("刚创建的邀请应被视为有效")
	}

	// 过期后不再算有效
	clock.Advance(testTTL + time.Minute)
	if has, _ := svc.HasActive(ctx, user.ID); has {
		t.Fatal("过期邀请不应被视为有效")
	}

	// 消费后不再算有效
	clock.Set(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	fresh, _, err := svc.Create(ctx, invite.CreateParams{UserID: user.ID, Email: user.Email, CreatedBy: 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Consume(ctx, fresh, "ops"); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if has, _ := svc.HasActive(ctx, user.ID); has {
		t.Fatal("已消费的邀请不应被视为有效")
	}
	_ = plain
}
