// Package otp 实现邮箱一次性验证码的签发与校验。
//
// 设计要点（docs/auth-redesign.md §5.3）：
//   - 只存 HMAC(code)，明文只出现在返回给调用方的结构里（用于入队邮件）；
//   - 同一 (email, purpose) 任意时刻至多一个有效码（数据库部分唯一索引 + 事务）；
//   - **冷却期复用**：冷却窗口内重复请求不生成新码、不发新邮件（省配额 + 防骚扰）；
//   - 尝试次数用单条原子 SQL 递增，达到上限即作废，杜绝并发绕过；
//   - 校验失败的所有原因都归结为同一个哨兵错误，避免给攻击者提供预言机。
package otp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/secure"
	"gorm.io/gorm"
)

// ErrInvalid 覆盖"不存在 / 已过期 / 已消费 / 超出尝试次数 / 码不匹配"全部情形。
var ErrInvalid = errors.New("otp: invalid code")

// Config 是验证码策略。
type Config struct {
	// CodeLength 是验证码位数。
	CodeLength int
	// TTL 是普通登录验证码有效期；AdminTTL 是管理员登录的（更短）。
	TTL      time.Duration
	AdminTTL time.Duration
	// MaxAttempts / AdminMaxAttempts 是单个码允许的失败次数。
	MaxAttempts      int
	AdminMaxAttempts int
	// Cooldown 是重发冷却：窗口内复用现有码，不生成新码也不发新邮件。
	Cooldown time.Duration
	// HMACKey 是验证码 HMAC 密钥，由宿主注入（HMACKey 不得为空）。
	HMACKey []byte
	// Now 可注入时钟。
	Now func() time.Time
}

// DefaultConfig 返回设计文档 §5.3 的参数表。
func DefaultConfig() Config {
	return Config{
		CodeLength:       6,
		TTL:              10 * time.Minute,
		AdminTTL:         5 * time.Minute,
		MaxAttempts:      5,
		AdminMaxAttempts: 3,
		Cooldown:         60 * time.Second,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	if c.CodeLength <= 0 {
		c.CodeLength = d.CodeLength
	}
	if c.TTL <= 0 {
		c.TTL = d.TTL
	}
	if c.AdminTTL <= 0 {
		c.AdminTTL = d.AdminTTL
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = d.MaxAttempts
	}
	if c.AdminMaxAttempts <= 0 {
		c.AdminMaxAttempts = d.AdminMaxAttempts
	}
	// 零值取默认的 60 秒**冷却**（安全默认）：冷却期复用是本设计里
	// 省配额与防骚扰的关键机制，不应因为"忘了配置"而消失。
	// 显式关闭请传负数。
	switch {
	case c.Cooldown == 0:
		c.Cooldown = d.Cooldown
	case c.Cooldown < 0:
		c.Cooldown = 0
	}
	return c
}

// Service 是验证码服务。
type Service struct {
	db  *gorm.DB
	cfg Config
}

// New 构造验证码服务。HMACKey 为空时返回错误——绝不使用默认弱密钥。
func New(db *gorm.DB, cfg Config) (*Service, error) {
	cfg = cfg.withDefaults()
	if len(cfg.HMACKey) == 0 {
		return nil, errors.New("otp: Config.HMACKey is required")
	}
	return &Service{db: db, cfg: cfg}, nil
}

// WithTx 返回绑定到同一事务的副本。
func (s *Service) WithTx(tx *gorm.DB) *Service {
	c := *s
	c.db = tx
	return &c
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// TTLFor 返回指定用途的有效期。
func (s *Service) TTLFor(purpose string) time.Duration {
	if purpose == domain.PurposeAdminLogin {
		return s.cfg.AdminTTL
	}
	return s.cfg.TTL
}

// MaxAttemptsFor 返回指定用途的最大尝试次数。
func (s *Service) MaxAttemptsFor(purpose string) int {
	if purpose == domain.PurposeAdminLogin {
		return s.cfg.AdminMaxAttempts
	}
	return s.cfg.MaxAttempts
}

// IssueParams 是签发验证码的上下文。
type IssueParams struct {
	Email   string // 必须已归一化（domain.NormalizeEmail）
	Purpose string
	IP      string
}

// Issued 是签发结果。
//
// Code 为空表示命中了冷却期复用：调用方**不应**发送邮件，
// 因为服务端只保存 HMAC，无法重新获得明文码。
type Issued struct {
	Code      string
	Reused    bool
	OTP       *domain.OTPCode
	ExpiresAt time.Time
}

// Issue 签发（或复用）验证码。
func (s *Service) Issue(ctx context.Context, p IssueParams) (*Issued, error) {
	if p.Email == "" {
		return nil, errors.New("otp: Email is required")
	}
	if p.Purpose == "" {
		return nil, errors.New("otp: Purpose is required")
	}

	now := s.now()
	out := &Issued{}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing domain.OTPCode
		err := tx.Where("email = ? AND purpose = ? AND consumed_at IS NULL", p.Email, p.Purpose).
			First(&existing).Error
		switch {
		case err == nil:
			// 冷却期内且未过期 → 复用，不产生新码、不发新邮件。
			if existing.ExpiresAt.After(now) && existing.IssuedAt.Add(s.cfg.Cooldown).After(now) {
				out.Reused = true
				out.OTP = &existing
				out.ExpiresAt = existing.ExpiresAt
				return nil
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			// 没有未消费的码，继续签发。
		default:
			return fmt.Errorf("otp: lookup active code: %w", err)
		}

		// 作废该 (email, purpose) 下所有未消费的码（含已过期但未标记的），
		// 与下面的插入同处一个事务，保证"只有最新码有效"。
		if err := tx.Model(&domain.OTPCode{}).
			Where("email = ? AND purpose = ? AND consumed_at IS NULL", p.Email, p.Purpose).
			Update("consumed_at", now).Error; err != nil {
			return fmt.Errorf("otp: invalidate previous codes: %w", err)
		}

		code, err := secure.NewNumericCode(s.cfg.CodeLength)
		if err != nil {
			return err
		}
		rec := &domain.OTPCode{
			Email:     p.Email,
			Purpose:   p.Purpose,
			CodeHMAC:  secure.CodeHMAC(s.cfg.HMACKey, p.Purpose, p.Email, code),
			IssuedAt:  now,
			ExpiresAt: now.Add(s.TTLFor(p.Purpose)),
		}
		if p.IP != "" {
			ip := p.IP
			rec.RequestIP = &ip
		}
		if err := tx.Create(rec).Error; err != nil {
			return fmt.Errorf("otp: insert: %w", err)
		}

		out.Code = code
		out.OTP = rec
		out.ExpiresAt = rec.ExpiresAt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Verify 校验验证码。成功时该码被消费（单次使用）。
//
// 并发安全：尝试次数的递增与"达到上限即作废"由同一条 UPDATE 完成；
// 成功消费也要求 `consumed_at IS NULL` 且受影响行数为 1。
func (s *Service) Verify(ctx context.Context, email, purpose, code string) error {
	if email == "" || purpose == "" || code == "" {
		return ErrInvalid
	}

	var rec domain.OTPCode
	err := s.db.WithContext(ctx).
		Where("email = ? AND purpose = ? AND consumed_at IS NULL", email, purpose).
		First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrInvalid
	}
	if err != nil {
		return fmt.Errorf("otp: lookup: %w", err)
	}

	now := s.now()
	max := s.MaxAttemptsFor(purpose)
	if !rec.ExpiresAt.After(now) || rec.Attempts >= max {
		return ErrInvalid
	}

	want := secure.CodeHMAC(s.cfg.HMACKey, purpose, email, code)
	if !secure.Equal(want, rec.CodeHMAC) {
		// 原子递增；若本次使 attempts 达到上限，则同时作废该码。
		const stmt = `UPDATE otp_codes
			SET attempts = attempts + 1,
			    consumed_at = CASE WHEN attempts + 1 >= ? THEN ? ELSE consumed_at END
			WHERE id = ? AND consumed_at IS NULL AND attempts < ?`
		res := s.db.WithContext(ctx).Exec(stmt, max, now, rec.ID, max)
		if res.Error != nil {
			return fmt.Errorf("otp: record failed attempt: %w", res.Error)
		}
		return ErrInvalid
	}

	const consume = `UPDATE otp_codes SET consumed_at = ? WHERE id = ? AND consumed_at IS NULL`
	res := s.db.WithContext(ctx).Exec(consume, now, rec.ID)
	if res.Error != nil {
		return fmt.Errorf("otp: consume: %w", res.Error)
	}
	if res.RowsAffected != 1 {
		// 已被并发请求消费。
		return ErrInvalid
	}
	return nil
}

// DeleteExpired 清理过期超过 1 小时的验证码，返回删除行数。
func (s *Service) DeleteExpired(ctx context.Context) (int64, error) {
	res := s.db.WithContext(ctx).
		Where("expires_at < ?", s.now().Add(-time.Hour)).
		Delete(&domain.OTPCode{})
	if res.Error != nil {
		return 0, fmt.Errorf("otp: delete expired: %w", res.Error)
	}
	return res.RowsAffected, nil
}
