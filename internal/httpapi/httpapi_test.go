package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/v2/internal/app"
	"github.com/tangthinker/user-center/v2/internal/httpapi"
	"github.com/tangthinker/user-center/v2/internal/mail"
	"github.com/tangthinker/user-center/v2/internal/store"
	"github.com/tangthinker/user-center/v2/internal/testsupport"
	"gorm.io/gorm"
)

type fakeMailer struct{ count int }

func (f *fakeMailer) Send(context.Context, mail.Message) error { f.count++; return nil }

// newEnvWithServiceName 用指定服务名构造环境（验证注入与转义）。
func newEnvWithServiceName(t *testing.T, name string) *env {
	t.Helper()
	return newEnv(t, func(c *app.Config) { c.ServiceName = name })
}

type env struct {
	base  string
	db    *gorm.DB
	app   *app.App
	clock *testsupport.Clock
}

func newEnv(t *testing.T, mutate func(*app.Config)) *env {
	t.Helper()
	return newEnvFull(t, mutate, nil)
}

// newEnvFull 允许同时定制用例层配置与 httpapi 层配置（图标等资源属于后者）。
func newEnvFull(
	t *testing.T,
	mutate func(*app.Config),
	mutateHTTP func(*httpapi.PublicConfig, *httpapi.AdminConfig),
) *env {
	t.Helper()

	// 先占一个回环端口，再把地址写进配置，最后才开始服务。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	base := "http://" + ln.Addr().String()

	st, err := store.Open(store.Config{DBPath: t.TempDir()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg := app.Config{
		ServiceName:         "测试服务",
		PublicBaseURL:       base,
		InvitePath:          "/api/v1/invite",
		BootstrapAdminEmail: "ops@example.com",
		HMACKey:             []byte("httpapi-test-key"),
		UAHashSalt:          "httpapi-test-salt",
		Now:                 clock.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := app.New(st, &fakeMailer{}, cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	publicCfg := httpapi.PublicConfig{MountPath: "/api/v1"}
	adminCfg := httpapi.AdminConfig{MountPath: "/admin"}
	if mutateHTTP != nil {
		mutateHTTP(&publicCfg, &adminCfg)
	}
	public, err := httpapi.NewPublic(a, publicCfg)
	if err != nil {
		t.Fatalf("NewPublic: %v", err)
	}
	admin, err := httpapi.NewAdmin(a, adminCfg)
	if err != nil {
		t.Fatalf("NewAdmin: %v", err)
	}

	f := fiber.New(fiber.Config{DisableStartupMessage: true})
	public.Register(f.Group("/api/v1"))
	admin.Register(f.Group("/admin"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.Listener(ln)
	}()
	t.Cleanup(func() {
		_ = f.Shutdown()
		<-done
		_ = st.Close()
	})

	return &env{base: base, db: st.DB(), app: a, clock: clock}
}

type response struct {
	status int
	body   map[string]any
	raw    string
	header http.Header
}

func (r response) code() float64 {
	if v, ok := r.body["code"].(float64); ok {
		return v
	}
	return -1
}

func (r response) data() map[string]any {
	if v, ok := r.body["data"].(map[string]any); ok {
		return v
	}
	return nil
}

func (e *env) do(t *testing.T, method, path string, payload any, opts ...func(*http.Request)) response {
	t.Helper()

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(req)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()

	raw, _ := io.ReadAll(res.Body)
	out := response{status: res.StatusCode, raw: string(raw), header: res.Header}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err == nil {
		out.body = parsed
	}
	return out
}

func withToken(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func withHost(host string) func(*http.Request) {
	return func(r *http.Request) { r.Host = host }
}

func withOrigin(origin string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Origin", origin) }
}

// --- 从发件箱取回明文（走真实链路，不依赖测试专用后门） ---

func (e *env) otpCode(t *testing.T, to string) string {
	t.Helper()
	var payload *string
	if err := e.db.Raw(
		`SELECT payload FROM mail_outbox WHERE template = ? AND to_email = ? ORDER BY id DESC LIMIT 1`,
		mail.TemplateOTPCode, to,
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

func (e *env) inviteToken(t *testing.T, to string) string {
	t.Helper()
	var payload *string
	if err := e.db.Raw(
		`SELECT payload FROM mail_outbox WHERE template = ? AND to_email = ? ORDER BY id DESC LIMIT 1`,
		mail.TemplateInvite, to,
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
	u, err := url.Parse(p.InviteURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("token")
}

// adminToken 走完整的 HTTP 登录流程取得管理会话。
func (e *env) adminToken(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	if _, err := e.app.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	if res := e.do(t, http.MethodPost, "/admin/otp/send", map[string]string{"email": "ops@example.com"}); res.status != http.StatusOK {
		t.Fatalf("admin otp send = %d %s", res.status, res.raw)
	}
	code := e.otpCode(t, "ops@example.com")
	res := e.do(t, http.MethodPost, "/admin/otp/verify", map[string]string{
		"email": "ops@example.com", "code": code,
	})
	if res.status != http.StatusOK {
		t.Fatalf("admin otp verify = %d %s", res.status, res.raw)
	}
	token, _ := res.data()["token"].(string)
	if token == "" {
		t.Fatal("admin token missing")
	}
	return token
}

func (e *env) seedActiveUser(t *testing.T, email, uid string) {
	t.Helper()
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com"}
	if _, err := e.app.AdminCreateUser(ctx, actor, email); err != nil {
		t.Fatalf("create user: %v", err)
	}
	token := e.inviteToken(t, email)
	if _, err := e.app.AcceptInvite(ctx, token, uid, "127.0.0.1", "test"); err != nil {
		t.Fatalf("accept invite: %v", err)
	}
}

// --- 公开面：邀请落地页 ---

// GET 落地页必须只读：邮件安全网关预取后邀请仍然可用。
func TestInvitePageIsReadOnlyAndRendersEmail(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if _, err := e.app.AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	token := e.inviteToken(t, "alice@example.com")

	for i := 0; i < 3; i++ {
		res := e.do(t, http.MethodGet, "/api/v1/invite?token="+url.QueryEscape(token), nil)
		if res.status != http.StatusOK {
			t.Fatalf("invite page = %d", res.status)
		}
		if !strings.Contains(res.raw, "alice@example.com") {
			t.Fatalf("page should render the invitee email")
		}
		if res.header.Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", res.header.Get("Cache-Control"))
		}
		if !strings.Contains(res.header.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("CSP missing or weak: %q", res.header.Get("Content-Security-Policy"))
		}
		if res.header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("Referrer-Policy = %q", res.header.Get("Referrer-Policy"))
		}
	}

	// 仍然可以激活（GET 未消费 token）
	res := e.do(t, http.MethodPost, "/api/v1/invite/accept", map[string]string{"token": token, "uid": "alice"})
	if res.status != http.StatusOK || res.code() != 0 {
		t.Fatalf("accept after page prefetch = %d %s", res.status, res.raw)
	}
}

func TestInvitePageWithBadOrMissingToken(t *testing.T) {
	e := newEnv(t, nil)

	res := e.do(t, http.MethodGet, "/api/v1/invite", nil)
	if res.status != http.StatusOK || !strings.Contains(res.raw, "链接不完整") {
		t.Fatalf("missing token page = %d %s", res.status, res.raw)
	}

	res = e.do(t, http.MethodGet, "/api/v1/invite?token=bogus", nil)
	if res.status != http.StatusOK || !strings.Contains(res.raw, "链接无效或已过期") {
		t.Fatalf("bad token page = %d", res.status)
	}
}

func TestInviteAssetsAreServedWithSafeContentTypes(t *testing.T) {
	e := newEnv(t, nil)

	css := e.do(t, http.MethodGet, "/api/v1/invite-assets/style.css", nil)
	if css.status != http.StatusOK || !strings.Contains(css.header.Get("Content-Type"), "text/css") {
		t.Fatalf("css = %d %q", css.status, css.header.Get("Content-Type"))
	}
	js := e.do(t, http.MethodGet, "/api/v1/invite-assets/invite.js", nil)
	if js.status != http.StatusOK || !strings.Contains(js.header.Get("Content-Type"), "javascript") {
		t.Fatalf("js = %d %q", js.status, js.header.Get("Content-Type"))
	}
	// 不存在的资源
	if res := e.do(t, http.MethodGet, "/api/v1/invite-assets/../admin/app.js", nil); res.status == http.StatusOK {
		t.Fatal("unexpected asset served")
	}
}

func TestCheckUIDEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if _, err := e.app.AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	token := e.inviteToken(t, "alice@example.com")

	res := e.do(t, http.MethodPost, "/api/v1/invite/check-uid", map[string]string{"token": token, "uid": "alice"})
	if res.status != http.StatusOK || res.data()["available"] != true {
		t.Fatalf("available check = %d %s", res.status, res.raw)
	}

	// 规则不合规 → 400，并且这是唯一会解释细节的地方
	res = e.do(t, http.MethodPost, "/api/v1/invite/check-uid", map[string]string{"token": token, "uid": "1abc"})
	if res.status != http.StatusBadRequest {
		t.Fatalf("invalid uid = %d %s", res.status, res.raw)
	}
	if !strings.Contains(res.raw, "字母开头") {
		t.Errorf("400 body should explain the rule: %s", res.raw)
	}

	// 无效 token → 410
	res = e.do(t, http.MethodPost, "/api/v1/invite/check-uid", map[string]string{"token": "nope", "uid": "alice"})
	if res.status != http.StatusGone {
		t.Fatalf("bad token = %d %s", res.status, res.raw)
	}
}

func TestAcceptInviteStatusCodes(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	actor := app.Actor{ID: 1, Email: "ops@example.com"}

	if _, err := e.app.AdminCreateUser(ctx, actor, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	aliceToken := e.inviteToken(t, "alice@example.com")
	if _, err := e.app.AdminCreateUser(ctx, actor, "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	bobToken := e.inviteToken(t, "bob@example.com")

	// 冲突 → 409
	if res := e.do(t, http.MethodPost, "/api/v1/invite/accept", map[string]string{"token": bobToken, "uid": "taken"}); res.status != http.StatusOK {
		t.Fatalf("first accept = %d %s", res.status, res.raw)
	}
	res := e.do(t, http.MethodPost, "/api/v1/invite/accept", map[string]string{"token": aliceToken, "uid": "taken"})
	if res.status != http.StatusConflict {
		t.Fatalf("uid conflict = %d %s", res.status, res.raw)
	}
	if strings.Contains(strings.ToLower(res.raw), "unique") || strings.Contains(res.raw, "constraint") {
		t.Fatalf("409 body leaks SQL details: %s", res.raw)
	}

	// 无效 token → 410
	res = e.do(t, http.MethodPost, "/api/v1/invite/accept", map[string]string{"token": "nope", "uid": "alice"})
	if res.status != http.StatusGone {
		t.Fatalf("invalid token = %d %s", res.status, res.raw)
	}

	// 已激活后重复提交 → 410（token 已被消费）
	res = e.do(t, http.MethodPost, "/api/v1/invite/accept", map[string]string{"token": bobToken, "uid": "bob2"})
	if res.status != http.StatusGone {
		t.Fatalf("reuse token = %d %s", res.status, res.raw)
	}
}

// --- 公开面：登录 ---

func TestOTPSendIsUniformForKnownAndUnknownEmail(t *testing.T) {
	e := newEnv(t, nil)
	e.seedActiveUser(t, "alice@example.com", "alice")

	known := e.do(t, http.MethodPost, "/api/v1/otp/send", map[string]string{"email": "alice@example.com"})
	unknown := e.do(t, http.MethodPost, "/api/v1/otp/send", map[string]string{"email": "nobody@example.com"})

	if known.status != http.StatusOK || unknown.status != http.StatusOK {
		t.Fatalf("status known=%d unknown=%d", known.status, unknown.status)
	}
	if known.body["msg"] != unknown.body["msg"] {
		t.Fatalf("messages differ: %q vs %q", known.body["msg"], unknown.body["msg"])
	}
	if known.raw != unknown.raw {
		t.Logf("body differs (allowed if only data shape): known=%s unknown=%s", known.raw, unknown.raw)
	}
	// 未知邮箱不得产生邮件
	var mails int64
	if err := e.db.Raw(`SELECT COUNT(*) FROM mail_outbox WHERE to_email = ?`, "nobody@example.com").
		Row().Scan(&mails); err != nil {
		t.Fatal(err)
	}
	if mails != 0 {
		t.Fatalf("unknown email produced %d mails", mails)
	}
}

func TestOTPVerifyIssuesSessionAndLogoutRevokesIt(t *testing.T) {
	e := newEnv(t, nil)
	e.seedActiveUser(t, "alice@example.com", "alice")

	if res := e.do(t, http.MethodPost, "/api/v1/otp/send", map[string]string{"email": "alice@example.com"}); res.status != http.StatusOK {
		t.Fatalf("send = %d", res.status)
	}
	code := e.otpCode(t, "alice@example.com")

	res := e.do(t, http.MethodPost, "/api/v1/otp/verify", map[string]string{"email": "alice@example.com", "code": code})
	if res.status != http.StatusOK {
		t.Fatalf("verify = %d %s", res.status, res.raw)
	}
	token, _ := res.data()["token"].(string)
	uid, _ := res.data()["uid"].(string)
	if token == "" || uid != "alice" {
		t.Fatalf("verify data = %+v", res.data())
	}

	// 会话校验：必须一并返回期限，否则客户端无法在到期前提示重新验证
	sess := e.do(t, http.MethodPost, "/api/v1/session/verify", map[string]string{"token": token})
	if sess.status != http.StatusOK || sess.data()["uid"] != "alice" || sess.data()["scope"] != "user" {
		t.Fatalf("session verify = %d %s", sess.status, sess.raw)
	}
	for _, key := range []string{"expires_at", "absolute_expires_at", "idle_seconds_left", "absolute_seconds_left"} {
		if _, okField := sess.data()[key]; !okField {
			t.Errorf("会话校验响应缺少 %q：%s", key, sess.raw)
		}
	}
	idleLeft, _ := sess.data()["idle_seconds_left"].(float64)
	absLeft, _ := sess.data()["absolute_seconds_left"].(float64)
	// 用户会话默认 idle 7 天 / absolute 30 天；两个倒计时都应为正且 absolute 不小于 idle
	if idleLeft <= 0 || absLeft <= 0 {
		t.Errorf("倒计时应为正数：idle=%v absolute=%v", idleLeft, absLeft)
	}
	if absLeft < idleLeft {
		t.Errorf("absolute 倒计时应不小于 idle：idle=%v absolute=%v", idleLeft, absLeft)
	}

	// 登出
	if out := e.do(t, http.MethodPost, "/api/v1/session/logout", map[string]string{"token": token}); out.status != http.StatusOK {
		t.Fatalf("logout = %d", out.status)
	}
	if after := e.do(t, http.MethodPost, "/api/v1/session/verify", map[string]string{"token": token}); after.status != http.StatusUnauthorized {
		t.Fatalf("verify after logout = %d %s", after.status, after.raw)
	}
}

func TestOTPVerifyWrongCodeReturns401(t *testing.T) {
	e := newEnv(t, nil)
	e.seedActiveUser(t, "alice@example.com", "alice")
	if res := e.do(t, http.MethodPost, "/api/v1/otp/send", map[string]string{"email": "alice@example.com"}); res.status != http.StatusOK {
		t.Fatal("send failed")
	}

	res := e.do(t, http.MethodPost, "/api/v1/otp/verify", map[string]string{"email": "alice@example.com", "code": "000000"})
	if res.status != http.StatusUnauthorized {
		t.Fatalf("wrong code = %d %s", res.status, res.raw)
	}
	if strings.Contains(res.raw, "sql") || strings.Contains(res.raw, "no such") {
		t.Fatalf("response leaks internals: %s", res.raw)
	}
}

func TestRateLimitReturns429WithRetryAfter(t *testing.T) {
	e := newEnv(t, func(c *app.Config) {
		c.RateLimit.EmailPerHour = 1
		c.RateLimit.EmailPerDay = 1
	})
	e.seedActiveUser(t, "alice@example.com", "alice")

	if res := e.do(t, http.MethodPost, "/api/v1/otp/send", map[string]string{"email": "alice@example.com"}); res.status != http.StatusOK {
		t.Fatalf("first send = %d", res.status)
	}
	res := e.do(t, http.MethodPost, "/api/v1/otp/send", map[string]string{"email": "alice@example.com"})
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("second send = %d %s", res.status, res.raw)
	}
	if res.header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
}

// --- 管理面 ---

// Host 头不在白名单时必须拒绝（DNS rebinding 防线）。真实连接来自 127.0.0.1，
// 因此这里验证的正是 Host 白名单这一层。
func TestAdminRejectsForeignHostHeader(t *testing.T) {
	e := newEnv(t, nil)
	_ = e.adminToken(t)

	res := e.do(t, http.MethodGet, "/admin/", nil, withHost("evil.example.com"))
	if res.status != http.StatusForbidden {
		t.Fatalf("foreign Host = %d, want 403", res.status)
	}
	res = e.do(t, http.MethodGet, "/admin/users", nil, withHost("evil.example.com"), withToken("whatever"))
	if res.status != http.StatusForbidden {
		t.Fatalf("foreign Host on API = %d, want 403", res.status)
	}
	// 正常 Host 通过
	res = e.do(t, http.MethodGet, "/admin/", nil)
	if res.status != http.StatusOK {
		t.Fatalf("loopback Host = %d, want 200", res.status)
	}
}

func TestAdminRejectsCrossOriginRequests(t *testing.T) {
	e := newEnv(t, nil)
	token := e.adminToken(t)

	res := e.do(t, http.MethodGet, "/admin/users", nil, withOrigin("http://evil.example.com"), withToken(token))
	if res.status != http.StatusForbidden {
		t.Fatalf("cross-origin = %d, want 403", res.status)
	}

	// 同源（Origin == Host）放行
	sameOrigin := "http://" + strings.TrimPrefix(e.base, "http://")
	res = e.do(t, http.MethodGet, "/admin/users", nil, withOrigin(sameOrigin), withToken(token))
	if res.status != http.StatusOK {
		t.Fatalf("same-origin = %d, want 200 (%s)", res.status, res.raw)
	}
}

func TestAdminRequiresBearerAndAdminScope(t *testing.T) {
	e := newEnv(t, nil)
	adminToken := e.adminToken(t)

	// 无令牌
	if res := e.do(t, http.MethodGet, "/admin/users", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", res.status)
	}
	// 伪造令牌
	if res := e.do(t, http.MethodGet, "/admin/users", nil, withToken("forged")); res.status != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", res.status)
	}

	// 普通用户令牌不得访问管理接口
	e.seedActiveUser(t, "alice@example.com", "alice")
	if res := e.do(t, http.MethodPost, "/api/v1/otp/send", map[string]string{"email": "alice@example.com"}); res.status != http.StatusOK {
		t.Fatal("user otp send failed")
	}
	userCode := e.otpCode(t, "alice@example.com")
	res := e.do(t, http.MethodPost, "/api/v1/otp/verify", map[string]string{"email": "alice@example.com", "code": userCode})
	if res.status != http.StatusOK {
		t.Fatalf("user login = %d", res.status)
	}
	userToken, _ := res.data()["token"].(string)

	got := e.do(t, http.MethodGet, "/admin/users", nil, withToken(userToken))
	if got.status != http.StatusForbidden {
		t.Fatalf("user-scope token = %d, want 403", got.status)
	}

	// 管理令牌正常
	ok := e.do(t, http.MethodGet, "/admin/users", nil, withToken(adminToken))
	if ok.status != http.StatusOK || ok.code() != 0 {
		t.Fatalf("admin token = %d %s", ok.status, ok.raw)
	}
}

func TestAdminUserLifecycleOverHTTP(t *testing.T) {
	e := newEnv(t, nil)
	token := e.adminToken(t)
	auth := withToken(token)

	// 建用户
	created := e.do(t, http.MethodPost, "/admin/users", map[string]string{"email": "alice@example.com"}, auth)
	if created.status != http.StatusOK {
		t.Fatalf("create = %d %s", created.status, created.raw)
	}
	inviteURL, _ := created.data()["invite_url"].(string)
	if !strings.Contains(inviteURL, "/api/v1/invite?token=") {
		t.Fatalf("invite_url = %q", inviteURL)
	}
	userID := int64(created.data()["user"].(map[string]any)["id"].(float64))

	// 列表（含 bootstrap 出来的管理员，因此按邮箱定位目标用户）
	list := e.do(t, http.MethodGet, "/admin/users", nil, auth)
	users, _ := list.data()["users"].([]any)
	var alice map[string]any
	for _, item := range users {
		u, _ := item.(map[string]any)
		if u["email"] == "alice@example.com" {
			alice = u
		}
	}
	if alice == nil {
		t.Fatalf("alice not found in user list: %s", list.raw)
	}
	if alice["status"] != "invited" {
		t.Fatalf("status = %v, want invited", alice["status"])
	}
	if alice["uid"] != "" {
		t.Fatalf("uid = %v, want empty before activation", alice["uid"])
	}

	// 重复邮箱 → 409
	dup := e.do(t, http.MethodPost, "/admin/users", map[string]string{"email": "alice@example.com"}, auth)
	if dup.status != http.StatusConflict {
		t.Fatalf("duplicate = %d %s", dup.status, dup.raw)
	}

	// 邮箱非法 → 400
	bad := e.do(t, http.MethodPost, "/admin/users", map[string]string{"email": "not-an-email"}, auth)
	if bad.status != http.StatusBadRequest {
		t.Fatalf("bad email = %d %s", bad.status, bad.raw)
	}

	// 重新生成链接（不发邮件）
	before := countMails(t, e.db)
	regen := e.do(t, http.MethodPost, "/admin/users/"+itoa64(userID)+"/invite/link", nil, auth)
	if regen.status != http.StatusOK {
		t.Fatalf("regenerate = %d %s", regen.status, regen.raw)
	}
	if countMails(t, e.db) != before {
		t.Fatal("regenerate must not queue mail")
	}

	// 停用 → 会话被吊销、状态变化
	if res := e.do(t, http.MethodPost, "/admin/users/"+itoa64(userID)+"/disable", nil, auth); res.status != http.StatusOK {
		t.Fatalf("disable = %d %s", res.status, res.raw)
	}
	if res := e.do(t, http.MethodPost, "/admin/users/"+itoa64(userID)+"/enable", nil, auth); res.status != http.StatusOK {
		t.Fatalf("enable = %d", res.status)
	}

	// 改邮箱
	if res := e.do(t, http.MethodPost, "/admin/users/"+itoa64(userID)+"/email",
		map[string]string{"email": "new@example.com"}, auth); res.status != http.StatusOK {
		t.Fatalf("change email = %d %s", res.status, res.raw)
	}

	// 审计里出现相应动作
	audit := e.do(t, http.MethodGet, "/admin/audit", nil, auth)
	entries, _ := audit.data()["entries"].([]any)
	if len(entries) < 4 {
		t.Fatalf("audit entries = %d, want >= 4", len(entries))
	}
	for _, e := range entries {
		if _, hasDetail := e.(map[string]any)["detail"]; hasDetail {
			t.Fatal("audit API must not expose detail")
		}
	}

	// 删除
	if res := e.do(t, http.MethodDelete, "/admin/users/"+itoa64(userID), nil, auth); res.status != http.StatusOK {
		t.Fatalf("delete = %d %s", res.status, res.raw)
	}
	if res := e.do(t, http.MethodGet, "/admin/users/"+itoa64(userID)+"/sessions", nil, auth); res.status != http.StatusNotFound {
		t.Fatalf("sessions after delete = %d, want 404", res.status)
	}
}

func TestAdminCannotDisableLastAdmin(t *testing.T) {
	e := newEnv(t, nil)
	token := e.adminToken(t)
	auth := withToken(token)

	list := e.do(t, http.MethodGet, "/admin/users", nil, auth)
	users := list.data()["users"].([]any)
	adminID := int64(users[0].(map[string]any)["id"].(float64))

	res := e.do(t, http.MethodPost, "/admin/users/"+itoa64(adminID)+"/disable", nil, auth)
	if res.status != http.StatusForbidden {
		t.Fatalf("disable last admin = %d, want 403 (%s)", res.status, res.raw)
	}
}

func TestAdminPageHeadersAndAssets(t *testing.T) {
	e := newEnv(t, nil)

	page := e.do(t, http.MethodGet, "/admin/", nil)
	if page.status != http.StatusOK {
		t.Fatalf("admin page = %d", page.status)
	}
	csp := page.header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("admin CSP = %q", csp)
	}
	if page.header.Get("Cache-Control") != "no-store" {
		t.Errorf("admin page Cache-Control = %q", page.header.Get("Cache-Control"))
	}
	// 页面里必须注入正确的 API 前缀
	if !strings.Contains(page.raw, `data-api="/admin"`) {
		t.Error("admin page did not inject the API prefix")
	}

	js := e.do(t, http.MethodGet, "/admin/assets/app.js", nil)
	if js.status != http.StatusOK || !strings.Contains(js.header.Get("Content-Type"), "javascript") {
		t.Fatalf("admin js = %d %q", js.status, js.header.Get("Content-Type"))
	}
	css := e.do(t, http.MethodGet, "/admin/assets/style.css", nil)
	if css.status != http.StatusOK || !strings.Contains(css.header.Get("Content-Type"), "text/css") {
		t.Fatalf("admin css = %d %q", css.status, css.header.Get("Content-Type"))
	}
	if res := e.do(t, http.MethodGet, "/admin/assets/nope.js", nil); res.status != http.StatusNotFound {
		t.Fatalf("unknown asset = %d, want 404", res.status)
	}
}

func TestAdminLogoutInvalidatesToken(t *testing.T) {
	e := newEnv(t, nil)
	token := e.adminToken(t)

	if res := e.do(t, http.MethodPost, "/admin/logout", nil, withToken(token)); res.status != http.StatusOK {
		t.Fatalf("logout = %d", res.status)
	}
	if res := e.do(t, http.MethodGet, "/admin/users", nil, withToken(token)); res.status != http.StatusUnauthorized {
		t.Fatalf("after logout = %d, want 401", res.status)
	}
}

func TestAdminStatsEndpoint(t *testing.T) {
	e := newEnv(t, nil)
	token := e.adminToken(t)

	res := e.do(t, http.MethodGet, "/admin/stats", nil, withToken(token))
	if res.status != http.StatusOK {
		t.Fatalf("stats = %d %s", res.status, res.raw)
	}
	data := res.data()
	if data["admins"] == nil || data["users_by_status"] == nil || data["queue"] == nil {
		t.Fatalf("stats payload incomplete: %s", res.raw)
	}
	if data["mails_last_24h"] == nil {
		t.Fatal("mails_last_24h missing")
	}
}

func TestPublicRequiresJSONBody(t *testing.T) {
	e := newEnv(t, nil)
	req, err := http.NewRequest(http.MethodPost, e.base+"/api/v1/otp/send", strings.NewReader("not-json"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-JSON body = %d, want 400", res.StatusCode)
	}
}

// --- helpers ---

func countMails(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Raw(`SELECT COUNT(*) FROM mail_outbox`).Row().Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// 管理动作端到端：重发邀请 → 激活 → 用户登录 → 管理员踢下线 → 用户令牌立即失效
func TestAdminResendInviteAndRevokeSessionsOverHTTP(t *testing.T) {
	e := newEnv(t, nil)
	token := e.adminToken(t)
	auth := withToken(token)

	created := e.do(t, http.MethodPost, "/admin/users", map[string]string{"email": "alice@example.com"}, auth)
	if created.status != http.StatusOK {
		t.Fatalf("create = %d %s", created.status, created.raw)
	}
	userID := int64(created.data()["user"].(map[string]any)["id"].(float64))

	// 重发邀请：作废旧链接并发新邮件
	before := countMails(t, e.db)
	resend := e.do(t, http.MethodPost, "/admin/users/"+itoa64(userID)+"/invite/resend", nil, auth)
	if resend.status != http.StatusOK {
		t.Fatalf("resend = %d %s", resend.status, resend.raw)
	}
	if countMails(t, e.db) != before+1 {
		t.Fatal("resend should queue exactly one mail")
	}
	if resend.data()["invite_url"] == nil {
		t.Fatal("resend should return the fresh link for manual delivery")
	}

	// 激活 + 用户登录
	inviteTokenValue := e.inviteToken(t, "alice@example.com")
	if res := e.do(t, http.MethodPost, "/api/v1/invite/accept",
		map[string]string{"token": inviteTokenValue, "uid": "alice"}); res.status != http.StatusOK {
		t.Fatalf("accept = %d %s", res.status, res.raw)
	}
	if res := e.do(t, http.MethodPost, "/api/v1/otp/send", map[string]string{"email": "alice@example.com"}); res.status != http.StatusOK {
		t.Fatal("otp send failed")
	}
	code := e.otpCode(t, "alice@example.com")
	login := e.do(t, http.MethodPost, "/api/v1/otp/verify", map[string]string{"email": "alice@example.com", "code": code})
	if login.status != http.StatusOK {
		t.Fatalf("login = %d %s", login.status, login.raw)
	}
	userToken, _ := login.data()["token"].(string)

	// 踢下线
	revoke := e.do(t, http.MethodPost, "/admin/users/"+itoa64(userID)+"/sessions/revoke", nil, auth)
	if revoke.status != http.StatusOK {
		t.Fatalf("revoke = %d %s", revoke.status, revoke.raw)
	}
	if n, _ := revoke.data()["revoked"].(float64); n < 1 {
		t.Fatalf("revoked = %v, want >= 1", revoke.data()["revoked"])
	}

	// 被踢后用户令牌立即失效
	after := e.do(t, http.MethodPost, "/api/v1/session/verify", map[string]string{"token": userToken})
	if after.status != http.StatusUnauthorized {
		t.Fatalf("session after kick = %d, want 401", after.status)
	}
	// 管理令牌不受影响
	if res := e.do(t, http.MethodGet, "/admin/users", nil, auth); res.status != http.StatusOK {
		t.Fatalf("admin token should survive: %d", res.status)
	}
}

// 管理员的"设置用户名"链接与管理员的普通邀请共用落地页，但文案必须不同：
// 账号已经可用，就不该再让人以为要"激活"。
func TestInviteLandingPageCopyForAdmin(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()

	// 触发引导：管理员被创建并收到"设置用户名"链接
	if _, err := e.app.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	adminToken := e.inviteToken(t, "ops@example.com")

	res := e.do(t, http.MethodGet, "/api/v1/invite?token="+adminToken, nil)
	if res.status != http.StatusOK {
		t.Fatalf("状态码 = %d", res.status)
	}
	for _, want := range []string{"设置用户名", "保存用户名", "账号已可使用"} {
		if !strings.Contains(res.raw, want) {
			t.Errorf("管理员落地页应包含 %q", want)
		}
	}
	if strings.Contains(res.raw, "激活账号") {
		t.Error("管理员账号已是 active，不应出现「激活账号」")
	}
	if leftover := placeholderRe.FindAllString(res.raw, -1); len(leftover) > 0 {
		t.Errorf("残留占位符 %v", leftover)
	}

	// 普通受邀用户仍然是"激活"文案
	if _, err := e.app.AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	userToken := e.inviteToken(t, "alice@example.com")
	page := e.do(t, http.MethodGet, "/api/v1/invite?token="+userToken, nil)
	for _, want := range []string{"账号激活", "激活账号"} {
		if !strings.Contains(page.raw, want) {
			t.Errorf("受邀用户落地页应包含 %q", want)
		}
	}

	// 链接不可用时也要把全部占位符填满（否则页面会留下空白）
	bad := e.do(t, http.MethodGet, "/api/v1/invite?token=bogus", nil)
	if leftover := placeholderRe.FindAllString(bad.raw, -1); len(leftover) > 0 {
		t.Errorf("失效链接页面残留占位符 %v", leftover)
	}
}
