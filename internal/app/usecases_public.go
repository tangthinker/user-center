package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/tangthinker/user-center/v2/internal/audit"
	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/invite"
	"github.com/tangthinker/user-center/v2/internal/mail"
	"github.com/tangthinker/user-center/v2/internal/otp"
	"github.com/tangthinker/user-center/v2/internal/ratelimit"
	"github.com/tangthinker/user-center/v2/internal/session"
	"github.com/tangthinker/user-center/v2/internal/uidrule"
	"github.com/tangthinker/user-center/v2/internal/user"
	"gorm.io/gorm"
)

// RequestCodeResult 是"请求登录验证码"的内部结果。
//
// 它**不应**被原样返回给客户端：调用方必须用统一文案响应，
// 否则就成了邮箱枚举预言机（§6.6）。
type RequestCodeResult struct {
	// UserExists 表示该邮箱是否存在且可登录（仅供内部日志与测试）。
	UserExists bool
	// MailQueued 表示本轮是否真的入队了一封邮件（冷却期复用时为 false）。
	MailQueued bool
	// Reused 表示命中了冷却期复用。
	Reused bool
}

// RequestLoginCode 处理"发送登录验证码"。
//
// 无论邮箱是否存在、是否可登录，都会消耗同一份限流额度并返回同样的结果形状，
// 从而不泄漏邮箱是否已注册。
func (a *App) RequestLoginCode(ctx context.Context, email, ip string) (*RequestCodeResult, error) {
	out := &RequestCodeResult{}
	normalized := domain.NormalizeEmail(email)

	decision, err := a.limits.AllowSend(ctx, ratelimit.SendParams{
		Email: normalized, Purpose: domain.PurposeLogin, IP: ip,
	})
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		return nil, &RateLimitedError{Reason: decision.Reason, RetryAfter: decision.RetryAfter}
	}

	u, err := a.users.GetByEmail(ctx, normalized)
	switch {
	case errors.Is(err, user.ErrNotFound):
		return out, nil
	case err != nil:
		return nil, err
	}
	if !u.IsActive() {
		return out, nil
	}
	out.UserExists = true

	issued, err := a.otps.Issue(ctx, otp.IssueParams{
		Email: normalized, Purpose: domain.PurposeLogin, IP: ip,
	})
	if err != nil {
		return nil, err
	}
	out.Reused = issued.Reused
	if issued.Reused {
		return out, nil
	}

	payload, err := mail.EncodePayload(mail.OTPPayload{
		Code:       issued.Code,
		TTLMinutes: int(a.otps.TTLFor(domain.PurposeLogin).Minutes()),
	})
	if err != nil {
		return nil, err
	}
	queued, err := a.enqueue(ctx, a.db, mail.EnqueueParams{
		DedupeKey: fmt.Sprintf("otp:%d", issued.OTP.ID),
		To:        normalized,
		Template:  mail.TemplateOTPCode,
		Payload:   payload,
	})
	if err != nil {
		return nil, err
	}
	out.MailQueued = queued
	return out, nil
}

// RequestAdminLoginCode 请求**管理员登录**验证码。
//
// 与用户登录的关键差异：purpose 为 admin_login，因此码与用户登录的码互不通用；
// 非管理员邮箱一律静默不发送（对外表现与"邮箱不存在"完全一致）。
func (a *App) RequestAdminLoginCode(ctx context.Context, email, ip string) (*RequestCodeResult, error) {
	out := &RequestCodeResult{}
	normalized := domain.NormalizeEmail(email)

	decision, err := a.limits.AllowSend(ctx, ratelimit.SendParams{
		Email: normalized, Purpose: domain.PurposeAdminLogin, IP: ip,
	})
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		return nil, &RateLimitedError{Reason: decision.Reason, RetryAfter: decision.RetryAfter}
	}

	u, err := a.users.GetByEmail(ctx, normalized)
	switch {
	case errors.Is(err, user.ErrNotFound):
		return out, nil
	case err != nil:
		return nil, err
	}
	if !u.IsActive() || !u.IsAdmin {
		return out, nil
	}
	out.UserExists = true

	issued, err := a.otps.Issue(ctx, otp.IssueParams{
		Email: normalized, Purpose: domain.PurposeAdminLogin, IP: ip,
	})
	if err != nil {
		return nil, err
	}
	out.Reused = issued.Reused
	if issued.Reused {
		return out, nil
	}

	payload, err := mail.EncodePayload(mail.OTPPayload{
		Code:       issued.Code,
		TTLMinutes: int(a.otps.TTLFor(domain.PurposeAdminLogin).Minutes()),
	})
	if err != nil {
		return nil, err
	}
	queued, err := a.enqueue(ctx, a.db, mail.EnqueueParams{
		DedupeKey: fmt.Sprintf("otp:%d", issued.OTP.ID),
		To:        normalized,
		Template:  mail.TemplateOTPCode,
		Payload:   payload,
	})
	if err != nil {
		return nil, err
	}
	out.MailQueued = queued
	return out, nil
}

// LoginResult 是一次成功登录的结果。
type LoginResult struct {
	Token      string
	UID        string
	User       *domain.User
	FirstLogin bool
}

// VerifyLoginCode 校验登录验证码并签发用户会话。
func (a *App) VerifyLoginCode(ctx context.Context, email, code, ip, ua string) (*LoginResult, error) {
	return a.verifyCode(ctx, email, code, ip, ua, false)
}

// VerifyAdminCode 校验管理员验证码并签发管理会话。
//
// 与用户登录的两处差异：purpose 为 admin_login（码互不通用），
// 且要求该用户是管理员。
func (a *App) VerifyAdminCode(ctx context.Context, email, code, ip, ua string) (*LoginResult, error) {
	return a.verifyCode(ctx, email, code, ip, ua, true)
}

func (a *App) verifyCode(ctx context.Context, email, code, ip, ua string, admin bool) (*LoginResult, error) {
	normalized := domain.NormalizeEmail(email)
	purpose := domain.PurposeLogin
	scope := domain.ScopeUser
	if admin {
		purpose = domain.PurposeAdminLogin
		scope = domain.ScopeAdmin
	}

	if d := a.limits.AllowVerifyByIP(ip); !d.Allowed {
		return nil, &RateLimitedError{Reason: d.Reason, RetryAfter: d.RetryAfter}
	}

	u, err := a.users.GetByEmail(ctx, normalized)
	if errors.Is(err, user.ErrNotFound) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if admin && !u.IsAdmin {
		// 不暴露"该邮箱不是管理员"，统一按凭据错误处理
		if a.audits != nil {
			_ = a.audits.Record(ctx, audit.Entry{
				ActorID: u.ID, ActorEmail: normalized, Action: audit.ActionAdminLoginFailed,
				Detail: map[string]any{"reason": "not_admin"}, IP: ip, UA: a.hashUA(ua),
			})
		}
		return nil, ErrInvalidCredentials
	}
	if !u.IsActive() {
		return nil, ErrInvalidCredentials
	}

	if err := a.otps.Verify(ctx, normalized, purpose, code); err != nil {
		if errors.Is(err, otp.ErrInvalid) {
			if admin {
				if a.audits != nil {
					_ = a.audits.Record(ctx, audit.Entry{
						ActorID: u.ID, ActorEmail: normalized, Action: audit.ActionAdminLoginFailed,
						Detail: map[string]any{"reason": "bad_code"}, IP: ip, UA: a.hashUA(ua),
					})
				}
			}
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}

	result := &LoginResult{UID: u.UIDValue(), User: u}
	err = a.transact(ctx, func(tx *gorm.DB) error {
		issued, err := a.sessions.WithTx(tx).Issue(ctx, session.IssueParams{
			UserID: u.ID, UID: u.UIDValue(), Scope: scope, IP: ip, UAHash: a.hashUA(ua),
		})
		if err != nil {
			return err
		}
		result.Token = issued.Plain

		first, err := a.touchLastLogin(ctx, tx, u.ID)
		if err != nil {
			return err
		}
		result.FirstLogin = first

		switch {
		case admin:
			payload, err := mail.EncodePayload(mail.AdminLoginPayload{
				IP: ip, UA: ua, At: a.Now(),
			})
			if err != nil {
				return err
			}
			if _, err := a.enqueue(ctx, tx, mail.EnqueueParams{
				DedupeKey: fmt.Sprintf("admin_login:%d:%d", u.ID, a.Now().UnixNano()),
				To:        normalized,
				Template:  mail.TemplateAdminLoginNotice,
				Payload:   payload,
			}); err != nil {
				return err
			}
			return a.audits.WithTx(tx).Record(ctx, audit.Entry{
				ActorID: u.ID, ActorEmail: normalized, Action: audit.ActionAdminLogin,
				IP: ip, UA: a.hashUA(ua),
			})
		case first:
			payload, err := mail.EncodePayload(mail.UIDPayload{UID: u.UIDValue()})
			if err != nil {
				return err
			}
			// 幂等键按用户固定：并发或重试都只会有一封首次登录欢迎邮件
			_, err = a.enqueue(ctx, tx, mail.EnqueueParams{
				DedupeKey: fmt.Sprintf("welcome_first_login:%d", u.ID),
				To:        normalized,
				Template:  mail.TemplateWelcomeFirstLogin,
				Payload:   payload,
			})
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// VerifySession 校验会话 token（`/session/verify` 与宿主的 TokenValid 使用）。
func (a *App) VerifySession(ctx context.Context, token string) (*domain.Session, error) {
	sess, err := a.sessions.Verify(ctx, token)
	if err != nil {
		if errors.Is(err, session.ErrInvalid) ||
			errors.Is(err, session.ErrExpired) ||
			errors.Is(err, session.ErrRevoked) {
			return nil, ErrSessionInvalid
		}
		return nil, err
	}
	return sess, nil
}

// VerifyUserSession 校验并要求是普通用户会话。
//
// 宿主用它保护自己的业务资源：**管理会话不能当作普通用户会话使用**，
// 否则管理员登录会顺带获得所有用户资源的访问权。
func (a *App) VerifyUserSession(ctx context.Context, token string) (*domain.Session, error) {
	sess, err := a.VerifySession(ctx, token)
	if err != nil {
		return nil, err
	}
	if sess.Scope != domain.ScopeUser {
		return nil, ErrSessionInvalid
	}
	if sess.UID == "" {
		return nil, ErrSessionInvalid
	}
	return sess, nil
}

// Logout 吊销当前会话（幂等）。
func (a *App) Logout(ctx context.Context, token string) error {
	if err := a.sessions.Revoke(ctx, token, "logout"); err != nil {
		if errors.Is(err, session.ErrInvalid) {
			return nil
		}
		return err
	}
	return nil
}

// InviteView 是邀请落地页需要的只读信息。
type InviteView struct {
	Email     string
	ExpiresAt string
	NeedsUID  bool
	// AlreadyActive 表示账号已是可用状态，本次只是补一个用户名
	// （bootstrap 出来的管理员就是这种情形）。
	AlreadyActive bool
}

// GetInvite 只读查证邀请（落地页 GET 使用，**不消费** token）。
func (a *App) GetInvite(ctx context.Context, token string) (*InviteView, error) {
	inv, u, err := a.invites.Lookup(ctx, token)
	if err != nil {
		if errors.Is(err, invite.ErrInvalid) {
			return nil, ErrInviteInvalid
		}
		return nil, err
	}
	return &InviteView{
		Email:         u.Email,
		ExpiresAt:     inv.ExpiresAt.UTC().Format("2006-01-02 15:04"),
		NeedsUID:      u.UID == nil,
		AlreadyActive: u.Status == domain.UserStatusActive,
	}, nil
}

// validateUID 校验 uid 规则：默认保留字 + 宿主追加保留字。
//
// 默认表刻意不含任何宿主/作者专有名词，因此"想保护自己的名字"这件事
// 由宿主通过 Config.ReservedUIDs 决定（见 internal/uidrule 的说明）。
func (a *App) validateUID(uid string) error {
	if err := uidrule.Validate(uid); err != nil {
		return err
	}
	if uidrule.IsReservedExtra(uid, a.cfg.ReservedUIDs) {
		return uidrule.ErrReserved
	}
	return nil
}

// CheckUID 校验 uid 规则与可用性，并消耗一次该 token 的检查额度。
func (a *App) CheckUID(ctx context.Context, token, uid string) (bool, error) {
	if err := a.validateUID(uid); err != nil {
		return false, fmt.Errorf("%w: %v", ErrUIDInvalid, err)
	}
	if err := a.invites.RecordUIDCheck(ctx, token); err != nil {
		switch {
		case errors.Is(err, invite.ErrCheckLimit):
			return false, &RateLimitedError{Reason: "uid_check_limit"}
		case errors.Is(err, invite.ErrInvalid):
			return false, ErrInviteInvalid
		default:
			return false, err
		}
	}
	available, err := a.invites.UIDAvailable(ctx, uid)
	if err != nil {
		return false, err
	}
	return available, nil
}

// AcceptResult 是激活结果。
type AcceptResult struct {
	User *domain.User
}

// AcceptInvite 消费邀请：设置 uid、激活用户、发送欢迎邮件。
//
// 注意：**不签发会话**（设计决策 #6）——"如何获得会话"只有 OTP 登录一条路径。
func (a *App) AcceptInvite(ctx context.Context, token, uid, ip, ua string) (*AcceptResult, error) {
	if err := a.validateUID(uid); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUIDInvalid, err)
	}

	var activated *domain.User
	err := a.transact(ctx, func(tx *gorm.DB) error {
		u, err := a.invites.WithTx(tx).Consume(ctx, token, uid)
		if err != nil {
			switch {
			case errors.Is(err, invite.ErrUIDTaken):
				return ErrUIDTaken
			case errors.Is(err, invite.ErrInvalid):
				return ErrInviteInvalid
			default:
				return err
			}
		}
		activated = u

		payload, err := mail.EncodePayload(mail.UIDPayload{UID: u.UIDValue()})
		if err != nil {
			return err
		}
		if _, err := a.enqueue(ctx, tx, mail.EnqueueParams{
			DedupeKey: fmt.Sprintf("welcome_activated:%d", u.ID),
			To:        u.Email,
			Template:  mail.TemplateWelcomeActivated,
			Payload:   payload,
		}); err != nil {
			return err
		}
		return a.audits.WithTx(tx).Record(ctx, audit.Entry{
			ActorID: u.ID, ActorEmail: u.Email, Action: audit.ActionUserActivated,
			Target: uid, Detail: map[string]any{"user_id": u.ID}, IP: ip, UA: a.hashUA(ua),
		})
	})
	if err != nil {
		return nil, err
	}
	return &AcceptResult{User: activated}, nil
}
