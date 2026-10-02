package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/tangthinker/user-center/v2/internal/audit"
	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/invite"
	"github.com/tangthinker/user-center/v2/internal/mail"
	"github.com/tangthinker/user-center/v2/internal/store"
	"github.com/tangthinker/user-center/v2/internal/user"
	"gorm.io/gorm"
)

// Actor 是管理动作的执行者。
//
// 每个宿主都有自己的管理员，因此不存在跨宿主的角色体系（§0.1 L9）。
type Actor struct {
	ID    int64
	Email string
	IP    string
	UA    string
}

// CreateUserResult 是"建用户/重发邀请"的结果。
type CreateUserResult struct {
	User *domain.User
	// InviteToken 是明文邀请 token，**只在此处出现一次**。
	InviteToken string
	ExpiresAt   string
	MailQueued  bool
}

// AdminCreateUser 创建待激活用户并发出邀请。
//
// 全程一个事务：建用户 + 签发邀请 + 入队邀请邮件 + 写审计。
// 事务提交则邮件必定在队列里，回滚则不会有幽灵用户或幽灵邮件。
func (a *App) AdminCreateUser(ctx context.Context, actor Actor, email string) (*CreateUserResult, error) {
	normalized := domain.NormalizeEmail(email)
	if err := mail.ValidateAddress(normalized); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEmailInvalid, err)
	}
	if d := a.limits.AllowAdminInvite(actor.ID); !d.Allowed {
		return nil, &RateLimitedError{Reason: d.Reason, RetryAfter: d.RetryAfter}
	}

	result := &CreateUserResult{}
	err := a.transact(ctx, func(tx *gorm.DB) error {
		u, err := a.users.WithTx(tx).CreateInvited(ctx, normalized, &actor.ID)
		if err != nil {
			if store.IsUniqueViolation(err) {
				return ErrEmailTaken
			}
			return err
		}
		result.User = u

		created, err := a.issueInvite(ctx, tx, actor, u, audit.ActionUserCreated)
		if err != nil {
			return err
		}
		result.InviteToken = created.InviteToken
		result.ExpiresAt = created.ExpiresAt
		result.MailQueued = created.MailQueued
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// AdminResendInvite 重新签发邀请并再发一封邀请邮件（旧链接立即失效）。
func (a *App) AdminResendInvite(ctx context.Context, actor Actor, userID int64) (*CreateUserResult, error) {
	return a.adminIssueInvite(ctx, actor, userID, true, audit.ActionInviteResent, true)
}

// AdminRegenerateInviteLink 重新签发邀请但**不发邮件**，返回明文链接供管理员复制。
//
// 由于服务端只保存 sha256(token)，明文无法二次取回，所以"复制链接"的语义
// 必然是"重新生成"：调用前 UI 必须提示"旧链接将立即失效"（§8.4）。
func (a *App) AdminRegenerateInviteLink(ctx context.Context, actor Actor, userID int64) (*CreateUserResult, error) {
	return a.adminIssueInvite(ctx, actor, userID, false, audit.ActionInviteLinkRegenerated, false)
}

func (a *App) adminIssueInvite(
	ctx context.Context,
	actor Actor,
	userID int64,
	sendMail bool,
	action string,
	checkLimit bool,
) (*CreateUserResult, error) {
	if checkLimit {
		if d := a.limits.AllowAdminInvite(actor.ID); !d.Allowed {
			return nil, &RateLimitedError{Reason: d.Reason, RetryAfter: d.RetryAfter}
		}
	}

	result := &CreateUserResult{}
	err := a.transact(ctx, func(tx *gorm.DB) error {
		u, err := a.users.WithTx(tx).GetByID(ctx, userID)
		if err != nil {
			if errors.Is(err, user.ErrNotFound) {
				return ErrNotFound
			}
			return err
		}
		// 统一按"尚未命名"判定：invited 用户与尚未设置用户名的管理员都适用
		if u.UID != nil {
			return ErrForbidden
		}
		if u.Status == domain.UserStatusDisabled {
			return ErrForbidden
		}

		issued, err := a.issueInviteTx(ctx, tx, actor, u, action, sendMail)
		if err != nil {
			return err
		}
		result.User = u
		result.InviteToken = issued.InviteToken
		result.ExpiresAt = issued.ExpiresAt
		result.MailQueued = issued.MailQueued
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// issueInvite 在**新事务**中签发邀请（供 AdminCreateUser 复用事务时调用）。
func (a *App) issueInvite(ctx context.Context, tx *gorm.DB, actor Actor, u *domain.User, action string) (*CreateUserResult, error) {
	return a.issueInviteTx(ctx, tx, actor, u, action, true)
}

// issueInviteTx 在给定事务内签发邀请（可选是否发邮件）并写审计。
func (a *App) issueInviteTx(
	ctx context.Context,
	tx *gorm.DB,
	actor Actor,
	u *domain.User,
	action string,
	sendMail bool,
) (*CreateUserResult, error) {
	plain, inv, err := a.invites.WithTx(tx).Create(ctx, invite.CreateParams{
		UserID:    u.ID,
		Email:     u.Email,
		Purpose:   domain.PurposeInvite,
		CreatedBy: actor.ID,
	})
	if err != nil {
		return nil, err
	}

	out := &CreateUserResult{
		User:        u,
		InviteToken: plain,
		ExpiresAt:   inv.ExpiresAt.UTC().Format("2006-01-02 15:04"),
	}

	if sendMail {
		payload, err := mail.EncodePayload(mail.InvitePayload{
			InviteURL: a.InviteURL(plain),
			ExpiresAt: inv.ExpiresAt,
		})
		if err != nil {
			return nil, err
		}
		queued, err := a.enqueue(ctx, tx, mail.EnqueueParams{
			DedupeKey: "invite:" + fmt.Sprintf("%x", inv.TokenHash),
			To:        u.Email,
			Template:  mail.TemplateInvite,
			Payload:   payload,
		})
		if err != nil {
			return nil, err
		}
		out.MailQueued = queued
	}

	if err := a.audits.WithTx(tx).Record(ctx, audit.Entry{
		ActorID: actor.ID, ActorEmail: actor.Email, Action: action,
		Target: u.Email, Detail: map[string]any{"user_id": u.ID, "mailed": sendMail},
		IP: actor.IP, UA: a.hashUA(actor.UA),
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// AdminListUsers 返回用户列表。
func (a *App) AdminListUsers(ctx context.Context, _ Actor, limit, offset int) ([]domain.User, error) {
	return a.users.List(ctx, limit, offset)
}

// AdminSetUserStatus 停用/启用用户；停用会同时吊销其全部会话。
func (a *App) AdminSetUserStatus(ctx context.Context, actor Actor, userID int64, status string) error {
	if status != domain.UserStatusActive && status != domain.UserStatusDisabled {
		return fmt.Errorf("app: unsupported status %q", status)
	}

	target, err := a.users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}

	if status == domain.UserStatusDisabled {
		lastAdmin, err := a.isLastAdmin(ctx, target)
		if err != nil {
			return err
		}
		if lastAdmin {
			return fmt.Errorf("%w: 不能停用最后一个管理员", ErrForbidden)
		}
	}

	action := audit.ActionUserEnabled
	if status == domain.UserStatusDisabled {
		action = audit.ActionUserDisabled
	}

	return a.transact(ctx, func(tx *gorm.DB) error {
		if err := a.users.WithTx(tx).SetStatus(ctx, userID, status); err != nil {
			return err
		}
		if status == domain.UserStatusDisabled {
			if _, err := a.sessions.WithTx(tx).RevokeAllForUser(ctx, userID, "user_disabled"); err != nil {
				return err
			}
		}
		return a.audits.WithTx(tx).Record(ctx, audit.Entry{
			ActorID: actor.ID, ActorEmail: actor.Email, Action: action,
			Target: target.Email, Detail: map[string]any{"user_id": userID},
			IP: actor.IP, UA: a.hashUA(actor.UA),
		})
	})
}

// AdminChangeEmail 修改登录邮箱：立即生效 + 吊销该用户全部会话 + 通知旧地址。
func (a *App) AdminChangeEmail(ctx context.Context, actor Actor, userID int64, newEmail string) error {
	normalized := domain.NormalizeEmail(newEmail)
	if err := mail.ValidateAddress(normalized); err != nil {
		return fmt.Errorf("%w: %v", ErrEmailInvalid, err)
	}

	target, err := a.users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if target.Email == normalized {
		return nil // 无变化
	}
	if existing, err := a.users.GetByEmail(ctx, normalized); err == nil && existing.ID != userID {
		return ErrEmailTaken
	} else if err != nil && !errors.Is(err, user.ErrNotFound) {
		return err
	}

	oldEmail := target.Email
	return a.transact(ctx, func(tx *gorm.DB) error {
		if err := a.users.WithTx(tx).ChangeEmail(ctx, userID, normalized); err != nil {
			if store.IsUniqueViolation(err) {
				return ErrEmailTaken
			}
			return err
		}
		if _, err := a.sessions.WithTx(tx).RevokeAllForUser(ctx, userID, "email_changed"); err != nil {
			return err
		}

		payload, err := mail.EncodePayload(mail.EmailChangedPayload{OldEmail: oldEmail})
		if err != nil {
			return err
		}
		// 通知发往**旧地址**：如果变更不是本人申请，本人会立刻发现
		if _, err := a.enqueue(ctx, tx, mail.EnqueueParams{
			DedupeKey: fmt.Sprintf("email_changed:%d:%d", userID, a.Now().UnixNano()),
			To:        oldEmail,
			Template:  mail.TemplateEmailChangedNotice,
			Payload:   payload,
		}); err != nil {
			return err
		}

		return a.audits.WithTx(tx).Record(ctx, audit.Entry{
			ActorID: actor.ID, ActorEmail: actor.Email, Action: audit.ActionEmailChanged,
			Target: normalized,
			Detail: map[string]any{"user_id": userID, "old_email": oldEmail},
			IP:     actor.IP, UA: a.hashUA(actor.UA),
		})
	})
}

// AdminRevokeSessions 吊销某用户的全部会话（"踢下线"）。
func (a *App) AdminRevokeSessions(ctx context.Context, actor Actor, userID int64) (int64, error) {
	target, err := a.users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return 0, ErrNotFound
		}
		return 0, err
	}

	var n int64
	err = a.transact(ctx, func(tx *gorm.DB) error {
		count, err := a.sessions.WithTx(tx).RevokeAllForUser(ctx, userID, "admin_revoked")
		if err != nil {
			return err
		}
		n = count
		return a.audits.WithTx(tx).Record(ctx, audit.Entry{
			ActorID: actor.ID, ActorEmail: actor.Email, Action: audit.ActionSessionsRevoked,
			Target: target.Email, Detail: map[string]any{"user_id": userID, "count": count},
			IP: actor.IP, UA: a.hashUA(actor.UA),
		})
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// AdminListSessions 返回某用户的活跃会话。
//
// 用户不存在时返回 ErrNotFound（而不是一个空列表），让接口层的 404 语义准确。
func (a *App) AdminListSessions(ctx context.Context, _ Actor, userID int64) ([]domain.Session, error) {
	if _, err := a.users.GetByID(ctx, userID); err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return a.sessions.ListForUser(ctx, userID)
}

// AdminDeleteUser 删除用户（级联删除其邀请与会话）。
func (a *App) AdminDeleteUser(ctx context.Context, actor Actor, userID int64) error {
	target, err := a.users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, user.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	lastAdmin, err := a.isLastAdmin(ctx, target)
	if err != nil {
		return err
	}
	if lastAdmin {
		return fmt.Errorf("%w: 不能删除最后一个管理员", ErrForbidden)
	}

	return a.transact(ctx, func(tx *gorm.DB) error {
		if _, err := a.sessions.WithTx(tx).RevokeAllForUser(ctx, userID, "user_deleted"); err != nil {
			return err
		}
		if err := a.users.WithTx(tx).Delete(ctx, userID); err != nil {
			return err
		}
		return a.audits.WithTx(tx).Record(ctx, audit.Entry{
			ActorID: actor.ID, ActorEmail: actor.Email, Action: audit.ActionUserDeleted,
			Target: target.Email, Detail: map[string]any{"user_id": userID, "uid": target.UIDValue()},
			IP: actor.IP, UA: a.hashUA(actor.UA),
		})
	})
}

// AdminAuditLog 返回审计记录。
func (a *App) AdminAuditLog(ctx context.Context, _ Actor, limit, offset int) ([]domain.AdminAudit, error) {
	return a.audits.List(ctx, limit, offset)
}

// Stats 是管理界面用的概览数据。
type Stats struct {
	UsersByStatus map[string]int64
	Admins        int64
	Queue         mail.Stats
	MailsLast24h  int64
}

// AdminStats 汇总用户与邮件队列概况。
//
// 队列积压与发信量是宿主最需要告警的两个信号（§9.3），这里一并返回，
// 由宿主的运维体系决定阈值（L5）。
func (a *App) AdminStats(ctx context.Context, _ Actor) (*Stats, error) {
	byStatus, err := a.users.CountByStatus(ctx)
	if err != nil {
		return nil, err
	}
	admins, err := a.users.CountAdmins(ctx)
	if err != nil {
		return nil, err
	}
	out := &Stats{UsersByStatus: byStatus, Admins: admins}

	if a.outbox != nil {
		stats, err := a.outbox.Stats(ctx)
		if err != nil {
			return nil, err
		}
		out.Queue = stats
	}
	mails, err := a.limits.SentInLast24h(ctx)
	if err != nil {
		return nil, err
	}
	out.MailsLast24h = mails
	return out, nil
}

// isLastAdmin 判断目标是否为"最后一个管理员"（防止把自己锁在门外）。
func (a *App) isLastAdmin(ctx context.Context, target *domain.User) (bool, error) {
	if !target.IsAdmin {
		return false, nil
	}
	n, err := a.users.CountAdmins(ctx)
	if err != nil {
		return false, err
	}
	return n <= 1, nil
}

// InviteURL 把邀请 token 拼成完整的公开链接。
//
// 宿主若注入自定义 Renderer，则由 Renderer 自行决定链接形态（这里退化返回 token）。
func (a *App) InviteURL(token string) string {
	if r, ok := a.renderer.(*mail.DefaultRenderer); ok {
		return r.InviteURL(token)
	}
	return token
}
