// Package app 是用例编排层：把 store / user / session / otp / invite /
// ratelimit / mail / audit 组合成"业务动作"，供 HTTP 层与宿主直接调用。
//
// 设计约束（docs/auth-redesign.md §0.1）：
//   - 没有任何包级状态：所有依赖都挂在 App 实例上（L2）；
//   - 所有公开方法返回 error，绝不 panic（L3）；
//   - 配置与 Mailer 全部由宿主注入（L4/L5）。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tangthinker/user-center/v2/internal/audit"
	"github.com/tangthinker/user-center/v2/internal/device"
	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/invite"
	"github.com/tangthinker/user-center/v2/internal/mail"
	"github.com/tangthinker/user-center/v2/internal/otp"
	"github.com/tangthinker/user-center/v2/internal/ratelimit"
	"github.com/tangthinker/user-center/v2/internal/session"
	"github.com/tangthinker/user-center/v2/internal/store"
	"github.com/tangthinker/user-center/v2/internal/user"
	"gorm.io/gorm"
)

// 对外语义化错误。HTTP 层把它们映射为状态码与统一文案（§6.6）。
var (
	// ErrInvalidCredentials 覆盖"邮箱不存在 / 未激活 / 验证码错误"等全部情形，
	// 避免给攻击者提供预言机。
	ErrInvalidCredentials = errors.New("app: invalid credentials")
	// ErrInviteInvalid 表示邀请链接无效、过期或用尽。
	ErrInviteInvalid = errors.New("app: invitation invalid or expired")
	// ErrUIDTaken 表示 uid 已被占用。
	ErrUIDTaken = errors.New("app: uid already taken")
	// ErrUIDInvalid 表示 uid 不符合规则（这是唯一会向用户解释细节的错误）。
	ErrUIDInvalid = errors.New("app: uid invalid")
	// ErrEmailTaken 表示邮箱已被占用。
	ErrEmailTaken = errors.New("app: email already taken")
	// ErrEmailInvalid 表示邮箱格式非法。
	ErrEmailInvalid = errors.New("app: email invalid")
	// ErrNotFound 表示目标不存在。
	ErrNotFound = errors.New("app: not found")
	// ErrForbidden 表示当前管理员不允许执行该操作。
	ErrForbidden = errors.New("app: forbidden")
	// ErrSessionInvalid 表示会话无效、过期或已吊销。
	ErrSessionInvalid = errors.New("app: invalid session")
	// ErrNotConfigured 表示缺少必需依赖（例如未注入 Mailer）。
	ErrNotConfigured = errors.New("app: not configured")
)

// RateLimitedError 携带重试建议，供 HTTP 层生成 429 + Retry-After。
type RateLimitedError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("app: rate limited (%s)", e.Reason)
}

// Config 是编排层的全部输入。
type Config struct {
	// ServiceName 出现在邮件主题与正文里。
	ServiceName string
	// PublicBaseURL 是该宿主自己的站点基址（邀请链接用它拼接）。
	PublicBaseURL string
	// InvitePath 是邀请落地页路径，空则用默认值。
	InvitePath string
	// SupportEmail 可选，出现在邮件正文中。
	SupportEmail string
	// TimeZone 是"给人看的时间"所用时区（IANA 名，如 "Asia/Shanghai"）。
	//
	// 留空表示跟随宿主进程的本地时区。库内存储与比较始终是 UTC，
	// 这里只影响邮件正文与落地页上的那一行文字（见 mail.RendererConfig.TimeZone）。
	TimeZone string
	// BootstrapAdminEmail 非空时，在库内尚无管理员的情况下把它建成管理员。
	BootstrapAdminEmail string

	// HMACKey 用于验证码 HMAC（必填）。
	HMACKey []byte
	// UAHashSalt 用于对 User-Agent 做不可逆摘要（审计用）。
	UAHashSalt string

	// ReservedUIDs 是宿主追加的保留字（大小写不敏感）。
	//
	// 默认表只收录"可能被误认为系统/运营方身份"的词（见 internal/uidrule）；
	// 宿主若想保护自己的产品名或运营账号名，在这里追加。
	ReservedUIDs []string

	Session   session.Config
	OTP       otp.Config
	Invite    invite.Config
	RateLimit ratelimit.Config
	Mail      mail.OutboxConfig
	Hooks     mail.Hooks

	// Now 可注入时钟。
	Now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	if c.InvitePath == "" {
		c.InvitePath = mail.DefaultInvitePath
	}
	if c.Session.Now == nil {
		c.Session.Now = c.Now
	}
	if c.OTP.Now == nil {
		c.OTP.Now = c.Now
	}
	if c.Invite.Now == nil {
		c.Invite.Now = c.Now
	}
	if c.RateLimit.Now == nil {
		c.RateLimit.Now = c.Now
	}
	return c
}

// App 是一个用户域的完整用例集合。
type App struct {
	cfg      Config
	store    *store.Store
	users    *user.Service
	sessions *session.Service
	otps     *otp.Service
	invites  *invite.Service
	limits   *ratelimit.Service
	audits   *audit.Service
	outbox   *mail.Outbox
	renderer mail.Renderer
	db       *gorm.DB

	// loc 是"给人看的时间"所用时区；loc 为 nil 时按宿主本地时区处理。
	loc *time.Location
}

// New 构造用例层。mailer 为 nil 时表示"不发送邮件"（例如只读演练）。
func New(st *store.Store, mailer mail.Mailer, cfg Config) (*App, error) {
	if st == nil {
		return nil, errors.New("app: nil store")
	}
	if st.DB() == nil {
		return nil, errors.New("app: store has no db")
	}
	cfg = cfg.withDefaults()

	if len(cfg.HMACKey) == 0 {
		return nil, errors.New("app: Config.HMACKey is required")
	}
	if strings.TrimSpace(cfg.ServiceName) == "" {
		return nil, errors.New("app: Config.ServiceName is required")
	}
	if strings.TrimSpace(cfg.PublicBaseURL) == "" {
		return nil, errors.New("app: Config.PublicBaseURL is required")
	}

	otpCfg := cfg.OTP
	otpCfg.HMACKey = cfg.HMACKey
	otps, err := otp.New(st.DB(), otpCfg)
	if err != nil {
		return nil, err
	}

	renderer, err := mail.NewRenderer(mail.RendererConfig{
		ServiceName:   cfg.ServiceName,
		PublicBaseURL: cfg.PublicBaseURL,
		InvitePath:    cfg.InvitePath,
		SupportEmail:  cfg.SupportEmail,
		TimeZone:      cfg.TimeZone,
	})
	if err != nil {
		return nil, err
	}
	// 展示用时区与邮件渲染器共用同一份解析结果，避免"邮件显示的时区"和
	// "落地页/接口显示的时区"各说各话。
	displayLoc := renderer.Location()

	var outbox *mail.Outbox
	if mailer != nil {
		outbox, err = mail.NewOutbox(st.DB(), mailer, renderer, cfg.Mail, cfg.Hooks)
		if err != nil {
			return nil, err
		}
		outbox = outbox.WithClock(cfg.Now)
	}

	a := &App{
		cfg:      cfg,
		store:    st,
		db:       st.DB(),
		users:    user.New(st.DB()).WithClock(cfg.Now),
		sessions: session.New(st.DB(), cfg.Session),
		otps:     otps,
		invites:  invite.New(st.DB(), cfg.Invite),
		limits:   ratelimit.New(st.DB(), cfg.RateLimit),
		audits:   audit.New(st.DB()).WithClock(cfg.Now),
		outbox:   outbox,
		renderer: renderer,
		loc:      displayLoc,
	}
	return a, nil
}

// ServiceName 返回配置里的服务名。
//
// 界面用它自报家门：同一个宿主进程可能内嵌多个实例、而本库的常态是"每个宿主一套
// 后台"，管理员必须一眼看出自己在管哪一项服务。
func (a *App) ServiceName() string { return a.cfg.ServiceName }

// Store 暴露底层存储（宿主可能需要它做自己的维护任务）。
func (a *App) Store() *store.Store { return a.store }

// DB 暴露 GORM 句柄。
func (a *App) DB() *gorm.DB { return a.db }

// Renderer 暴露渲染器，便于宿主构造自定义邮件文案。
func (a *App) Renderer() mail.Renderer { return a.renderer }

// Outbox 暴露发件队列（未注入 Mailer 时为 nil）。
func (a *App) Outbox() *mail.Outbox { return a.outbox }

// Now 返回编排层时钟的当前时间。
func (a *App) Now() time.Time { return a.cfg.Now().UTC() }

// DisplayTime 把时间渲染成给人看的字符串：按配置的时区显示，并带上 UTC 偏移。
//
// 与邮件正文用的是同一套规则（layout 见调用方），避免"邮件里是 12:13、页面上是
// 04:13"这种同一件事两种说法。库内存储与比较始终是 UTC，这里只负责显示。
func (a *App) DisplayTime(t time.Time, layout string) string {
	if t.IsZero() {
		return ""
	}
	loc := a.loc
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format(layout)
}

// DisplayLocation 返回展示用时区（供宿主日志提示"现在是按哪个时区显示"）。
func (a *App) DisplayLocation() *time.Location {
	if a.loc == nil {
		return time.Local
	}
	return a.loc
}

// BootstrapResult 描述引导管理员这一步实际做了什么，便于宿主记录或提示运维。
type BootstrapResult struct {
	// Action ∈ created | promoted | noop | skipped
	Action string
	// Email 是配置的引导管理员邮箱（未配置时为空）。
	Email string
	// UserID 是管理员用户 ID（未配置或跳过时为 0）。
	UserID int64
	// UIDSet 表示该管理员是否已经设置过用户名。
	UIDSet bool
	// InviteSent 表示本次是否新发了"设置用户名"邮件（沙箱/无 Mailer 时为 false）。
	InviteSent bool
	// ExistingInvite 表示已有一封未过期的设置链接，本次刻意没有重发。
	ExistingInvite bool
}

// Bootstrap 确保"引导管理员存在、可用、且有机会设置自己的用户名"（幂等）。
//
// 行为：
//   - 库内没有任何管理员 → 若该邮箱已存在则提升为管理员（promoted），否则创建（created）；
//   - 已有管理员     → 仅当配置的邮箱本身就是管理员时才继续（否则 skipped）；
//   - 管理员还没有用户名 → 发一封"设置用户名"的邀请邮件；
//     若已有一封未过期的链接，则**不重发**（避免把对方邮箱里那条还有效的链接作废）；
//   - 管理员已有用户名   → 什么都不做。
//
// 注意：管理员始终以 active 状态创建——唯一管理员必须能立刻登录管理界面，
// 否则在他点开邮件之前系统就无人可管了。
func (a *App) Bootstrap(ctx context.Context) (*BootstrapResult, error) {
	email := domain.NormalizeEmail(a.cfg.BootstrapAdminEmail)
	if email == "" {
		return &BootstrapResult{Action: "skipped"}, nil
	}
	if err := mail.ValidateAddress(email); err != nil {
		return nil, fmt.Errorf("app: BootstrapAdminEmail: %w", ErrEmailInvalid)
	}

	res := &BootstrapResult{Email: email}

	count, err := a.users.CountAdmins(ctx)
	if err != nil {
		return nil, err
	}

	var admin *domain.User
	if count > 0 {
		existing, err := a.users.GetByEmail(ctx, email)
		switch {
		case errors.Is(err, user.ErrNotFound):
			// 已有别的管理员，且配置的邮箱不是管理员 → 不越权改动
			return &BootstrapResult{Action: "skipped", Email: email}, nil
		case err != nil:
			return nil, err
		}
		if !existing.IsAdmin {
			return &BootstrapResult{Action: "skipped", Email: email}, nil
		}
		admin = existing
		res.Action = "noop"
	} else {
		existing, err := a.users.GetByEmail(ctx, email)
		switch {
		case err == nil:
			if err := a.users.SetAdmin(ctx, existing.ID, true); err != nil {
				return nil, err
			}
			if existing.Status != domain.UserStatusActive {
				if err := a.users.SetStatus(ctx, existing.ID, domain.UserStatusActive); err != nil {
					return nil, err
				}
				existing.Status = domain.UserStatusActive
			}
			existing.IsAdmin = true
			admin = existing
			res.Action = "promoted"
		case errors.Is(err, user.ErrNotFound):
			created, err := a.users.CreateActive(ctx, email, true)
			if err != nil {
				return nil, err
			}
			admin = created
			res.Action = "created"
		default:
			return nil, err
		}
	}

	res.UserID = admin.ID
	res.UIDSet = admin.UID != nil
	if res.UIDSet {
		return res, nil
	}

	hasActive, err := a.invites.HasActive(ctx, admin.ID)
	if err != nil {
		return nil, err
	}
	if hasActive {
		res.ExistingInvite = true
		return res, nil
	}

	err = a.transact(ctx, func(tx *gorm.DB) error {
		plain, inv, err := a.invites.WithTx(tx).Create(ctx, invite.CreateParams{
			UserID:    admin.ID,
			Email:     admin.Email,
			Purpose:   domain.PurposeAdminUID,
			CreatedBy: 0, // 系统发起
		})
		if err != nil {
			return err
		}

		payload, err := mail.EncodePayload(mail.InvitePayload{
			InviteURL: a.InviteURL(plain),
			ExpiresAt: inv.ExpiresAt,
		})
		if err != nil {
			return err
		}
		queued, err := a.enqueue(ctx, tx, mail.EnqueueParams{
			DedupeKey: "admin_uid:" + fmt.Sprintf("%x", inv.TokenHash),
			To:        admin.Email,
			Template:  mail.TemplateInvite,
			Payload:   payload,
		})
		if err != nil {
			return err
		}
		res.InviteSent = queued

		return a.audits.WithTx(tx).Record(ctx, audit.Entry{
			ActorID:    0,
			ActorEmail: "system",
			Action:     audit.ActionAdminUIDInviteSent,
			Target:     admin.Email,
			Detail:     map[string]any{"user_id": admin.ID, "action": res.Action},
		})
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// touchLastLogin 记录登录并返回是否首次登录。
func (a *App) touchLastLogin(ctx context.Context, tx *gorm.DB, userID int64) (bool, error) {
	return a.users.WithTx(tx).MarkLogin(ctx, userID)
}

// hashUA 对 User-Agent 做不可逆摘要。仅用于审计与异常识别，不做硬绑定
// （§5.4：移动网络下 IP/UA 会变，硬绑定会误伤）。
func (a *App) hashUA(ua string) string {
	if ua == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(a.cfg.UAHashSalt + "\x00" + ua))
	return hex.EncodeToString(sum[:8])
}

// deviceID 把设备指纹原像摘要成 device_id。
//
// 与 hashUA 用不同的域前缀：两类摘要都落在同一个盐上，前缀不隔离就等于
// 允许"拿审计里的 UA 摘要去比对设备指纹"这种荒唐事。
//
// 认不出来的设备返回空串（落库为 NULL），让它们与旧版本留下的 NULL
// device_id 归成同一台"未知设备"，而不是各占一行。
func (a *App) deviceID(info device.Info) string {
	key := info.Key()
	if key == device.KeyUnknown {
		return ""
	}
	sum := sha256.Sum256([]byte(a.cfg.UAHashSalt + "\x00device\x00" + key))
	return hex.EncodeToString(sum[:8])
}

// enqueue 在给定事务内入队一封邮件，返回是否**确实**入队。
//
// 未注入 Mailer 时队列不存在，此时返回 false —— 调用方据此把
// mail_queued 置为 false，让管理员知道"邮件没有发出"，而不是以为已发出。
// 重复入队（幂等命中）不算错误，返回 false。
func (a *App) enqueue(ctx context.Context, tx *gorm.DB, p mail.EnqueueParams) (bool, error) {
	if a.outbox == nil {
		return false, nil
	}
	err := a.outbox.WithTx(tx).Enqueue(ctx, p)
	if err != nil {
		if errors.Is(err, mail.ErrDuplicate) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// transact 在一个事务里执行 fn。
func (a *App) transact(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return a.db.WithContext(ctx).Transaction(fn)
}

// maskEmail 用于"回显但不过度暴露"的场景（当前未启用，保留给宿主）。
func maskEmail(email string) string {
	local, domainPart, ok := strings.Cut(email, "@")
	if !ok || len(local) <= 1 {
		return "***"
	}
	return local[:1] + "***@" + domainPart
}
