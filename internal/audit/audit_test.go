package audit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tangthinker/user-center/v2/internal/audit"
	"github.com/tangthinker/user-center/v2/internal/testsupport"
	"gorm.io/gorm"
)

func newService(t *testing.T) (*audit.Service, *gorm.DB, *testsupport.Clock) {
	t.Helper()
	db := testsupport.OpenDB(t)
	clock := testsupport.NewClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	return audit.New(db).WithClock(clock.Now), db, clock
}

func TestRecordAndList(t *testing.T) {
	svc, _, clock := newService(t)
	ctx := context.Background()

	entries := []audit.Entry{
		{ActorID: 1, ActorEmail: "ops@example.com", Action: audit.ActionUserCreated, Target: "a@example.com"},
		{ActorID: 1, ActorEmail: "ops@example.com", Action: audit.ActionInviteSent, Target: "a@example.com"},
		{ActorID: 1, ActorEmail: "ops@example.com", Action: audit.ActionEmailChanged, Target: "b@example.com"},
	}
	for i, e := range entries {
		if err := svc.Record(ctx, e); err != nil {
			t.Fatalf("Record #%d: %v", i+1, err)
		}
		clock.Advance(time.Second)
	}

	list, err := svc.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("List returned %d rows, want 3", len(list))
	}
	// 按 id 倒序：最新在前
	if list[0].Action != audit.ActionEmailChanged {
		t.Fatalf("first row = %q, want %q", list[0].Action, audit.ActionEmailChanged)
	}
	if list[2].Action != audit.ActionUserCreated {
		t.Fatalf("last row = %q, want %q", list[2].Action, audit.ActionUserCreated)
	}
	if list[0].CreatedAt.IsZero() {
		t.Fatal("created_at not populated")
	}
}

func TestListPaging(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := svc.Record(ctx, audit.Entry{ActorID: 1, Action: audit.ActionInviteSent}); err != nil {
			t.Fatal(err)
		}
	}

	page1, err := svc.List(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	page2, err := svc.List(ctx, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("pages = %d/%d, want 2/2", len(page1), len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Fatal("paging returned overlapping rows")
	}

	// 非法参数被夹紧，不应报错
	if _, err := svc.List(ctx, -5, -5); err != nil {
		t.Fatalf("List with bad params: %v", err)
	}
}

// 安全网：把凭据写进审计表必须变成显式错误，而不是静默落库
func TestDetailRejectsCredentialLikeKeys(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		detail any
	}{
		{"direct token", struct{ Token string }{Token: "abc"}},
		{"direct code", map[string]any{"code": "123456"}},
		{"password", map[string]any{"Password": "x"}},
		{"secret", map[string]any{"client_secret": "x"}},
		{"suffix _token", map[string]any{"invite_token": "x"}},
		{"suffix _hmac", map[string]any{"code_hmac": "x"}},
		{"nested", map[string]any{"outer": map[string]any{"token": "x"}}},
		{"in slice", []any{map[string]any{"otp": "x"}}},
	}
	for _, c := range cases {
		err := svc.Record(ctx, audit.Entry{ActorID: 1, Action: audit.ActionUserCreated, Detail: c.detail})
		if !errors.Is(err, audit.ErrDetailRejected) {
			t.Errorf("%s: err = %v, want ErrDetailRejected", c.name, err)
		}
	}

	// 确认没有任何一行落库
	list, err := svc.List(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("%d rejected entries leaked into the table", len(list))
	}
}

func TestDetailAcceptsInnocentKeys(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	err := svc.Record(ctx, audit.Entry{
		ActorID: 1, ActorEmail: "ops@example.com",
		Action: audit.ActionUserCreated, Target: "a@example.com",
		Detail: map[string]any{
			"email":    "a@example.com",
			"role":     "user",
			"previous": "b@example.com",
			"reason":   "admin request",
			"count":    3,
			"nested":   map[string]any{"uid": "alice"},
			"list":     []any{"x", "y"},
		},
		IP: "127.0.0.1", UA: "test-agent",
	})
	if err != nil {
		t.Fatalf("innocent detail rejected: %v", err)
	}

	list, err := svc.List(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("rows = %d, want 1", len(list))
	}
	if list[0].Detail == nil || !strings.Contains(*list[0].Detail, "alice") {
		t.Fatalf("detail not persisted as JSON: %v", list[0].Detail)
	}
}

func TestNilDetailIsAllowed(t *testing.T) {
	svc, _, _ := newService(t)
	if err := svc.Record(context.Background(), audit.Entry{ActorID: 1, Action: audit.ActionAdminLogin}); err != nil {
		t.Fatalf("Record with nil detail: %v", err)
	}
}

func TestEmptyActionRejected(t *testing.T) {
	svc, _, _ := newService(t)
	if err := svc.Record(context.Background(), audit.Entry{ActorID: 1}); err == nil {
		t.Fatal("expected error for empty Action")
	}
}

func TestCleanup(t *testing.T) {
	svc, db, clock := newService(t)
	ctx := context.Background()
	if err := svc.Record(ctx, audit.Entry{ActorID: 1, Action: audit.ActionAdminLogin}); err != nil {
		t.Fatal(err)
	}

	clock.Advance(200 * 24 * time.Hour)
	n, err := svc.Cleanup(ctx, 180*24*time.Hour)
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d, want 1", n)
	}
	var left int64
	if err := db.Raw(`SELECT COUNT(*) FROM admin_audit`).Row().Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("admin_audit not cleaned, left=%d", left)
	}
}
