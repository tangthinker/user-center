package mail_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/mail"
)

func newRenderer(t *testing.T) *mail.DefaultRenderer {
	t.Helper()
	// 固定 UTC：这些用例断言的是文案本身，不该随跑测试的机器的本地时区变化。
	return newRendererIn(t, "UTC")
}

func newRendererIn(t *testing.T, tz string) *mail.DefaultRenderer {
	t.Helper()
	r, err := mail.NewRenderer(mail.RendererConfig{
		ServiceName:   "云盘",
		PublicBaseURL: "https://files.example.com",
		SupportEmail:  "ops@example.com",
		TimeZone:      tz,
	})
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	return r
}

func TestRendererRejectsBadConfig(t *testing.T) {
	if _, err := mail.NewRenderer(mail.RendererConfig{}); err == nil {
		t.Fatal("expected error for missing ServiceName/PublicBaseURL")
	}
	if _, err := mail.NewRenderer(mail.RendererConfig{ServiceName: "x", PublicBaseURL: "y", SupportEmail: "bad"}); err == nil {
		t.Fatal("expected error for invalid SupportEmail")
	}
	// 时区名写错要在构造时就失败：拖着只会让用户收到一封时间诡异的邮件。
	if _, err := mail.NewRenderer(mail.RendererConfig{
		ServiceName: "x", PublicBaseURL: "y", TimeZone: "Beijing/Chaoyang",
	}); err == nil {
		t.Fatal("expected error for unknown TimeZone")
	}
}

// 时区展示：邮件里的时间必须是"收件人当地的钟点 + 明确偏移"，
// 而不是需要自己换算的 UTC（北京中午 12:13 曾被渲染成 04:13 UTC）。
func TestAdminLoginNoticeRendersTimeInConfiguredZone(t *testing.T) {
	// 2026-10-04 04:13:12 UTC == 北京时间当天 12:13:12
	at := time.Date(2026, 10, 4, 4, 13, 12, 0, time.UTC)
	payload, err := mail.EncodePayload(mail.AdminLoginPayload{IP: "127.0.0.1", UA: "UA", At: at})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		tz   string
		want string
	}{
		{"Asia/Shanghai", "2026-10-04 12:13:12 +08:00"},
		{"UTC", "2026-10-04 04:13:12 +00:00"},
	}
	for _, c := range cases {
		msg, err := newRendererIn(t, c.tz).Build(mail.TemplateAdminLoginNotice, payload)
		if err != nil {
			t.Fatalf("%s: Build: %v", c.tz, err)
		}
		if !strings.Contains(msg.Text, c.want) {
			t.Errorf("%s: time line = %q, want it to contain %q", c.tz, msg.Text, c.want)
		}
	}
}

// 夏令时：必须按"那个时刻"的偏移渲染，而不是拿一个固定偏移糊上去。
func TestHumanTimeFollowsDST(t *testing.T) {
	r := newRendererIn(t, "America/New_York")
	build := func(at time.Time) string {
		t.Helper()
		payload, err := mail.EncodePayload(mail.AdminActionPayload{Action: "disable_user", At: at})
		if err != nil {
			t.Fatal(err)
		}
		msg, err := r.Build(mail.TemplateAdminActionNotice, payload)
		if err != nil {
			t.Fatal(err)
		}
		return msg.Text
	}
	if got := build(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)); !strings.Contains(got, "2026-01-15 07:00:00 -05:00") {
		t.Errorf("winter time = %q, want 07:00 -05:00", got)
	}
	if got := build(time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)); !strings.Contains(got, "2026-07-15 08:00:00 -04:00") {
		t.Errorf("summer time = %q, want 08:00 -04:00", got)
	}
}

// 邀请失效时间走同一套规则；留空表示跟随宿主进程的本地时区。
func TestInviteDeadlineUsesConfiguredZoneAndDefaultsToLocal(t *testing.T) {
	expires := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	payload, err := mail.EncodePayload(mail.InvitePayload{InviteURL: "https://files.example.com/i?token=t", ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}

	msg, err := newRendererIn(t, "Asia/Shanghai").Build(mail.TemplateInvite, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Text, "2026-03-01 20:00:00 +08:00") {
		t.Errorf("invite expiry = %q, want 20:00:00 +08:00", msg.Text)
	}

	if loc := newRendererIn(t, "").Location(); loc != time.Local {
		t.Errorf("empty TimeZone should follow the host's local zone, got %v", loc)
	}
	// 零值时间仍然是"未知"，不能渲染成 0001-01-01
	zero, err := mail.EncodePayload(mail.InvitePayload{InviteURL: "https://x/y"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err = newRendererIn(t, "Asia/Shanghai").Build(mail.TemplateInvite, zero)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg.Text, "0001-") {
		t.Errorf("zero expiry should not render a date: %q", msg.Text)
	}
}

func TestInviteURLUsesPublicBaseAndEscapesToken(t *testing.T) {
	r := newRenderer(t)
	got := r.InviteURL("a b/c+d")
	want := "https://files.example.com/api/v1/invite?token=a+b%2Fc%2Bd"
	if got != want {
		t.Fatalf("InviteURL = %q, want %q", got, want)
	}
	// 结尾斜杠不应产生双斜杠
	r2, _ := mail.NewRenderer(mail.RendererConfig{ServiceName: "x", PublicBaseURL: "https://x.example.com/"})
	if got := r2.InviteURL("t"); strings.Contains(got, "//api") {
		t.Fatalf("unexpected double slash: %q", got)
	}
}

// 安全要求：验证码**绝不能**出现在主题里（主题会进通知栏与列表预览）
func TestOTPCodeNeverAppearsInSubject(t *testing.T) {
	r := newRenderer(t)
	payload, err := mail.EncodePayload(mail.OTPPayload{Code: "482915", TTLMinutes: 10})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := r.Build(mail.TemplateOTPCode, payload)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(msg.Subject, "482915") {
		t.Fatalf("验证码出现在主题中: %q", msg.Subject)
	}
	if !strings.Contains(msg.Text, "482915") {
		t.Fatal("code missing from body")
	}
	if !strings.Contains(msg.Text, "10 分钟") {
		t.Errorf("TTL not rendered: %q", msg.Text)
	}
	if !strings.Contains(msg.Subject, "云盘") {
		t.Errorf("subject should carry the service name: %q", msg.Subject)
	}
}

func TestInviteTemplateRendersLinkAndExpiry(t *testing.T) {
	r := newRenderer(t)
	expires := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	payload, err := mail.EncodePayload(mail.InvitePayload{
		InviteURL: r.InviteURL("tok123"), ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := r.Build(mail.TemplateInvite, payload)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(msg.Text, "https://files.example.com/api/v1/invite?token=tok123") {
		t.Fatalf("invite link missing: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "2026-03-01") {
		t.Errorf("expiry missing: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "忽略") {
		t.Error("invite mail must tell the recipient what to do if unexpected")
	}
	if msg.HTML == "" {
		t.Error("invite mail should provide an HTML alternative")
	}
	if strings.Contains(msg.HTML, "http://") {
		t.Error("HTML must not reference external resources")
	}
}

func TestEmailChangedNoticeGoesToOldAddressAndMentionsSupport(t *testing.T) {
	r := newRenderer(t)
	payload, _ := mail.EncodePayload(mail.EmailChangedPayload{OldEmail: "old@example.com"})
	msg, err := r.Build(mail.TemplateEmailChangedNotice, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Text, "old@example.com") {
		t.Error("notice should reference the previous address")
	}
	if !strings.Contains(msg.Text, "ops@example.com") {
		t.Error("notice should tell the user how to reach support")
	}
	if !strings.Contains(msg.Subject, "安全") {
		t.Errorf("subject should flag this as a security notice: %q", msg.Subject)
	}
}

func TestBuildRejectsBadPayloads(t *testing.T) {
	r := newRenderer(t)

	cases := []struct {
		name     string
		template string
		payload  []byte
	}{
		{"otp without code", mail.TemplateOTPCode, []byte(`{"ttl_minutes":10}`)},
		{"otp without payload", mail.TemplateOTPCode, nil},
		{"invite without url", mail.TemplateInvite, []byte(`{"expires_at":"2026-01-01T00:00:00Z"}`)},
		{"malformed json", mail.TemplateInvite, []byte(`{`)},
		{"unknown template", "no_such_template", []byte(`{}`)},
	}
	for _, c := range cases {
		if _, err := r.Build(c.template, c.payload); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}

func TestBuildRFC822BasicHeaders(t *testing.T) {
	raw, err := mail.BuildRFC822("noreply@example.com", "云盘", mail.Message{
		To:      "alice@example.com",
		Subject: "登录验证码",
		Text:    "你的验证码是 123456",
	})
	if err != nil {
		t.Fatalf("BuildRFC822: %v", err)
	}
	s := string(raw)

	for _, want := range []string{
		"From: ", "To: alice@example.com", "Subject: ", "Date: ",
		"Message-ID: <", "MIME-Version: 1.0",
		`Content-Type: text/plain; charset="utf-8"`,
		"Auto-Submitted: auto-generated",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing header %q\n%s", want, s)
		}
	}
	if !strings.Contains(s, "=?utf-8?") {
		t.Error("non-ASCII subject/display name should be RFC 2047 encoded")
	}
	if !strings.Contains(s, "\r\n") {
		t.Error("CRLF line endings required")
	}
	if strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\n") {
		t.Error("bare LF found; all line endings must be CRLF")
	}
}

// 头部注入防护：To / Subject 中的 CRLF 必须被拒绝
func TestBuildRFC822RejectsHeaderInjection(t *testing.T) {
	cases := []struct {
		name string
		msg  mail.Message
	}{
		{"CRLF in To", mail.Message{To: "a@example.com\r\nBcc: evil@example.com", Subject: "s", Text: "t"}},
		{"LF in To", mail.Message{To: "a@example.com\nBcc: evil@example.com", Subject: "s", Text: "t"}},
		{"CRLF in Subject", mail.Message{To: "a@example.com", Subject: "hi\r\nX-Evil: 1", Text: "t"}},
	}
	for _, c := range cases {
		if _, err := mail.BuildRFC822("noreply@example.com", "", c.msg); err == nil {
			t.Errorf("%s: expected rejection", c.name)
		}
	}
}

func TestBuildRFC822RejectsInvalidAddresses(t *testing.T) {
	if _, err := mail.BuildRFC822("not-an-email", "", mail.Message{To: "a@example.com", Subject: "s"}); err == nil {
		t.Error("invalid From must be rejected")
	}
	if _, err := mail.BuildRFC822("noreply@example.com", "", mail.Message{To: "bad", Subject: "s"}); err == nil {
		t.Error("invalid To must be rejected")
	}
	if _, err := mail.BuildRFC822("noreply@example.com", "", mail.Message{To: "a@example.com"}); err == nil {
		t.Error("empty Subject must be rejected")
	}
}

func TestBuildRFC822MultipartWhenHTMLPresent(t *testing.T) {
	raw, err := mail.BuildRFC822("noreply@example.com", "", mail.Message{
		To: "a@example.com", Subject: "s", Text: "plain", HTML: "<p>html</p>",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "multipart/alternative") {
		t.Error("HTML alternative should produce multipart/alternative")
	}
	if !strings.Contains(s, "text/plain") || !strings.Contains(s, "text/html") {
		t.Error("both parts are required")
	}
	if !strings.HasSuffix(s, "--\r\n") {
		t.Error("multipart body must end with the closing boundary")
	}
}

func TestValidateAddress(t *testing.T) {
	valid := []string{"a@example.com", "first.last+tag@sub.example.co", "x@y.io"}
	for _, addr := range valid {
		if err := mail.ValidateAddress(addr); err != nil {
			t.Errorf("ValidateAddress(%q) = %v, want nil", addr, err)
		}
	}

	invalid := []string{
		"", "no-at-sign", "@example.com", "a@", "a@@b.com",
		"a@localhost", "a b@example.com", "a@exam ple.com",
		"a@example.com\r\nBcc: x@y.com", "a@.com", "a@com.", "a@ex..com",
		"a<b@example.com", "a@exa\"mple.com",
	}
	for _, addr := range invalid {
		if err := mail.ValidateAddress(addr); err == nil {
			t.Errorf("ValidateAddress(%q) = nil, want error", addr)
		}
	}
}

func TestSMTPConfigValidation(t *testing.T) {
	if _, err := mail.NewSMTP(mail.SMTPConfig{}); err == nil {
		t.Error("missing Host must be rejected")
	}
	if _, err := mail.NewSMTP(mail.SMTPConfig{Host: "smtp.example.com"}); err == nil {
		t.Error("missing From must be rejected")
	}
	if _, err := mail.NewSMTP(mail.SMTPConfig{
		Host: "smtp.example.com", From: "bad-address",
	}); err == nil {
		t.Error("invalid From must be rejected")
	}
	if _, err := mail.NewSMTP(mail.SMTPConfig{
		Host: "smtp.example.com", From: "noreply@example.com", Username: "u",
	}); err == nil {
		t.Error("Username without Password must be rejected")
	}

	m, err := mail.NewSMTP(mail.SMTPConfig{
		Host: "smtp.qiye.aliyun.com", Port: 465, ImplicitTLS: true,
		Username: "noreply@example.com", Password: "secret",
		From: "noreply@example.com", FromName: "云盘",
	})
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if m.From() != "noreply@example.com" {
		t.Fatalf("From() = %q", m.From())
	}
}

// Verify 在无法连接时必须返回错误（而不是 panic 或静默成功）。
func TestSMTPVerifyFailsOnUnreachableServer(t *testing.T) {
	m, err := mail.NewSMTP(mail.SMTPConfig{
		Host: "127.0.0.1", Port: 1, ImplicitTLS: true,
		From: "noreply@example.com", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	if err := m.Verify(context.Background()); err == nil {
		t.Fatal("Verify against a closed port must fail")
	}
}
