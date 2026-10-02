package usercenter_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	uc "github.com/tangthinker/user-center/v2"
	"github.com/tangthinker/user-center/v2/internal/app"
	"github.com/tangthinker/user-center/v2/internal/testsupport"
)

// recordingMailer 记录发送内容（测试用）。
type recordingMailer struct {
	mu   sync.Mutex
	sent []uc.Message
}

func (m *recordingMailer) Send(_ context.Context, msg uc.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *recordingMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func baseConfig(t *testing.T, dir string) uc.Config {
	t.Helper()
	return uc.Config{
		DBPath:              dir,
		ServiceName:         "测试服务",
		PublicBaseURL:       "https://svc.example.com",
		BootstrapAdminEmail: "ops@example.com",
		HMACKey:             []byte("0123456789abcdef0123456789abcdef"),
		UAHashSalt:          "salt",
	}
}

func TestNewValidatesRequiredFields(t *testing.T) {
	dir := t.TempDir()

	cases := map[string]func(*uc.Config){
		"no DBPath":        func(c *uc.Config) { c.DBPath = "" },
		"no ServiceName":   func(c *uc.Config) { c.ServiceName = "" },
		"no PublicBaseURL": func(c *uc.Config) { c.PublicBaseURL = "" },
		"no HMACKey":       func(c *uc.Config) { c.HMACKey = nil },
		"short HMACKey":    func(c *uc.Config) { c.HMACKey = []byte("short") },
		"mail without from": func(c *uc.Config) {
			c.Mail = &uc.MailConfig{Host: "smtp.qiye.aliyun.com"}
		},
		"mail user without password": func(c *uc.Config) {
			c.Mail = &uc.MailConfig{Host: "smtp.qiye.aliyun.com", From: "a@b.com", Username: "a@b.com"}
		},
	}
	for name, mutate := range cases {
		cfg := baseConfig(t, dir)
		mutate(&cfg)
		if _, err := uc.New(cfg); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestNewCreatesInstanceAndBootstrapAdmin(t *testing.T) {
	dir := t.TempDir()
	instance, err := uc.New(baseConfig(t, dir))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = instance.Close() }()

	if instance.App() == nil || instance.DB() == nil || instance.Store() == nil {
		t.Fatal("accessors must not be nil")
	}
	// 库文件落在指定目录
	if _, err := filepath.Glob(filepath.Join(dir, "user-center.db")); err != nil {
		t.Fatal(err)
	}

	users, err := instance.App().AdminListUsers(context.Background(), app.Actor{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || !users[0].IsAdmin || users[0].Email != "ops@example.com" {
		t.Fatalf("bootstrap admin missing: %+v", users)
	}
}

// L2：同一进程内两个实例必须完全隔离
func TestTwoInstancesAreIsolated(t *testing.T) {
	a, err := uc.New(baseConfig(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()

	bCfg := baseConfig(t, t.TempDir())
	bCfg.BootstrapAdminEmail = ""
	b, err := uc.New(bCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	if a.Store().Path() == b.Store().Path() {
		t.Fatal("instances share the same database file")
	}

	ctx := context.Background()
	if _, err := a.App().AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}

	usersA, err := a.App().AdminListUsers(ctx, app.Actor{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	usersB, err := b.App().AdminListUsers(ctx, app.Actor{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(usersA) != 2 {
		t.Fatalf("instance A users = %d, want 2", len(usersA))
	}
	if len(usersB) != 0 {
		t.Fatalf("instance B users = %d, want 0", len(usersB))
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	instance, err := uc.New(baseConfig(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := instance.Close(); err != nil {
		t.Fatalf("second Close should be a no-op: %v", err)
	}
}

// 未注入 Mailer 时，邮件只入队不投递；其余功能照常。
func TestInstanceWithoutMailer(t *testing.T) {
	instance, err := uc.New(baseConfig(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instance.Close() }()

	ctx := context.Background()
	res, err := instance.App().AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com")
	if err != nil {
		t.Fatalf("create user without mailer: %v", err)
	}
	// 未配置发送器时**不入队**：mail_queued=false 是诚实的信号，
	// 提示管理员改用"复制邀请链接"人工送达，而不是以为邮件已发出。
	if res.MailQueued {
		t.Fatal("mail_queued should be false when no mailer is configured")
	}
	if res.InviteToken == "" {
		t.Fatal("invite token must still be returned for manual delivery")
	}
	stats, err := instance.QueueStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 0 {
		t.Fatalf("pending mails = %d, want 0", stats.Pending)
	}
}

func TestVerifyTokenOnlyAcceptsUserSessions(t *testing.T) {
	cfg := baseConfig(t, t.TempDir())
	cfg.Mailer = &recordingMailer{}
	cfg.DisableWorker = true
	instance, err := uc.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instance.Close() }()
	ctx := context.Background()

	// 造一个已激活用户并登录
	if _, err := instance.App().AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.App().AcceptInvite(ctx, inviteToken(t, instance, "alice@example.com"), "alice", "127.0.0.1", "ua"); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.App().RequestLoginCode(ctx, "alice@example.com", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	code := otpCode(t, instance, "alice@example.com")
	login, err := instance.App().VerifyLoginCode(ctx, "alice@example.com", code, "127.0.0.1", "ua")
	if err != nil {
		t.Fatal(err)
	}

	uid, err := instance.VerifyToken(login.Token)
	if err != nil || uid != "alice" {
		t.Fatalf("VerifyToken = %q, %v; want alice, nil", uid, err)
	}

	// 无效令牌
	if _, err := instance.VerifyToken("forged"); !errors.Is(err, app.ErrSessionInvalid) {
		t.Fatalf("forged token err = %v, want ErrSessionInvalid", err)
	}

	// 管理会话不得作为下游业务凭据
	if _, err := instance.App().RequestAdminLoginCode(ctx, "ops@example.com", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	adminCode := otpCode(t, instance, "ops@example.com")
	adminLogin, err := instance.App().VerifyAdminCode(ctx, "ops@example.com", adminCode, "127.0.0.1", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.VerifyToken(adminLogin.Token); !errors.Is(err, app.ErrSessionInvalid) {
		t.Fatalf("admin session err = %v, want ErrSessionInvalid（管理会话不能当用户会话）", err)
	}
	// 但会话本身是有效的（scope 可区分）
	if sess, err := instance.VerifySessionContext(ctx, adminLogin.Token); err != nil || sess.Scope != "admin" {
		t.Fatalf("VerifySessionContext = %+v, %v", sess, err)
	}
}

func TestMaintenanceCleansExpiredRows(t *testing.T) {
	cfg := baseConfig(t, t.TempDir())
	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg.Now = clock.Now
	cfg.Mailer = &recordingMailer{}
	cfg.DisableWorker = true
	instance, err := uc.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instance.Close() }()

	ctx := context.Background()
	if _, err := instance.App().AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.App().AcceptInvite(ctx, inviteToken(t, instance, "alice@example.com"), "alice", "127.0.0.1", "ua"); err != nil {
		t.Fatal(err)
	}
	if _, err := instance.App().RequestLoginCode(ctx, "alice@example.com", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	// 推到很久以后：验证码/会话/邀请都应被清掉
	clock.Advance(400 * 24 * time.Hour)
	if err := instance.Maintenance(ctx); err != nil {
		t.Fatalf("Maintenance: %v", err)
	}

	var otpLeft, sessionLeft int64
	if err := instance.DB().Raw(`SELECT COUNT(*) FROM otp_codes`).Row().Scan(&otpLeft); err != nil {
		t.Fatal(err)
	}
	if err := instance.DB().Raw(`SELECT COUNT(*) FROM sessions`).Row().Scan(&sessionLeft); err != nil {
		t.Fatal(err)
	}
	if otpLeft != 0 || sessionLeft != 0 {
		t.Fatalf("cleanup left otp=%d sessions=%d", otpLeft, sessionLeft)
	}
}

// 端到端：真实监听 + 真实 HTTP，覆盖"宿主两个监听"的部署形态。
func TestEndToEndPublicAndAdminOverRealListener(t *testing.T) {
	pubLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig(t, t.TempDir())
	cfg.PublicBaseURL = "http://" + pubLn.Addr().String()
	cfg.InvitePath = "/api/v1/invite"
	cfg.Mailer = &recordingMailer{}
	cfg.WorkerInterval = time.Hour
	instance, err := uc.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instance.Close() }()

	// 公共面监听
	publicApp := fiber.New(fiber.Config{DisableStartupMessage: true})
	if _, err := instance.RegisterPublic(publicApp.Group("/api/v1"), "/api/v1"); err != nil {
		t.Fatal(err)
	}
	// 管理面监听（真实部署里应只绑回环；这里同样绑在回环上）
	adminApp := fiber.New(fiber.Config{DisableStartupMessage: true})
	if _, err := instance.RegisterAdmin(adminApp.Group("/admin"), "/admin"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = publicApp.Listener(pubLn) }()
	go func() { defer wg.Done(); _ = adminApp.Listener(adminLn) }()
	t.Cleanup(func() {
		_ = publicApp.Shutdown()
		_ = adminApp.Shutdown()
		wg.Wait()
	})

	pubBase := "http://" + pubLn.Addr().String()
	adminBase := "http://" + adminLn.Addr().String()

	// 管理登录
	res, err := http.Post(adminBase+"/admin/otp/send", "application/json",
		strings.NewReader(`{"email":"ops@example.com"}`))
	if err != nil {
		t.Fatalf("admin otp send: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("admin otp send = %d", res.StatusCode)
	}
	_ = res.Body.Close()

	adminCode := otpCode(t, instance, "ops@example.com")
	res, err = http.Post(adminBase+"/admin/otp/verify", "application/json",
		strings.NewReader(`{"email":"ops@example.com","code":"`+adminCode+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("admin otp verify = %d %s", res.StatusCode, body)
	}
	var parsed struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Data.Token == "" {
		t.Fatalf("admin token missing: %s", body)
	}

	// 用管理令牌建用户 → 邀请链接指向**公共面**
	req, _ := http.NewRequest(http.MethodPost, adminBase+"/admin/users",
		strings.NewReader(`{"email":"alice@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+parsed.Data.Token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("create user = %d %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), pubBase+"/api/v1/invite?token=") {
		t.Fatalf("invite link should point at the public listener: %s", body)
	}

	// 公共面上打开落地页
	var created struct {
		Data struct {
			InviteURL string `json:"invite_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	res, err = http.Get(created.Data.InviteURL)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(page), "alice@example.com") {
		t.Fatalf("invite page = %d", res.StatusCode)
	}
}

// --- helpers ---

func otpCode(t *testing.T, instance *uc.UserCenter, to string) string {
	t.Helper()
	var payload *string
	if err := instance.DB().Raw(
		`SELECT payload FROM mail_outbox WHERE template = 'otp_code' AND to_email = ? ORDER BY id DESC LIMIT 1`, to,
	).Row().Scan(&payload); err != nil {
		t.Fatalf("read otp payload: %v", err)
	}
	if payload == nil {
		t.Fatalf("no otp mail for %s", to)
	}
	var p struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(*payload), &p); err != nil {
		t.Fatal(err)
	}
	return p.Code
}

func inviteToken(t *testing.T, instance *uc.UserCenter, to string) string {
	t.Helper()
	var payload *string
	if err := instance.DB().Raw(
		`SELECT payload FROM mail_outbox WHERE template = 'invite' AND to_email = ? ORDER BY id DESC LIMIT 1`, to,
	).Row().Scan(&payload); err != nil {
		t.Fatalf("read invite payload: %v", err)
	}
	if payload == nil {
		t.Fatalf("no invite mail for %s", to)
	}
	var p struct {
		InviteURL string `json:"invite_url"`
	}
	if err := json.Unmarshal([]byte(*payload), &p); err != nil {
		t.Fatal(err)
	}
	const marker = "token="
	idx := strings.Index(p.InviteURL, marker)
	if idx < 0 {
		t.Fatalf("invite url without token: %s", p.InviteURL)
	}
	return p.InviteURL[idx+len(marker):]
}

// SMTP 自检接口的参数校验（真实连通性用 `go run ./examples/demo -test-mail` 验证）。
func TestVerifySMTPAndSendTestMailValidation(t *testing.T) {
	if err := uc.VerifySMTP(context.Background(), nil); err == nil {
		t.Error("VerifySMTP(nil) must fail")
	}
	if err := uc.SendTestMail(context.Background(), nil, "a@example.com"); err == nil {
		t.Error("SendTestMail(nil) must fail")
	}

	bad := &uc.MailConfig{Host: "smtp.example.com", From: "not-an-address"}
	if err := uc.VerifySMTP(context.Background(), bad); err == nil {
		t.Error("VerifySMTP with invalid From must fail")
	}
	ok := &uc.MailConfig{Host: "smtp.example.com", From: "noreply@example.com"}
	if err := uc.SendTestMail(context.Background(), ok, "bad-address"); err == nil {
		t.Error("SendTestMail with invalid recipient must fail")
	}
}

// 阿里企业邮箱的演示配置能被正确转换成库配置（含 From 回退与 TLS 默认）。
func TestMailConfigFromAlibabaPreset(t *testing.T) {
	cfg := uc.MailConfig{
		Host: "smtp.qiye.aliyun.com", Port: 465, ImplicitTLS: true,
		Username: "noreply@example.com", Password: "client-auth-code",
		From: "noreply@example.com", FromName: "云盘",
	}
	instance, err := uc.New(uc.Config{
		DBPath: t.TempDir(), ServiceName: "云盘", PublicBaseURL: "https://x.example.com",
		HMACKey: []byte("0123456789abcdef0123456789abcdef"),
		Mail:    &cfg, DisableWorker: true,
	})
	if err != nil {
		t.Fatalf("New with alibaba preset: %v", err)
	}
	defer func() { _ = instance.Close() }()
}

// 启动引导会向管理员发出"设置用户名"的邮件；重复启动不会重复发信。
func TestNewSendsAdminUIDInviteOnce(t *testing.T) {
	mails := &recordingMailer{}
	cfg := baseConfig(t, t.TempDir())
	cfg.Mailer = mails
	cfg.DisableWorker = true // 让邮件留在队列里，便于断言

	instance, err := uc.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = instance.Close() }()

	res := instance.BootstrapResult()
	if res == nil {
		t.Fatal("BootstrapResult() = nil")
	}
	if res.Action != "created" || res.UIDSet {
		t.Fatalf("res = %+v，期望 created 且尚未设置用户名", res)
	}
	if !res.InviteSent {
		t.Fatal("启动时应当给管理员发出设置用户名的邮件")
	}

	var pending int64
	if err := instance.DB().Raw(
		`SELECT COUNT(*) FROM mail_outbox WHERE template = 'invite'`).Row().Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("邀请邮件 = %d 封，期望 1 封", pending)
	}

	// 用同一个数据目录再启动一次（模拟重启）：不应再发第二封，
	// 因为对方邮箱里那条链接还有效，重发会把它作废。
	second, err := uc.New(cfg)
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() { _ = second.Close() }()

	res2 := second.BootstrapResult()
	if res2.InviteSent {
		t.Fatal("重启不应重发设置用户名邮件")
	}
	if !res2.ExistingInvite {
		t.Fatalf("res = %+v，期望识别出已有一封有效链接", res2)
	}

	var total int64
	if err := second.DB().Raw(
		`SELECT COUNT(*) FROM mail_outbox WHERE template = 'invite'`).Row().Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("邀请邮件累计 %d 封，期望仍是 1 封", total)
	}
}

// 管理员设置完用户名后，后续启动不再发信。
func TestNewSkipsAdminUIDInviteAfterUIDSet(t *testing.T) {
	cfg := baseConfig(t, t.TempDir())
	cfg.DisableWorker = true

	instance, err := uc.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = instance.Close() }()

	ctx := context.Background()
	ucApp := instance.App() // 注意：变量名不要叫 app，否则会遮住 app 包

	var adminID int64
	if err := instance.DB().Raw(
		`SELECT id FROM users WHERE email = ?`, "ops@example.com").Row().Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	actor := app.Actor{ID: adminID, Email: "ops@example.com"}

	// 这个用例没有注入 Mailer，因此拿不到队列里的明文链接——
	// 正好走一遍"邮件丢了怎么办"的真实恢复路径：在管理界面重新生成链接。
	link, err := ucApp.AdminRegenerateInviteLink(ctx, actor, adminID)
	if err != nil {
		t.Fatalf("AdminRegenerateInviteLink: %v", err)
	}
	if _, err := ucApp.AcceptInvite(ctx, link.InviteToken, "ops", "127.0.0.1", "ua"); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}

	second, err := uc.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	res := second.BootstrapResult()
	if !res.UIDSet || res.InviteSent || res.ExistingInvite {
		t.Fatalf("res = %+v，期望 UIDSet=true 且完全不发信", res)
	}
}
