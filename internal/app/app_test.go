package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/app"
	"github.com/tangthinker/user-center/v2/internal/audit"
	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/invite"
	"github.com/tangthinker/user-center/v2/internal/mail"
	"github.com/tangthinker/user-center/v2/internal/store"
	"github.com/tangthinker/user-center/v2/internal/testsupport"
	"gorm.io/gorm"
)

type fakeMailer struct {
	mu   sync.Mutex
	sent []mail.Message
}

func (f *fakeMailer) Send(_ context.Context, msg mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeMailer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type harness struct {
	app    *app.App
	db     *gorm.DB
	mailer *fakeMailer
	clock  *testsupport.Clock
}

func newHarness(t *testing.T, mutate func(*app.Config)) *harness {
	t.Helper()
	st, err := store.Open(store.Config{DBPath: t.TempDir()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	mailer := &fakeMailer{}
	cfg := app.Config{
		ServiceName:         "测试服务",
		PublicBaseURL:       "https://svc.example.com",
		BootstrapAdminEmail: "ops@example.com",
		HMACKey:             []byte("unit-test-hmac-key"),
		UAHashSalt:          "unit-test-salt",
		Now:                 clock.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := app.New(st, mailer, cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	return &harness{app: a, db: st.DB(), mailer: mailer, clock: clock}
}

// --- 从发件箱 payload 里取回明文，用于走完整链路 ---

type otpPayload struct {
	Code string `json:"code"`
}

func otpCodeFromOutbox(t *testing.T, db *gorm.DB, to string) string {
	t.Helper()
	var payload *string
	if err := db.Raw(
		`SELECT payload FROM mail_outbox WHERE template = ? AND to_email = ? ORDER BY id DESC LIMIT 1`,
		mail.TemplateOTPCode, to,
	).Row().Scan(&payload); err != nil {
		t.Fatalf("read otp payload: %v", err)
	}
	if payload == nil {
		t.Fatal("otp mail has no payload")
	}
	var p otpPayload
	if err := json.Unmarshal([]byte(*payload), &p); err != nil {
		t.Fatalf("parse otp payload: %v", err)
	}
	return p.Code
}

type invitePayload struct {
	InviteURL string `json:"invite_url"`
}

func inviteTokenFromOutbox(t *testing.T, db *gorm.DB, to string) string {
	t.Helper()
	var payload *string
	if err := db.Raw(
		`SELECT payload FROM mail_outbox WHERE template = ? AND to_email = ? ORDER BY id DESC LIMIT 1`,
		mail.TemplateInvite, to,
	).Row().Scan(&payload); err != nil {
		t.Fatalf("read invite payload: %v", err)
	}
	if payload == nil {
		t.Fatal("invite mail has no payload")
	}
	var p invitePayload
	if err := json.Unmarshal([]byte(*payload), &p); err != nil {
		t.Fatalf("parse invite payload: %v", err)
	}
	u, err := url.Parse(p.InviteURL)
	if err != nil {
		t.Fatalf("parse invite url: %v", err)
	}
	return u.Query().Get("token")
}

func statusOfUser(t *testing.T, db *gorm.DB, email string) (string, string) {
	t.Helper()
	var status string
	var uid *string
	if err := db.Raw(`SELECT status, uid FROM users WHERE email = ?`, email).
		Row().Scan(&status, &uid); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if uid == nil {
		return status, ""
	}
	return status, *uid
}

func countRows(t *testing.T, db *gorm.DB, table string) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT COUNT(*) FROM ` + table).Row().Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// --- Bootstrap ---

func TestBootstrapCreatesAdminThenSkips(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	res, err := h.app.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if res.Action != "created" {
		t.Fatalf("action = %q, want created", res.Action)
	}

	status, _ := statusOfUser(t, h.db, "ops@example.com")
	if status != domain.UserStatusActive {
		t.Fatalf("admin status = %q, want active", status)
	}
	var isAdmin bool
	if err := h.db.Raw(`SELECT is_admin FROM users WHERE email = ?`, "ops@example.com").
		Row().Scan(&isAdmin); err != nil {
		t.Fatal(err)
	}
	if !isAdmin {
		t.Fatal("bootstrap user is not admin")
	}

	// 幂等：第二次不会再造管理员，也不会重发"设置用户名"邮件
	again, err := h.app.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.Action != "noop" {
		t.Fatalf("second Bootstrap = %q, want noop（管理员已存在）", again.Action)
	}
	if again.ExistingInvite != true {
		t.Fatal("第二次引导应识别出已有一封未过期的设置链接，而不是重发")
	}
	if n := countRows(t, h.db, "users"); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}

func TestBootstrapPromotesExistingUser(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	// 先有一个普通用户
	if _, err := h.app.AdminCreateUser(ctx, app.Actor{ID: 99, Email: "bootstrap@example.com"}, "ops@example.com"); err != nil {
		t.Fatal(err)
	}

	res, err := h.app.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "promoted" {
		t.Fatalf("action = %q, want promoted", res.Action)
	}
	status, _ := statusOfUser(t, h.db, "ops@example.com")
	if status != domain.UserStatusActive {
		t.Fatalf("promoted admin status = %q, want active", status)
	}
}

func TestBootstrapSkipsWithoutEmail(t *testing.T) {
	h := newHarness(t, func(c *app.Config) { c.BootstrapAdminEmail = "" })
	res, err := h.app.Bootstrap(context.Background())
	if err != nil || res.Action != "skipped" {
		t.Fatalf("Bootstrap = %q, %v; want skipped, nil", res.Action, err)
	}
}

// --- 建用户 + 邀请 ---

func TestAdminCreateUserQueuesInviteAndAudits(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com", IP: "127.0.0.1", UA: "test"}

	res, err := h.app.AdminCreateUser(ctx, actor, "Alice@Example.com ")
	if err != nil {
		t.Fatalf("AdminCreateUser: %v", err)
	}
	if res.User.Email != "alice@example.com" {
		t.Fatalf("email not normalized: %q", res.User.Email)
	}
	if res.InviteToken == "" {
		t.Fatal("invite token not returned")
	}
	if !res.MailQueued {
		t.Fatal("invite mail not queued")
	}

	status, uid := statusOfUser(t, h.db, "alice@example.com")
	if status != domain.UserStatusInvited || uid != "" {
		t.Fatalf("user = %s/%q, want invited with empty uid", status, uid)
	}

	// 邮件已入队（尚未投递）
	var pending int64
	if err := h.db.Raw(`SELECT COUNT(*) FROM mail_outbox WHERE status = 'pending'`).Row().Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending mails = %d, want 1", pending)
	}

	// 审计已写
	var action string
	if err := h.db.Raw(`SELECT action FROM admin_audit ORDER BY id DESC LIMIT 1`).Row().Scan(&action); err != nil {
		t.Fatal(err)
	}
	if action != "user_created" {
		t.Fatalf("audit action = %q, want user_created", action)
	}
}

func TestAdminCreateUserRejectsDuplicatesAndBadEmail(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com"}

	if _, err := h.app.AdminCreateUser(ctx, actor, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	_, err := h.app.AdminCreateUser(ctx, actor, "a@example.com")
	if !errors.Is(err, app.ErrEmailTaken) {
		t.Fatalf("duplicate email err = %v, want ErrEmailTaken", err)
	}

	for _, bad := range []string{"", "not-an-email", "a@localhost", "a b@example.com"} {
		if _, err := h.app.AdminCreateUser(ctx, actor, bad); !errors.Is(err, app.ErrEmailInvalid) {
			t.Errorf("AdminCreateUser(%q) err = %v, want ErrEmailInvalid", bad, err)
		}
	}
}

// 完整链路：建用户 → 邀请落地页 → 检查 uid → 激活
func TestFullInviteFlow(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com"}

	if _, err := h.app.AdminCreateUser(ctx, actor, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	token := inviteTokenFromOutbox(t, h.db, "alice@example.com")

	// 落地页只读，不消费
	for i := 0; i < 3; i++ {
		view, err := h.app.GetInvite(ctx, token)
		if err != nil {
			t.Fatalf("GetInvite #%d: %v", i+1, err)
		}
		if view.Email != "alice@example.com" || !view.NeedsUID {
			t.Fatalf("unexpected view: %+v", view)
		}
		if view.ExpiresAt == "" {
			t.Fatal("expiry not rendered")
		}
	}

	// uid 可用性
	available, err := h.app.CheckUID(ctx, token, "Alice")
	if err != nil {
		t.Fatalf("CheckUID: %v", err)
	}
	if !available {
		t.Fatal("uid should be available")
	}

	// 激活
	res, err := h.app.AcceptInvite(ctx, token, "Alice", "1.2.3.4", "ua")
	if err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if res.User.UIDValue() != "Alice" || res.User.Status != domain.UserStatusActive {
		t.Fatalf("activated user = %+v", res.User)
	}

	status, uid := statusOfUser(t, h.db, "alice@example.com")
	if status != domain.UserStatusActive || uid != "Alice" {
		t.Fatalf("user = %s/%q, want active/Alice（大小写保留）", status, uid)
	}

	// 激活**不发放会话**
	if n := countRows(t, h.db, "sessions"); n != 0 {
		t.Fatalf("sessions = %d, want 0（激活不发会话）", n)
	}

	// 欢迎邮件入队
	var template string
	if err := h.db.Raw(
		`SELECT template FROM mail_outbox WHERE to_email = ? ORDER BY id DESC LIMIT 1`, "alice@example.com",
	).Row().Scan(&template); err != nil {
		t.Fatal(err)
	}
	if template != mail.TemplateWelcomeActivated {
		t.Fatalf("last mail = %q, want welcome_activated", template)
	}
}

// 给人看的时间一律按 Config.TimeZone 渲染：
//   - 邮件正文 / 邀请落地页：带偏移的当地钟点（北京 12:13 不再写成 04:13 UTC）；
//   - 管理接口：RFC3339，由客户端按自己的时区显示。
func TestInviteTimesUseConfiguredTimeZone(t *testing.T) {
	h := newHarness(t, func(c *app.Config) { c.TimeZone = "Asia/Shanghai" })
	ctx := context.Background()

	res, err := h.app.AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com")
	if err != nil {
		t.Fatalf("AdminCreateUser: %v", err)
	}
	// 时钟固定在 2026-01-01 00:00 UTC，默认有效期 7 天。
	if res.ExpiresAt != "2026-01-08T00:00:00Z" {
		t.Errorf("CreateUserResult.ExpiresAt = %q, want RFC3339 UTC", res.ExpiresAt)
	}

	view, err := h.app.GetInvite(ctx, inviteTokenFromOutbox(t, h.db, "alice@example.com"))
	if err != nil {
		t.Fatalf("GetInvite: %v", err)
	}
	if want := "2026-01-08 08:00 +08:00"; view.ExpiresAt != want {
		t.Errorf("InviteView.ExpiresAt = %q, want %q", view.ExpiresAt, want)
	}
}

// 时区名写错必须在启动时就报错，而不是等用户收到一封时间不对的邮件。
func TestNewRejectsUnknownTimeZone(t *testing.T) {
	st, err := store.Open(store.Config{DBPath: t.TempDir()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	_, err = app.New(st, nil, app.Config{
		ServiceName:   "测试服务",
		PublicBaseURL: "https://svc.example.com",
		HMACKey:       []byte("unit-test-hmac-key"),
		TimeZone:      "Beijing/Chaoyang",
	})
	if err == nil {
		t.Fatal("expected app.New to reject an unknown TimeZone")
	}
}

func TestAcceptInviteRejectsBadUIDAndTakenUID(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com"}

	if _, err := h.app.AdminCreateUser(ctx, actor, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	token := inviteTokenFromOutbox(t, h.db, "alice@example.com")

	// 规则不合法的 uid
	for _, bad := range []string{"ab", "1abc", "admin", "has space"} {
		if _, err := h.app.AcceptInvite(ctx, token, bad, "1.2.3.4", "ua"); !errors.Is(err, app.ErrUIDInvalid) {
			t.Errorf("AcceptInvite(uid=%q) err = %v, want ErrUIDInvalid", bad, err)
		}
	}

	// 占用冲突
	if _, err := h.app.AdminCreateUser(ctx, actor, "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	bobToken := inviteTokenFromOutbox(t, h.db, "bob@example.com")
	if _, err := h.app.AcceptInvite(ctx, bobToken, "shared", "1.2.3.4", "ua"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.AcceptInvite(ctx, token, "shared", "1.2.3.4", "ua"); !errors.Is(err, app.ErrUIDTaken) {
		t.Fatalf("err = %v, want ErrUIDTaken", err)
	}
}

func TestGetInviteRejectsUnknownToken(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.app.GetInvite(context.Background(), "nope"); !errors.Is(err, app.ErrInviteInvalid) {
		t.Fatalf("err = %v, want ErrInviteInvalid", err)
	}
}

func TestCheckUIDRejectsInvalidUID(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com"}
	if _, err := h.app.AdminCreateUser(ctx, actor, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	token := inviteTokenFromOutbox(t, h.db, "alice@example.com")

	if _, err := h.app.CheckUID(ctx, token, "1bad"); !errors.Is(err, app.ErrUIDInvalid) {
		t.Fatalf("err = %v, want ErrUIDInvalid", err)
	}
}

func TestResendInvalidatesOldLinkAndRegenerateDoesNotMail(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com"}

	created, err := h.app.AdminCreateUser(ctx, actor, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	oldToken := inviteTokenFromOutbox(t, h.db, "alice@example.com")

	// 重新签发链接（复制用）：不发邮件
	before := countRows(t, h.db, "mail_outbox")
	regen, err := h.app.AdminRegenerateInviteLink(ctx, actor, created.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if regen.MailQueued {
		t.Fatal("regenerate must not send mail")
	}
	if after := countRows(t, h.db, "mail_outbox"); after != before {
		t.Fatalf("outbox grew from %d to %d", before, after)
	}

	// 旧链接失效
	if _, err := h.app.GetInvite(ctx, oldToken); !errors.Is(err, app.ErrInviteInvalid) {
		t.Fatalf("old token err = %v, want ErrInviteInvalid", err)
	}
	// 新链接可用
	if _, err := h.app.GetInvite(ctx, regen.InviteToken); err != nil {
		t.Fatalf("regenerated token: %v", err)
	}

	// 重发邀请：发邮件
	resent, err := h.app.AdminResendInvite(ctx, actor, created.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !resent.MailQueued {
		t.Fatal("resend must queue mail")
	}
}

// --- 登录 ---

func seedActiveUser(t *testing.T, h *harness) string {
	t.Helper()
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com"}
	if _, err := h.app.AdminCreateUser(ctx, actor, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	token := inviteTokenFromOutbox(t, h.db, "alice@example.com")
	if _, err := h.app.AcceptInvite(ctx, token, "alice", "1.2.3.4", "ua"); err != nil {
		t.Fatal(err)
	}
	return "alice@example.com"
}

func TestRequestLoginCodeUniformForUnknownEmail(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	seedActiveUser(t, h)

	known, err := h.app.RequestLoginCode(ctx, "alice@example.com", "1.1.1.1")
	if err != nil {
		t.Fatalf("known email: %v", err)
	}
	if !known.UserExists || !known.MailQueued {
		t.Fatalf("known = %+v, want exists+queued", known)
	}

	unknown, err := h.app.RequestLoginCode(ctx, "nobody@example.com", "2.2.2.2")
	if err != nil {
		t.Fatalf("unknown email must not error: %v", err)
	}
	if unknown.UserExists || unknown.MailQueued {
		t.Fatalf("unknown = %+v, want no user and no mail", unknown)
	}

	// 但请求仍然被记账（防止用未注册邮箱白嫖探测）
	var logged int64
	if err := h.db.Raw(`SELECT COUNT(*) FROM mail_log WHERE accepted = 1`).Row().Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if logged != 2 {
		t.Fatalf("mail_log accepted = %d, want 2（未注册邮箱也消耗额度）", logged)
	}
}

func TestRequestLoginCodeCooldownReuse(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	seedActiveUser(t, h)

	first, err := h.app.RequestLoginCode(ctx, "alice@example.com", "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if !first.MailQueued {
		t.Fatal("first request should queue mail")
	}

	h.clock.Advance(20 * time.Second)
	second, err := h.app.RequestLoginCode(ctx, "alice@example.com", "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || second.MailQueued {
		t.Fatalf("second = %+v, want reused and not queued（省配额 + 防骚扰）", second)
	}
	// 复用不产生新邮件：验证码邮件仍然只有 1 封
	var otpMails int64
	if err := h.db.Raw(
		`SELECT COUNT(*) FROM mail_outbox WHERE template = ?`, mail.TemplateOTPCode,
	).Row().Scan(&otpMails); err != nil {
		t.Fatal(err)
	}
	if otpMails != 1 {
		t.Fatalf("otp_code mails = %d, want 1（复用不产生新邮件）", otpMails)
	}
}

func TestLoginFlowIssuesSessionAndSendsFirstLoginWelcomeOnce(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := seedActiveUser(t, h)

	if _, err := h.app.RequestLoginCode(ctx, email, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := otpCodeFromOutbox(t, h.db, email)

	res, err := h.app.VerifyLoginCode(ctx, email, code, "1.1.1.1", "ua")
	if err != nil {
		t.Fatalf("VerifyLoginCode: %v", err)
	}
	if res.Token == "" || res.UID != "alice" {
		t.Fatalf("login result = %+v", res)
	}
	if !res.FirstLogin {
		t.Fatal("first login not detected")
	}

	// 会话可用，且是 user scope
	sess, err := h.app.VerifySession(ctx, res.Token)
	if err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
	if sess.Scope != domain.ScopeUser || sess.UID != "alice" {
		t.Fatalf("session = %+v", sess)
	}
	if _, err := h.app.VerifyUserSession(ctx, res.Token); err != nil {
		t.Fatalf("VerifyUserSession: %v", err)
	}

	// 首次登录欢迎邮件只发一封
	var welcomeCount int64
	if err := h.db.Raw(
		`SELECT COUNT(*) FROM mail_outbox WHERE template = ?`, mail.TemplateWelcomeFirstLogin,
	).Row().Scan(&welcomeCount); err != nil {
		t.Fatal(err)
	}
	if welcomeCount != 1 {
		t.Fatalf("welcome_first_login rows = %d, want 1", welcomeCount)
	}

	// 第二次登录不再标记首次
	h.clock.Advance(2 * time.Minute)
	if _, err := h.app.RequestLoginCode(ctx, email, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code2 := otpCodeFromOutbox(t, h.db, email)
	res2, err := h.app.VerifyLoginCode(ctx, email, code2, "1.1.1.1", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if res2.FirstLogin {
		t.Fatal("second login must not be marked as first")
	}
	if err := h.db.Raw(
		`SELECT COUNT(*) FROM mail_outbox WHERE template = ?`, mail.TemplateWelcomeFirstLogin,
	).Row().Scan(&welcomeCount); err != nil {
		t.Fatal(err)
	}
	if welcomeCount != 1 {
		t.Fatalf("welcome_first_login rows = %d, want still 1", welcomeCount)
	}
}

func TestVerifyLoginCodeFailuresAreUniform(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := seedActiveUser(t, h)

	if _, err := h.app.RequestLoginCode(ctx, email, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}

	// 错误验证码
	if _, err := h.app.VerifyLoginCode(ctx, email, "000000", "1.1.1.1", "ua"); !errors.Is(err, app.ErrInvalidCredentials) {
		t.Fatalf("wrong code err = %v, want ErrInvalidCredentials", err)
	}
	// 未注册邮箱
	if _, err := h.app.VerifyLoginCode(ctx, "nobody@example.com", "000000", "1.1.1.1", "ua"); !errors.Is(err, app.ErrInvalidCredentials) {
		t.Fatalf("unknown email err = %v, want ErrInvalidCredentials", err)
	}

	// 停用用户同样只能得到同一种错误
	var userID int64
	if err := h.db.Raw(`SELECT id FROM users WHERE email = ?`, email).Row().Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := h.app.AdminSetUserStatus(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, userID, domain.UserStatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.VerifyLoginCode(ctx, email, "000000", "1.1.1.1", "ua"); !errors.Is(err, app.ErrInvalidCredentials) {
		t.Fatalf("disabled user err = %v, want ErrInvalidCredentials", err)
	}
}

func TestAdminLoginRequiresAdminAndAdminSessionCannotBeUsedAsUserSession(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	// 普通用户不能走 admin 登录
	email := seedActiveUser(t, h)
	if _, err := h.app.RequestLoginCode(ctx, email, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := otpCodeFromOutbox(t, h.db, email)
	if _, err := h.app.VerifyAdminCode(ctx, email, code, "127.0.0.1", "ua"); !errors.Is(err, app.ErrInvalidCredentials) {
		t.Fatalf("non-admin admin-login err = %v, want ErrInvalidCredentials", err)
	}

	// 管理员登录
	if _, err := h.app.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.RequestAdminLoginCode(ctx, "ops@example.com", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	adminCode := otpCodeFromOutbox(t, h.db, "ops@example.com")

	res, err := h.app.VerifyAdminCode(ctx, "ops@example.com", adminCode, "127.0.0.1", "ua")
	if err != nil {
		t.Fatalf("VerifyAdminCode: %v", err)
	}
	sess, err := h.app.VerifySession(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Scope != domain.ScopeAdmin {
		t.Fatalf("scope = %q, want admin", sess.Scope)
	}

	// 关键：管理会话不能当普通用户会话使用
	if _, err := h.app.VerifyUserSession(ctx, res.Token); !errors.Is(err, app.ErrSessionInvalid) {
		t.Fatalf("VerifyUserSession(admin token) err = %v, want ErrSessionInvalid", err)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := seedActiveUser(t, h)

	if _, err := h.app.RequestLoginCode(ctx, email, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := otpCodeFromOutbox(t, h.db, email)
	res, err := h.app.VerifyLoginCode(ctx, email, code, "1.1.1.1", "ua")
	if err != nil {
		t.Fatal(err)
	}

	if err := h.app.Logout(ctx, res.Token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := h.app.VerifySession(ctx, res.Token); !errors.Is(err, app.ErrSessionInvalid) {
		t.Fatalf("after logout err = %v, want ErrSessionInvalid", err)
	}
	// 幂等
	if err := h.app.Logout(ctx, res.Token); err != nil {
		t.Fatalf("second Logout: %v", err)
	}
}

// --- 管理动作 ---

func TestAdminChangeEmailRevokesSessionsAndNotifiesOldAddress(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := seedActiveUser(t, h)

	if _, err := h.app.RequestLoginCode(ctx, email, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := otpCodeFromOutbox(t, h.db, email)
	res, err := h.app.VerifyLoginCode(ctx, email, code, "1.1.1.1", "ua")
	if err != nil {
		t.Fatal(err)
	}

	var userID int64
	if err := h.db.Raw(`SELECT id FROM users WHERE email = ?`, email).Row().Scan(&userID); err != nil {
		t.Fatal(err)
	}

	actor := app.Actor{ID: 1, Email: "ops@example.com", IP: "127.0.0.1"}
	if err := h.app.AdminChangeEmail(ctx, actor, userID, "new@example.com"); err != nil {
		t.Fatalf("AdminChangeEmail: %v", err)
	}

	// 邮箱已改
	status, _ := statusOfUser(t, h.db, "new@example.com")
	if status != domain.UserStatusActive {
		t.Fatalf("new email status = %q", status)
	}
	// 会话被吊销
	if _, err := h.app.VerifySession(ctx, res.Token); !errors.Is(err, app.ErrSessionInvalid) {
		t.Fatalf("session after email change = %v, want invalid", err)
	}
	// 通知发往旧地址
	var to string
	if err := h.db.Raw(
		`SELECT to_email FROM mail_outbox WHERE template = ? ORDER BY id DESC LIMIT 1`,
		mail.TemplateEmailChangedNotice,
	).Row().Scan(&to); err != nil {
		t.Fatal(err)
	}
	if to != email {
		t.Fatalf("notice sent to %q, want the OLD address %q", to, email)
	}
}

func TestAdminChangeEmailRejectsTakenAddress(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com"}

	first, err := h.app.AdminCreateUser(ctx, actor, "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.AdminCreateUser(ctx, actor, "b@example.com"); err != nil {
		t.Fatal(err)
	}

	if err := h.app.AdminChangeEmail(ctx, actor, first.User.ID, "b@example.com"); !errors.Is(err, app.ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
}

func TestAdminDisableRevokesSessions(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := seedActiveUser(t, h)

	if _, err := h.app.RequestLoginCode(ctx, email, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := otpCodeFromOutbox(t, h.db, email)
	res, err := h.app.VerifyLoginCode(ctx, email, code, "1.1.1.1", "ua")
	if err != nil {
		t.Fatal(err)
	}

	var userID int64
	if err := h.db.Raw(`SELECT id FROM users WHERE email = ?`, email).Row().Scan(&userID); err != nil {
		t.Fatal(err)
	}

	actor := app.Actor{ID: 1, Email: "ops@example.com"}
	if err := h.app.AdminSetUserStatus(ctx, actor, userID, domain.UserStatusDisabled); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := h.app.VerifySession(ctx, res.Token); !errors.Is(err, app.ErrSessionInvalid) {
		t.Fatalf("session after disable = %v, want invalid", err)
	}

	// 重新启用
	if err := h.app.AdminSetUserStatus(ctx, actor, userID, domain.UserStatusActive); err != nil {
		t.Fatalf("enable: %v", err)
	}
	status, _ := statusOfUser(t, h.db, email)
	if status != domain.UserStatusActive {
		t.Fatalf("status = %q, want active", status)
	}
}

func TestCannotDisableOrDeleteLastAdmin(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if _, err := h.app.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	var adminID int64
	if err := h.db.Raw(`SELECT id FROM users WHERE email = ?`, "ops@example.com").Row().Scan(&adminID); err != nil {
		t.Fatal(err)
	}

	actor := app.Actor{ID: adminID, Email: "ops@example.com"}
	if err := h.app.AdminSetUserStatus(ctx, actor, adminID, domain.UserStatusDisabled); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("disable last admin err = %v, want ErrForbidden", err)
	}
	if err := h.app.AdminDeleteUser(ctx, actor, adminID); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("delete last admin err = %v, want ErrForbidden", err)
	}
}

func TestAdminRevokeSessionsAndStats(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	email := seedActiveUser(t, h)

	if _, err := h.app.RequestLoginCode(ctx, email, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	code := otpCodeFromOutbox(t, h.db, email)
	if _, err := h.app.VerifyLoginCode(ctx, email, code, "1.1.1.1", "ua"); err != nil {
		t.Fatal(err)
	}

	var userID int64
	if err := h.db.Raw(`SELECT id FROM users WHERE email = ?`, email).Row().Scan(&userID); err != nil {
		t.Fatal(err)
	}

	actor := app.Actor{ID: 1, Email: "ops@example.com"}
	n, err := h.app.AdminRevokeSessions(ctx, actor, userID)
	if err != nil {
		t.Fatalf("AdminRevokeSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked %d, want 1", n)
	}

	sessions, err := h.app.AdminListSessions(ctx, actor, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("active sessions = %d, want 0", len(sessions))
	}

	stats, err := h.app.AdminStats(ctx, actor)
	if err != nil {
		t.Fatalf("AdminStats: %v", err)
	}
	if stats.UsersByStatus[domain.UserStatusActive] != 1 {
		t.Fatalf("active users = %d, want 1", stats.UsersByStatus[domain.UserStatusActive])
	}
	if stats.MailsLast24h == 0 {
		t.Error("MailsLast24h should count accepted send requests")
	}
}

func TestRateLimitBlocksRepeatedSend(t *testing.T) {
	h := newHarness(t, func(c *app.Config) {
		c.RateLimit.EmailPerHour = 1
		c.RateLimit.EmailPerDay = 1
		c.RateLimit.IPPerHour = 0
		c.RateLimit.IPPerDay = 0
	})
	ctx := context.Background()
	email := seedActiveUser(t, h)

	if _, err := h.app.RequestLoginCode(ctx, email, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	_, err := h.app.RequestLoginCode(ctx, email, "1.1.1.1")
	var rl *app.RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want RateLimitedError", err)
	}
	if rl.Reason != "email_hour" {
		t.Fatalf("reason = %q, want email_hour", rl.Reason)
	}
}

func TestNewRejectsMissingCriticalConfig(t *testing.T) {
	st, err := store.Open(store.Config{DBPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	if _, err := app.New(nil, &fakeMailer{}, app.Config{}); err == nil {
		t.Error("nil store must be rejected")
	}
	if _, err := app.New(st, &fakeMailer{}, app.Config{ServiceName: "x", PublicBaseURL: "y"}); err == nil {
		t.Error("missing HMACKey must be rejected")
	}
	if _, err := app.New(st, &fakeMailer{}, app.Config{HMACKey: []byte("k"), PublicBaseURL: "y"}); err == nil {
		t.Error("missing ServiceName must be rejected")
	}
	if _, err := app.New(st, &fakeMailer{}, app.Config{HMACKey: []byte("k"), ServiceName: "x"}); err == nil {
		t.Error("missing PublicBaseURL must be rejected")
	}
}

// 未注入 Mailer 时不应崩溃：邮件只是不发，其余流程照常可测。
func TestWorksWithoutMailer(t *testing.T) {
	st, err := store.Open(store.Config{DBPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	a, err := app.New(st, nil, app.Config{
		ServiceName: "x", PublicBaseURL: "https://x.example.com", HMACKey: []byte("k"),
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	if _, err := a.AdminCreateUser(context.Background(), app.Actor{ID: 1, Email: "o@example.com"}, "a@example.com"); err != nil {
		t.Fatalf("AdminCreateUser without mailer: %v", err)
	}
}

// --- 引导管理员设置用户名 ---

// 启动引导必须给管理员发一封"设置用户名"的邮件，并写审计。
func TestBootstrapQueuesAdminUIDInvite(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	res, err := h.app.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if res.Action != "created" || res.UIDSet {
		t.Fatalf("res = %+v, 期望 created 且尚未设置用户名", res)
	}
	if !res.InviteSent {
		t.Fatal("Bootstrap 应当发出设置用户名的邀请")
	}

	// 邮件已入队，且收件人是管理员
	var to, tmpl string
	if err := h.db.Raw(
		`SELECT to_email, template FROM mail_outbox ORDER BY id DESC LIMIT 1`,
	).Row().Scan(&to, &tmpl); err != nil {
		t.Fatal(err)
	}
	if to != "ops@example.com" || tmpl != mail.TemplateInvite {
		t.Fatalf("邮件 = %s/%s，期望 ops@example.com/invite", to, tmpl)
	}

	// 审计留痕，动作可区分于普通邀请
	var action, actor string
	if err := h.db.Raw(
		`SELECT action, actor_email FROM admin_audit ORDER BY id DESC LIMIT 1`,
	).Row().Scan(&action, &actor); err != nil {
		t.Fatal(err)
	}
	if action != audit.ActionAdminUIDInviteSent || actor != "system" {
		t.Fatalf("审计 = %s/%s", action, actor)
	}

	// 邀请的用途是 admin_uid，便于运维区分
	var purpose string
	if err := h.db.Raw(`SELECT purpose FROM invitations ORDER BY issued_at DESC LIMIT 1`).Row().Scan(&purpose); err != nil {
		t.Fatal(err)
	}
	if purpose != domain.PurposeAdminUID {
		t.Fatalf("purpose = %q, want %q", purpose, domain.PurposeAdminUID)
	}
}

// 管理员点开链接设置用户名：账号本来就是 active，落地页应只呈现"设置用户名"。
func TestAcceptAdminUIDInvite(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	if _, err := h.app.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	token := inviteTokenFromOutbox(t, h.db, "ops@example.com")

	view, err := h.app.GetInvite(ctx, token)
	if err != nil {
		t.Fatalf("GetInvite: %v", err)
	}
	if !view.NeedsUID {
		t.Fatal("管理员还没有用户名，NeedsUID 应为 true")
	}
	if !view.AlreadyActive {
		t.Fatal("管理员账号已是 active，AlreadyActive 应为 true（页面应显示「设置用户名」而非「激活账号」）")
	}

	if _, err := h.app.AcceptInvite(ctx, token, "ops", "127.0.0.1", "ua"); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}

	uid, status := uidAndStatus(t, h.db, "ops@example.com")
	if uid != "ops" || status != domain.UserStatusActive {
		t.Fatalf("管理员 = %s/%s，期望 ops/active", uid, status)
	}

	// 再次引导：已有用户名 → 什么都不做、不再发信
	again, err := h.app.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !again.UIDSet || again.InviteSent || again.ExistingInvite {
		t.Fatalf("res = %+v，期望 UIDSet=true 且不再发信", again)
	}
}

// 已命名用户不能再用邀请改用户名（uid 不可变）。
func TestAcceptInviteRejectsAlreadyNamedUser(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com"}

	if _, err := h.app.AdminCreateUser(ctx, actor, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	first := inviteTokenFromOutbox(t, h.db, "alice@example.com")
	if _, err := h.app.AcceptInvite(ctx, first, "alice", "127.0.0.1", "ua"); err != nil {
		t.Fatal(err)
	}

	// 给已命名用户再造一张邀请（管理员重发只允许未命名用户，因此这里直接建邀请行）
	var userID int64
	if err := h.db.Raw(`SELECT id FROM users WHERE email = ?`, "alice@example.com").Row().Scan(&userID); err != nil {
		t.Fatal(err)
	}
	plain, _, err := invite.New(h.db, invite.Config{}).Create(ctx, invite.CreateParams{
		UserID: userID, Email: "alice@example.com", CreatedBy: actor.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.AcceptInvite(ctx, plain, "alice2", "127.0.0.1", "ua"); !errors.Is(err, app.ErrInviteInvalid) {
		t.Fatalf("err = %v, want ErrInviteInvalid（用户名不可变）", err)
	}
}

// 管理面重发：未命名的 active 用户（管理员）可以重发；已命名用户被拒。
func TestAdminResendInviteForUnnamedActiveUser(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	boot, err := h.app.Bootstrap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor := app.Actor{ID: boot.UserID, Email: "ops@example.com"}

	before := countRows(t, h.db, "mail_outbox")
	res, err := h.app.AdminResendInvite(ctx, actor, boot.UserID)
	if err != nil {
		t.Fatalf("AdminResendInvite(管理员自身): %v", err)
	}
	if !res.MailQueued || countRows(t, h.db, "mail_outbox") != before+1 {
		t.Fatal("重发应当再入队一封邮件")
	}

	// 设置用户名之后就不能再重发了
	if _, err := h.app.AcceptInvite(ctx, res.InviteToken, "ops", "127.0.0.1", "ua"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.AdminResendInvite(ctx, actor, boot.UserID); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden（已命名用户不再有邀请可发）", err)
	}
}

func uidAndStatus(t *testing.T, db *gorm.DB, email string) (string, string) {
	t.Helper()
	var status string
	var uid *string
	if err := db.Raw(`SELECT status, uid FROM users WHERE email = ?`, email).Row().Scan(&status, &uid); err != nil {
		t.Fatal(err)
	}
	if uid == nil {
		return "", status
	}
	return *uid, status
}
