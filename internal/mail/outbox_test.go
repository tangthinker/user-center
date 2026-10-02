package mail_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/mail"
	"github.com/tangthinker/user-center/v2/internal/testsupport"
	"gorm.io/gorm"
)

// fakeMailer 记录发送内容，并可按脚本失败。
type fakeMailer struct {
	mu        sync.Mutex
	sent      []mail.Message
	failTimes int   // 还需失败几次
	failErr   error // 失败时返回的错误，nil 表示普通错误
}

func (f *fakeMailer) Send(_ context.Context, msg mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failTimes > 0 {
		f.failTimes--
		if f.failErr != nil {
			return f.failErr
		}
		return errors.New("smtp: temporary failure")
	}
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeMailer) messages() []mail.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mail.Message(nil), f.sent...)
}

func newOutbox(t *testing.T, mailer mail.Mailer, cfg mail.OutboxConfig, hooks mail.Hooks) (*mail.Outbox, *gorm.DB, *testsupport.Clock) {
	t.Helper()
	db := testsupport.OpenDB(t)
	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ob, err := mail.NewOutbox(db, mailer, newRenderer(t), cfg, hooks)
	if err != nil {
		t.Fatalf("NewOutbox: %v", err)
	}
	return ob.WithClock(clock.Now), db, clock
}

func inviteParams(t *testing.T, key, to string) mail.EnqueueParams {
	t.Helper()
	payload, err := mail.EncodePayload(mail.InvitePayload{
		InviteURL: "https://files.example.com/api/v1/invite?token=abc",
		ExpiresAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return mail.EnqueueParams{DedupeKey: key, To: to, Template: mail.TemplateInvite, Payload: payload}
}

func statusOf(t *testing.T, db *gorm.DB, dedupeKey string) (string, int, *time.Time) {
	t.Helper()
	var (
		status   string
		attempts int
		next     time.Time
	)
	if err := db.Raw(
		`SELECT status, attempts, next_attempt_at FROM mail_outbox WHERE dedupe_key = ?`, dedupeKey,
	).Row().Scan(&status, &attempts, &next); err != nil {
		t.Fatalf("status query: %v", err)
	}
	return status, attempts, &next
}

func TestEnqueueAndDeliver(t *testing.T) {
	m := &fakeMailer{}
	ob, db, _ := newOutbox(t, m, mail.OutboxConfig{}, mail.Hooks{})
	ctx := context.Background()

	if err := ob.Enqueue(ctx, inviteParams(t, "invite:1", "alice@example.com")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	got, err := ob.ProcessOnce(ctx)
	if err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	if got.Sent != 1 || got.Retried != 0 || got.Failed != 0 {
		t.Fatalf("processed = %+v, want 1 sent", got)
	}

	msgs := m.messages()
	if len(msgs) != 1 {
		t.Fatalf("mailer received %d messages, want 1", len(msgs))
	}
	if msgs[0].To != "alice@example.com" {
		t.Errorf("To = %q", msgs[0].To)
	}
	if !contains(msgs[0].Text, "https://files.example.com/api/v1/invite?token=abc") {
		t.Errorf("body missing invite link: %q", msgs[0].Text)
	}

	status, attempts, _ := statusOf(t, db, "invite:1")
	if status != domain.MailSent || attempts != 1 {
		t.Fatalf("row = %s/%d, want sent/1", status, attempts)
	}
}

// 幂等：同一 dedupe_key 重复入队只留一行
func TestEnqueueIsIdempotent(t *testing.T) {
	m := &fakeMailer{}
	ob, db, _ := newOutbox(t, m, mail.OutboxConfig{}, mail.Hooks{})
	ctx := context.Background()

	if err := ob.Enqueue(ctx, inviteParams(t, "otp:42", "a@example.com")); err != nil {
		t.Fatal(err)
	}
	err := ob.Enqueue(ctx, inviteParams(t, "otp:42", "a@example.com"))
	if !errors.Is(err, mail.ErrDuplicate) {
		t.Fatalf("second enqueue = %v, want ErrDuplicate", err)
	}

	var rows int64
	if err := db.Raw(`SELECT COUNT(*) FROM mail_outbox`).Row().Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}

	// 只会发一封
	if _, err := ob.ProcessOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(m.messages()); n != 1 {
		t.Fatalf("sent %d messages, want 1", n)
	}
}

func TestEnqueueValidation(t *testing.T) {
	ob, _, _ := newOutbox(t, &fakeMailer{}, mail.OutboxConfig{}, mail.Hooks{})
	ctx := context.Background()

	cases := []struct {
		name string
		p    mail.EnqueueParams
	}{
		{"missing key", mail.EnqueueParams{To: "a@example.com", Template: mail.TemplateOTPCode}},
		{"missing template", mail.EnqueueParams{DedupeKey: "k", To: "a@example.com"}},
		{"invalid address", mail.EnqueueParams{DedupeKey: "k2", To: "not-an-email", Template: mail.TemplateOTPCode}},
	}
	for _, c := range cases {
		if err := ob.Enqueue(ctx, c.p); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}

// 失败 → 退避重试 → 恢复后成功
func TestRetryWithBackoff(t *testing.T) {
	m := &fakeMailer{failTimes: 1}
	ob, db, clock := newOutbox(t, m, mail.OutboxConfig{
		MaxAttempts: 5,
		Backoff:     []time.Duration{time.Minute, 5 * time.Minute},
	}, mail.Hooks{})
	ctx := context.Background()

	if err := ob.Enqueue(ctx, inviteParams(t, "invite:2", "a@example.com")); err != nil {
		t.Fatal(err)
	}

	got, err := ob.ProcessOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Retried != 1 {
		t.Fatalf("processed = %+v, want 1 retried", got)
	}

	status, attempts, next := statusOf(t, db, "invite:2")
	if status != domain.MailPending || attempts != 1 {
		t.Fatalf("row = %s/%d, want pending/1", status, attempts)
	}
	if !next.Equal(clock.Now().Add(time.Minute)) {
		t.Fatalf("next_attempt_at = %v, want now+1m", next)
	}

	// 未到时间：不应被取出
	got, err = ob.ProcessOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sent+got.Retried+got.Failed != 0 {
		t.Fatalf("mail should not be retried before next_attempt_at: %+v", got)
	}

	// 到时间：重试成功
	clock.Advance(61 * time.Second)
	got, err = ob.ProcessOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sent != 1 {
		t.Fatalf("processed = %+v, want 1 sent", got)
	}
	status, attempts, _ = statusOf(t, db, "invite:2")
	if status != domain.MailSent || attempts != 2 {
		t.Fatalf("row = %s/%d, want sent/2", status, attempts)
	}
}

// 达到最大次数后进入 failed，并回调 OnFailed(final=true)
func TestMaxAttemptsMarksFailed(t *testing.T) {
	m := &fakeMailer{failTimes: 99}
	type failure struct {
		attempts int
		final    bool
	}
	var failures []failure
	var runs int
	hooks := mail.Hooks{
		OnFailed: func(_, _ string, _ error, attempts int, final bool) {
			runs++
			failures = append(failures, failure{attempts, final})
		},
	}
	ob, db, clock := newOutbox(t, m, mail.OutboxConfig{
		MaxAttempts: 3,
		Backoff:     []time.Duration{time.Minute},
	}, hooks)
	ctx := context.Background()

	if err := ob.Enqueue(ctx, inviteParams(t, "invite:3", "a@example.com")); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := ob.ProcessOnce(ctx); err != nil {
			t.Fatal(err)
		}
		clock.Advance(2 * time.Minute)
	}

	status, attempts, _ := statusOf(t, db, "invite:3")
	if status != domain.MailFailed || attempts != 3 {
		t.Fatalf("row = %s/%d, want failed/3", status, attempts)
	}
	if runs != 3 {
		t.Fatalf("OnFailed called %d times, want 3", runs)
	}
	if failures[2].final != true {
		t.Error("the last OnFailed must report final=true")
	}

	// 已终态的邮件不再被处理
	got, err := ob.ProcessOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sent+got.Retried+got.Failed != 0 {
		t.Fatalf("failed mail must not be retried: %+v", got)
	}
}

// 永久性失败（模板损坏）直接终态，不浪费重试
func TestPermanentFailureIsNotRetried(t *testing.T) {
	m := &fakeMailer{}
	ob, db, _ := newOutbox(t, m, mail.OutboxConfig{MaxAttempts: 5}, mail.Hooks{})
	ctx := context.Background()

	// 故意入队一个 payload 与模板不匹配的邮件
	err := ob.Enqueue(ctx, mail.EnqueueParams{
		DedupeKey: "bad:1", To: "a@example.com",
		Template: mail.TemplateOTPCode, // payload 里没有 code
		Payload:  map[string]any{"ttl_minutes": 10},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := ob.ProcessOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Failed != 1 || got.Retried != 0 {
		t.Fatalf("processed = %+v, want 1 failed / 0 retried", got)
	}
	status, attempts, _ := statusOf(t, db, "bad:1")
	if status != domain.MailFailed || attempts != 1 {
		t.Fatalf("row = %s/%d, want failed/1（不重试）", status, attempts)
	}
}

// 队列是持久的：进程重启（新建 Outbox）后邮件仍在并会被发出
func TestQueueSurvivesRestart(t *testing.T) {
	m := &fakeMailer{}
	db := testsupport.OpenDB(t)
	ctx := context.Background()

	ob1, err := mail.NewOutbox(db, m, newRenderer(t), mail.OutboxConfig{}, mail.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := ob1.Enqueue(ctx, inviteParams(t, "invite:restart", "a@example.com")); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：全新的 Outbox 实例，同一个数据库
	ob2, err := mail.NewOutbox(db, m, newRenderer(t), mail.OutboxConfig{}, mail.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ob2.ProcessOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sent != 1 {
		t.Fatalf("processed = %+v, want 1 sent after restart", got)
	}
}

func TestHooksOnSent(t *testing.T) {
	m := &fakeMailer{}
	var (
		mu   sync.Mutex
		sent []string
	)
	hooks := mail.Hooks{OnSent: func(to, template string) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, to+"/"+template)
	}}
	ob, _, _ := newOutbox(t, m, mail.OutboxConfig{}, hooks)
	ctx := context.Background()

	if err := ob.Enqueue(ctx, inviteParams(t, "invite:hook", "a@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := ob.ProcessOnce(ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || sent[0] != "a@example.com/"+mail.TemplateInvite {
		t.Fatalf("OnSent calls = %v", sent)
	}
}

func TestStatsAndCleanup(t *testing.T) {
	m := &fakeMailer{failTimes: 99}
	ob, _, clock := newOutbox(t, m, mail.OutboxConfig{MaxAttempts: 1}, mail.Hooks{})
	ctx := context.Background()

	if err := ob.Enqueue(ctx, inviteParams(t, "ok:1", "a@example.com")); err != nil {
		t.Fatal(err)
	}
	if err := ob.Enqueue(ctx, inviteParams(t, "bad:1", "b@example.com")); err != nil {
		t.Fatal(err)
	}
	// 只让第一封成功
	m.failTimes = 1
	if _, err := ob.ProcessOnce(ctx); err != nil {
		t.Fatal(err)
	}

	stats, err := ob.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Failed+stats.Sent+stats.Pending != 2 {
		t.Fatalf("stats = %+v, want 2 rows in total", stats)
	}
	if stats.Pending > 0 && stats.OldestPendingAt == nil {
		t.Error("OldestPendingAt should be set when there is a pending mail")
	}

	clock.Advance(31 * 24 * time.Hour)
	n, err := ob.CleanupSent(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("CleanupSent: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleaned %d rows, want 1", n)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	m := &fakeMailer{}
	ob, _, _ := newOutbox(t, m, mail.OutboxConfig{}, mail.Hooks{})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- ob.Run(ctx, 10*time.Millisecond) }()

	if err := ob.Enqueue(context.Background(), inviteParams(t, "invite:run", "a@example.com")); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for len(m.messages()) == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("Run did not deliver the queued mail")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestNewOutboxValidation(t *testing.T) {
	db := testsupport.OpenDB(t)
	if _, err := mail.NewOutbox(nil, &fakeMailer{}, newRenderer(t), mail.OutboxConfig{}, mail.Hooks{}); err == nil {
		t.Error("nil db must be rejected")
	}
	if _, err := mail.NewOutbox(db, nil, newRenderer(t), mail.OutboxConfig{}, mail.Hooks{}); err == nil {
		t.Error("nil mailer must be rejected")
	}
	if _, err := mail.NewOutbox(db, &fakeMailer{}, nil, mail.OutboxConfig{}, mail.Hooks{}); err == nil {
		t.Error("nil renderer must be rejected")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
