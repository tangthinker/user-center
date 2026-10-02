// Package audit 记录管理动作审计。
//
// 设计要点（docs/auth-redesign.md §8.5）：
//   - 审计按宿主隔离（每个宿主自己的库、自己的管理员）；
//   - `detail` **禁止**出现 token / 验证码 / 口令，本包用键名黑名单做硬拦截，
//     把"不小心把凭据写进审计表"从静默事故变成显式错误。
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"gorm.io/gorm"
)

// 审计动作常量。
const (
	ActionUserCreated           = "user_created"
	ActionInviteSent            = "invite_sent"
	ActionInviteResent          = "invite_resent"
	ActionInviteLinkRegenerated = "invite_link_regenerated"
	ActionUserActivated         = "user_activated"
	ActionUserDisabled          = "user_disabled"
	ActionUserEnabled           = "user_enabled"
	ActionEmailChanged          = "email_changed"
	ActionSessionsRevoked       = "sessions_revoked"
	ActionUserDeleted           = "user_deleted"
	ActionAdminUIDInviteSent    = "admin_uid_invite_sent"
	ActionAdminLogin            = "admin_login"
	ActionAdminLoginFailed      = "admin_login_failed"
	ActionAdminLogout           = "admin_logout"
)

// ErrDetailRejected 表示 detail 中出现了疑似凭据的键名。
var ErrDetailRejected = errors.New("audit: detail contains a credential-like key")

// Entry 是一条审计记录。Detail 可为 nil、结构体或 map。
type Entry struct {
	ActorID    int64
	ActorEmail string
	Action     string
	Target     string
	Detail     any
	IP         string
	UA         string
}

// Service 是审计服务。
type Service struct {
	db  *gorm.DB
	now func() time.Time
}

// New 构造审计服务。
func New(db *gorm.DB) *Service {
	return &Service{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock 注入时钟（测试用）。
func (s *Service) WithClock(now func() time.Time) *Service {
	c := *s
	c.now = now
	return &c
}

// WithTx 返回绑定到同一事务的副本。
func (s *Service) WithTx(tx *gorm.DB) *Service {
	c := *s
	c.db = tx
	return &c
}

// Record 写入一条审计记录。
func (s *Service) Record(ctx context.Context, e Entry) error {
	if e.Action == "" {
		return errors.New("audit: Action is required")
	}

	detail, err := encodeDetail(e.Detail)
	if err != nil {
		return err
	}

	rec := &domain.AdminAudit{
		ActorID:    e.ActorID,
		ActorEmail: e.ActorEmail,
		Action:     e.Action,
		Target:     opt(e.Target),
		Detail:     detail,
		IP:         opt(e.IP),
		UA:         opt(e.UA),
		CreatedAt:  s.now().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(rec).Error; err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// List 按时间倒序返回审计记录。
func (s *Service) List(ctx context.Context, limit, offset int) ([]domain.AdminAudit, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var out []domain.AdminAudit
	err := s.db.WithContext(ctx).
		Order("id DESC").Limit(limit).Offset(offset).
		Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	return out, nil
}

// Cleanup 删除保留期之外的审计记录，返回删除行数。
func (s *Service) Cleanup(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		retention = 180 * 24 * time.Hour
	}
	res := s.db.WithContext(ctx).
		Where("created_at < ?", s.now().UTC().Add(-retention)).
		Delete(&domain.AdminAudit{})
	if res.Error != nil {
		return 0, fmt.Errorf("audit: cleanup: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// encodeDetail 序列化 detail，并拦截疑似凭据的键名。
func encodeDetail(detail any) (*string, error) {
	if detail == nil {
		return nil, nil
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal detail: %w", err)
	}

	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("audit: unmarshal detail: %w", err)
	}
	if key, bad := findForbiddenKey(probe); bad {
		return nil, fmt.Errorf("%w: %q", ErrDetailRejected, key)
	}

	s := string(raw)
	return &s, nil
}

// findForbiddenKey 递归查找疑似凭据的键名。
func findForbiddenKey(v any) (string, bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if isForbiddenKey(k) {
				return k, true
			}
			if key, bad := findForbiddenKey(child); bad {
				return key, true
			}
		}
	case []any:
		for _, child := range t {
			if key, bad := findForbiddenKey(child); bad {
				return key, true
			}
		}
	}
	return "", false
}

var forbiddenExact = map[string]struct{}{
	"token": {}, "password": {}, "passwd": {}, "secret": {}, "code": {},
	"code_hmac": {}, "hmac": {}, "otp": {}, "otp_code": {}, "credential": {},
}

func isForbiddenKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if _, bad := forbiddenExact[k]; bad {
		return true
	}
	return strings.HasSuffix(k, "_token") ||
		strings.HasSuffix(k, "_secret") ||
		strings.HasSuffix(k, "_hmac") ||
		strings.HasSuffix(k, "_password")
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
