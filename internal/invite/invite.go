// Package invite 实现一次性邀请：签发、只读查证、受控的用户名检查、原子消费。
//
// 设计要点（docs/auth-redesign.md §5.1）：
//   - 明文 token 永不落库，只存 sha256；
//   - GET 落地页**不消费** token（防邮件安全网关预取把链接烧掉）；
//   - 只有 POST accept 才消费，且"消费 + 设置 uid + 激活"在同一事务内完成；
//   - 消费与激活都用条件更新 + 受影响行数判定，杜绝并发重复激活；
//   - uid 唯一性由数据库部分唯一索引裁决，冲突映射为 ErrUIDTaken（不泄漏 SQL 文本）。
package invite

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/secure"
	"github.com/tangthinker/user-center/v2/internal/store"
	"gorm.io/gorm"
)

// 邀请相关的哨兵错误。
var (
	// ErrInvalid 覆盖"不存在 / 已消费 / 已过期 / 用户状态不符"全部情形。
	ErrInvalid = errors.New("invite: invalid or expired invitation")
	// ErrUIDTaken 表示 uid 已被占用（响应为 409，且不得泄漏数据库错误）。
	ErrUIDTaken = errors.New("invite: uid already taken")
	// ErrCheckLimit 表示该邀请 token 的 uid 可用性检查次数已用尽。
	ErrCheckLimit = errors.New("invite: uid check limit exceeded")
)

// DefaultTTL 是邀请链接的默认有效期（用户决策：7 天）。
const DefaultTTL = 7 * 24 * time.Hour

// DefaultCheckLimit 是每个邀请 token 允许的 uid 可用性检查次数。
const DefaultCheckLimit = 20

// Config 是邀请策略。
type Config struct {
	TTL        time.Duration
	CheckLimit int
	Now        func() time.Time
}

// DefaultConfig 返回设计文档确定的默认值。
func DefaultConfig() Config {
	return Config{TTL: DefaultTTL, CheckLimit: DefaultCheckLimit}
}

func (c Config) withDefaults() Config {
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	if c.TTL <= 0 {
		c.TTL = DefaultTTL
	}
	if c.CheckLimit <= 0 {
		c.CheckLimit = DefaultCheckLimit
	}
	return c
}

// Service 是邀请服务。
type Service struct {
	db  *gorm.DB
	cfg Config
}

// New 构造邀请服务。
func New(db *gorm.DB, cfg Config) *Service {
	return &Service{db: db, cfg: cfg.withDefaults()}
}

// WithTx 返回绑定到同一事务的副本。
func (s *Service) WithTx(tx *gorm.DB) *Service {
	c := *s
	c.db = tx
	return &c
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// CreateParams 是签发邀请的上下文。
type CreateParams struct {
	UserID    int64
	Email     string
	Purpose   string
	CreatedBy int64
}

// Create 签发一个新邀请，并作废该用户此前所有未消费的邀请（不变量 I5）。
//
// 返回的明文 token 只在此处出现一次；调用方负责放进邮件或交给管理员复制。
func (s *Service) Create(ctx context.Context, p CreateParams) (string, *domain.Invitation, error) {
	if p.UserID == 0 {
		return "", nil, errors.New("invite: UserID is required")
	}
	purpose := p.Purpose
	if purpose == "" {
		purpose = domain.PurposeInvite
	}

	plain, hash, err := secure.NewToken()
	if err != nil {
		return "", nil, err
	}
	now := s.now()
	rec := &domain.Invitation{
		TokenHash: hash,
		UserID:    p.UserID,
		Email:     p.Email,
		Purpose:   purpose,
		IssuedAt:  now,
		ExpiresAt: now.Add(s.cfg.TTL),
		CreatedBy: p.CreatedBy,
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&domain.Invitation{}).
			Where("user_id = ? AND consumed_at IS NULL", p.UserID).
			Update("consumed_at", now).Error; err != nil {
			return fmt.Errorf("invite: invalidate previous: %w", err)
		}
		if err := tx.Create(rec).Error; err != nil {
			return fmt.Errorf("invite: insert: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return plain, rec, nil
}

// Lookup 只读地查证邀请，**不改变任何状态**（GET 落地页使用）。
func (s *Service) Lookup(ctx context.Context, plain string) (*domain.Invitation, *domain.User, error) {
	if plain == "" {
		return nil, nil, ErrInvalid
	}
	var inv domain.Invitation
	err := s.db.WithContext(ctx).
		Where("token_hash = ?", secure.HashToken(plain)).
		First(&inv).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil, ErrInvalid
	}
	if err != nil {
		return nil, nil, fmt.Errorf("invite: lookup: %w", err)
	}
	if inv.ConsumedAt != nil || !inv.ExpiresAt.After(s.now()) {
		return nil, nil, ErrInvalid
	}

	var user domain.User
	if err := s.db.WithContext(ctx).Where("id = ?", inv.UserID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, ErrInvalid
		}
		return nil, nil, fmt.Errorf("invite: lookup user: %w", err)
	}
	if !canSetUID(&user) {
		return nil, nil, ErrInvalid
	}
	return &inv, &user, nil
}

// canSetUID 判断该用户当前能否凭邀请设置用户名。
//
// 规则统一为「**尚未命名**」（uid IS NULL），与 invited / active 无关：
//   - invited：正常邀请激活；
//   - active ：bootstrap 出来的管理员——账号必须立刻可用（否则唯一管理员在
//     点开邮件前就登不进管理界面，等于自锁），但仍需要设置用户名；
//   - disabled：拒绝（停用期间不应改身份）。
//
// 已命名用户（uid 非空）一律拒绝：uid 一旦设置就不可变。
func canSetUID(u *domain.User) bool {
	if u.UID != nil {
		return false
	}
	return u.Status == domain.UserStatusInvited || u.Status == domain.UserStatusActive
}

// HasActive 报告该用户是否已有一封未消费且未过期的邀请。
//
// 用途：启动引导时避免重复发信，也避免把对方邮箱里那条还有效的链接作废。
func (s *Service) HasActive(ctx context.Context, userID int64) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&domain.Invitation{}).
		Where("user_id = ? AND consumed_at IS NULL AND expires_at > ?", userID, s.now()).
		Count(&n).Error
	if err != nil {
		return false, fmt.Errorf("invite: has active: %w", err)
	}
	return n > 0, nil
}

// RecordUIDCheck 原子地为该 token 的检查计数 +1，超过上限返回 ErrCheckLimit。
//
// 计数带上限的条件更新保证"绝不超过 CheckLimit"（并发下也不会超）。
func (s *Service) RecordUIDCheck(ctx context.Context, plain string) error {
	if plain == "" {
		return ErrInvalid
	}
	hash := secure.HashToken(plain)
	now := s.now()

	const stmt = `UPDATE invitations
		SET check_count = check_count + 1
		WHERE token_hash = ? AND consumed_at IS NULL AND expires_at > ? AND check_count < ?`
	res := s.db.WithContext(ctx).Exec(stmt, hash, now, s.cfg.CheckLimit)
	if res.Error != nil {
		return fmt.Errorf("invite: record check: %w", res.Error)
	}
	if res.RowsAffected == 1 {
		return nil
	}

	// 未命中：区分"链接无效"与"检查次数用尽"，供接口层决定 410 / 429。
	if _, _, err := s.Lookup(ctx, plain); err != nil {
		return err
	}
	return ErrCheckLimit
}

// UIDAvailable 报告 uid 是否未被占用。**大小写敏感**（SQLite BINARY collation）。
func (s *Service) UIDAvailable(ctx context.Context, uid string) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).
		Model(&domain.User{}).
		Where("uid = ?", uid).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("invite: uid availability: %w", err)
	}
	return count == 0, nil
}

// Consume 消费邀请：设置 uid、激活用户。整体在一个事务内完成。
//
// 并发保护：消费与激活都是条件更新并要求受影响行数为 1；
// uid 冲突由唯一索引裁决并映射为 ErrUIDTaken。
func (s *Service) Consume(ctx context.Context, plain, uid string) (*domain.User, error) {
	if plain == "" || uid == "" {
		return nil, ErrInvalid
	}
	hash := secure.HashToken(plain)
	now := s.now()

	var userID int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var inv domain.Invitation
		if err := tx.Where("token_hash = ?", hash).First(&inv).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInvalid
			}
			return fmt.Errorf("invite: consume lookup: %w", err)
		}
		if inv.ConsumedAt != nil || !inv.ExpiresAt.After(now) {
			return ErrInvalid
		}

		var user domain.User
		if err := tx.Where("id = ?", inv.UserID).First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInvalid
			}
			return fmt.Errorf("invite: consume user: %w", err)
		}
		if !canSetUID(&user) {
			return ErrInvalid
		}

		// 1) 消费 token（单次使用）
		consumed := tx.Exec(
			`UPDATE invitations SET consumed_at = ? WHERE token_hash = ? AND consumed_at IS NULL`,
			now, hash,
		)
		if consumed.Error != nil {
			return fmt.Errorf("invite: consume: %w", consumed.Error)
		}
		if consumed.RowsAffected != 1 {
			return ErrInvalid
		}

		// 2) 写入用户名并确保账号可用。
		//
		// 并发保护是 `uid IS NULL` 这一条（而不是 status）：它同时覆盖两种合法情形——
		// invited 的受邀用户，以及 active 但还没命名的管理员。已命名用户永远匹配不上，
		// 因此"uid 只能设置一次"由 SQL 本身保证。
		// activated_at 用 COALESCE 保留管理员原本的激活时间。
		activated := tx.Exec(
			`UPDATE users
			 SET uid = ?, status = ?, activated_at = COALESCE(activated_at, ?), updated_at = ?
			 WHERE id = ? AND uid IS NULL AND status IN (?, ?)`,
			uid, domain.UserStatusActive, now, now,
			user.ID, domain.UserStatusInvited, domain.UserStatusActive,
		)
		if activated.Error != nil {
			if store.IsUniqueViolation(activated.Error) {
				return ErrUIDTaken
			}
			return fmt.Errorf("invite: activate: %w", activated.Error)
		}
		if activated.RowsAffected != 1 {
			return ErrInvalid
		}

		// 3) 作废该用户其余邀请
		if err := tx.Model(&domain.Invitation{}).
			Where("user_id = ? AND consumed_at IS NULL", user.ID).
			Update("consumed_at", now).Error; err != nil {
			return fmt.Errorf("invite: invalidate siblings: %w", err)
		}

		userID = user.ID
		return nil
	})
	if err != nil {
		return nil, err
	}

	var user domain.User
	if err := s.db.WithContext(ctx).Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, fmt.Errorf("invite: reload user: %w", err)
	}
	return &user, nil
}

// DeleteExpired 删除过期超过 30 天的邀请，返回删除行数。
func (s *Service) DeleteExpired(ctx context.Context) (int64, error) {
	res := s.db.WithContext(ctx).
		Where("expires_at < ?", s.now().Add(-30*24*time.Hour)).
		Delete(&domain.Invitation{})
	if res.Error != nil {
		return 0, fmt.Errorf("invite: delete expired: %w", res.Error)
	}
	return res.RowsAffected, nil
}
