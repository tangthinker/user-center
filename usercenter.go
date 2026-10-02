// Package usercenter 是本库的公开入口。
//
// 形态约定（docs/auth-redesign.md §0.1）：
//
//	uc, err := usercenter.New(usercenter.Config{
//	    DBPath:        "/var/lib/svc-a",
//	    ServiceName:   "云盘",
//	    PublicBaseURL: "https://files.example.com",
//	    HMACKey:       []byte(os.Getenv("UC_HMAC_KEY")),
//	    Admin:         usercenter.AdminConfig{BootstrapEmail: "ops@example.com"},
//	    Mail:          &usercenter.MailConfig{Host: "smtp.qiye.aliyun.com", ...},
//	})
//	if err != nil { return err }
//	defer uc.Close()
//
//	// 公开面：宿主挂到自己的公网监听上
//	public, _ := uc.PublicHandler("/api/v1")
//	public.Register(app.Group("/api/v1"))
//
//	// 管理面：宿主挂到**只绑回环**的第二个监听上
//	admin, _ := uc.AdminHandler("/admin")
//	admin.Register(adminApp.Group("/admin"))
//
// 本包不含任何包级状态：同一个进程里可以创建多个互相隔离的实例（L2）。
package usercenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/v2/internal/app"
	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/httpapi"
	"github.com/tangthinker/user-center/v2/internal/invite"
	"github.com/tangthinker/user-center/v2/internal/mail"
	"github.com/tangthinker/user-center/v2/internal/otp"
	"github.com/tangthinker/user-center/v2/internal/ratelimit"
	"github.com/tangthinker/user-center/v2/internal/session"
	"github.com/tangthinker/user-center/v2/internal/store"
	"gorm.io/gorm"
)

// MailConfig 是内置 SMTP 发送器的配置。
//
// 阿里企业邮箱（官方文档取值）：
//
//	Host: "smtp.qiye.aliyun.com", Port: 465, ImplicitTLS: true,
//	Username: 完整邮箱地址, Password: 三方客户端安全密码
//
// 使用前需在邮箱管理后台开启该账号的「第三方客户端登录权限」与 POP3/IMAP 权限。
type MailConfig struct {
	Host        string
	Port        int
	Username    string
	Password    string
	From        string
	FromName    string
	ImplicitTLS bool

	// Timeout 是单次投递超时，0 时取 30s。
	Timeout time.Duration

	// WorkerInterval 是队列处理间隔，0 时取 10s。
	WorkerInterval time.Duration
}

func (c MailConfig) smtp() mail.SMTPConfig {
	return mail.SMTPConfig{
		Host:        c.Host,
		Port:        c.Port,
		Username:    c.Username,
		Password:    c.Password,
		From:        c.From,
		FromName:    c.FromName,
		ImplicitTLS: c.ImplicitTLS,
		Timeout:     c.Timeout,
	}
}

// Message 是一封待发送的邮件（对外形态）。
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Mailer 是发送抽象：宿主可以注入任意实现（自建网关 SDK、其他 provider）。
//
// 若同时设置了 Config.Mailer 与 Config.Mail，以 Mailer 为准。
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// mailerAdapter 把宿主实现适配到内部接口。
type mailerAdapter struct{ inner Mailer }

func (a mailerAdapter) Send(ctx context.Context, msg mail.Message) error {
	return a.inner.Send(ctx, Message{To: msg.To, Subject: msg.Subject, Text: msg.Text, HTML: msg.HTML})
}

// Hooks 把发送结果与内部错误交给宿主的日志/告警体系（L5）。
type Hooks struct {
	MailSent    func(to, template string)
	MailFailed  func(to, template string, err error, attempts int, final bool)
	WorkerError func(err error)
	HTTPError   func(err error)
}

// Config 是实例配置。
type Config struct {
	// DBPath 以 ".db" 结尾时视为文件路径，否则视为目录（库文件为 <DBPath>/user-center.db）。
	DBPath string

	// ServiceName 出现在邮件主题与正文中（必填）。
	ServiceName string
	// PublicBaseURL 是**该宿主自己的**站点基址，用于拼接邀请链接（必填）。
	PublicBaseURL string
	// InvitePath 是邀请落地页的**公开绝对路径**，默认 "/api/v1/invite"。
	// 必须与 httpapi.PublicConfig 的挂载方式一致。
	InvitePath string
	// SupportEmail 可选，出现在邮件正文中。
	SupportEmail string

	// HMACKey 是验证码 HMAC 密钥（必填，至少 16 字节；建议 32 字节随机值）。
	HMACKey []byte
	// UAHashSalt 用于对 User-Agent 做不可逆摘要（审计用）。
	UAHashSalt string

	// AppIcon 是管理界面与邀请落地页使用的应用图标（PNG/JPEG/WebP/GIF/ICO 的原始字节）。
	//
	// 为空时界面用服务名首字作为标识。图标由库在**同源**路径下提供
	// （<管理面>/assets/icon 与 <公开面>/invite-assets/icon），同时用作浏览器标签页图标——
	// 因此不需要把宿主的静态资源域名写进 CSP。不支持 SVG（可携带脚本）。
	AppIcon []byte

	// AppIconContentType 是图标的 MIME 类型；留空或非法时按内容嗅探。
	AppIconContentType string

	// ReservedUIDs 是宿主追加的 uid 保留字（大小写不敏感）。
	//
	// 默认表只收录可能被误认为系统身份的词（admin / support / noreply …）；
	// 想保护自己的产品名或运营账号名，在这里追加即可，不必改库。
	ReservedUIDs []string

	// BootstrapAdminEmail 非空时，在"库内尚无管理员"的情况下把它建成管理员（幂等）。
	BootstrapAdminEmail string

	// Mailer 允许宿主注入自定义发送实现；与 Mail 同时设置时以 Mailer 为准。
	Mailer Mailer

	// Mail 是内置 SMTP 发送器的配置；Mailer 与 Mail 都为空时，邮件只入队不投递
	// （宿主可让管理员从管理界面复制邀请链接人工送达）。
	// 注意：**未配置任何发送器时不会创建发件队列**，因此管理接口返回的
	// mail_queued 会是 false——这是刻意的诚实语义，避免"以为发了其实没发"。
	Mail *MailConfig

	// WorkerInterval 是队列处理间隔，0 时取 10s。
	WorkerInterval time.Duration

	// DisableWorker 为 true 时不启动后台投递协程，由宿主自行调用队列处理
	// （测试、只读演练或宿主已有自己的调度器时使用）。
	DisableWorker bool

	// SkipMigration 为 true 时不执行 schema 迁移（只读/演练场景）。
	SkipMigration bool

	// 以下策略留零即用默认值（见各包 DefaultConfig）。
	Session    session.Config
	OTP        otp.Config
	Invite     invite.Config
	RateLimit  ratelimit.Config
	MailOutbox mail.OutboxConfig

	// Now 可注入时钟。
	Now func() time.Time

	Hooks Hooks
}

// Validate 校验必填项，返回可直接展示给运维的错误。
func (c Config) Validate() error {
	if strings.TrimSpace(c.DBPath) == "" {
		return errors.New("usercenter: Config.DBPath is required")
	}
	if strings.TrimSpace(c.ServiceName) == "" {
		return errors.New("usercenter: Config.ServiceName is required（用于邮件主题与正文）")
	}
	if strings.TrimSpace(c.PublicBaseURL) == "" {
		return errors.New("usercenter: Config.PublicBaseURL is required（用于拼接邀请链接）")
	}
	if len(c.HMACKey) < 16 {
		return errors.New("usercenter: Config.HMACKey is required and must be at least 16 bytes")
	}
	if len(c.AppIcon) > 0 {
		if _, err := httpapi.NewIcon(c.AppIcon, c.AppIconContentType); err != nil {
			return err
		}
	}
	if c.Mail != nil {
		if strings.TrimSpace(c.Mail.Host) == "" {
			return errors.New("usercenter: Mail.Host is required（阿里企业邮箱为 smtp.qiye.aliyun.com）")
		}
		if strings.TrimSpace(c.Mail.From) == "" {
			return errors.New("usercenter: Mail.From is required")
		}
		if c.Mail.Username != "" && strings.TrimSpace(c.Mail.Password) == "" {
			return errors.New("usercenter: Mail.Password is required when Mail.Username is set（阿里企业邮箱使用「三方客户端安全密码」）")
		}
	}
	return nil
}

// UserCenter 是一个用户域实例。
type UserCenter struct {
	cfg    Config
	store  *store.Store
	app    *app.App
	outbox *mail.Outbox

	// 后台投递协程的生命周期由实例管理：Close 会等待它退出（L8）。
	workerCancel context.CancelFunc
	workerDone   chan struct{}

	// bootstrap 记录引导管理员这一步做了什么（宿主可用于日志/提示）。
	bootstrap *app.BootstrapResult

	// icon 是校验过的应用图标（可为 nil）。
	icon *httpapi.Icon

	closeOnce sync.Once
	closeErr  error
}

// New 创建实例：打开存储 → 执行迁移 → 构造用例层 → 引导管理员 → 启动邮件投递。
//
// 任何失败都返回 error，**绝不 panic**（L3）。
func New(cfg Config) (*UserCenter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	st, err := store.Open(store.Config{
		DBPath:        cfg.DBPath,
		SkipMigration: cfg.SkipMigration,
	})
	if err != nil {
		return nil, err
	}

	var mailer mail.Mailer
	switch {
	case cfg.Mailer != nil:
		mailer = mailerAdapter{inner: cfg.Mailer}
	case cfg.Mail != nil:
		mailer, err = mail.NewSMTP(cfg.Mail.smtp())
		if err != nil {
			_ = st.Close()
			return nil, err
		}
	}

	appCfg := app.Config{
		ServiceName:         cfg.ServiceName,
		PublicBaseURL:       cfg.PublicBaseURL,
		InvitePath:          cfg.InvitePath,
		SupportEmail:        cfg.SupportEmail,
		BootstrapAdminEmail: cfg.BootstrapAdminEmail,
		HMACKey:             cfg.HMACKey,
		UAHashSalt:          cfg.UAHashSalt,
		ReservedUIDs:        cfg.ReservedUIDs,
		Session:             cfg.Session,
		OTP:                 cfg.OTP,
		Invite:              cfg.Invite,
		RateLimit:           cfg.RateLimit,
		Mail:                cfg.MailOutbox,
		Now:                 cfg.Now,
		Hooks: mail.Hooks{
			OnSent:        cfg.Hooks.MailSent,
			OnFailed:      cfg.Hooks.MailFailed,
			OnWorkerError: cfg.Hooks.WorkerError,
		},
	}

	a, err := app.New(st, mailer, appCfg)
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	icon, err := httpapi.NewIcon(cfg.AppIcon, cfg.AppIconContentType)
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	uc := &UserCenter{cfg: cfg, store: st, app: a, icon: icon}

	interval := cfg.WorkerInterval
	if interval <= 0 && cfg.Mail != nil {
		interval = cfg.Mail.WorkerInterval
	}
	// 先起投递协程、再引导管理员：这样"请设置用户名"的邮件能立刻发出，
	// 而不是干等一个轮询周期。
	if mailer != nil && !cfg.DisableWorker {
		uc.startWorker(interval)
	}

	bootstrap, err := a.Bootstrap(context.Background())
	if err != nil {
		_ = uc.Close()
		return nil, fmt.Errorf("usercenter: bootstrap admin: %w", err)
	}
	uc.bootstrap = bootstrap
	return uc, nil
}

func (u *UserCenter) startWorker(interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.workerCancel = cancel
	u.workerDone = make(chan struct{})

	ob := u.app.Outbox()
	if ob == nil {
		close(u.workerDone)
		return
	}
	go func() {
		defer close(u.workerDone)
		_ = ob.Run(ctx, interval)
	}()
}

// Close 停止后台投递并关闭存储。可重复调用（幂等）。
func (u *UserCenter) Close() error {
	if u == nil {
		return nil
	}
	u.closeOnce.Do(func() {
		if u.workerCancel != nil {
			u.workerCancel()
		}
		if u.workerDone != nil {
			<-u.workerDone
		}
		if u.store != nil {
			u.closeErr = u.store.Close()
		}
	})
	return u.closeErr
}

// BootstrapResult 返回启动引导的结果：管理员是谁、是否已设置用户名、
// 本次是否发出了"设置用户名"的邮件。宿主可用它打一条启动日志。
func (u *UserCenter) BootstrapResult() *app.BootstrapResult {
	if u == nil {
		return nil
	}
	return u.bootstrap
}

// App 暴露用例层（宿主可直接调用全部业务动作）。
func (u *UserCenter) App() *app.App { return u.app }

// DB 暴露 GORM 句柄（宿主做自有维护任务时使用）。
func (u *UserCenter) DB() *gorm.DB { return u.store.DB() }

// Store 暴露存储实例。
func (u *UserCenter) Store() *store.Store { return u.store }

// PublicHandler 构造公开面处理器。
//
// mountPath 必须与宿主实际挂载的前缀一致（例如宿主挂在 "/api/v1"），
// 它决定页面内注入的 API 前缀以及邀请落地页/静态资源路径。
func (u *UserCenter) PublicHandler(mountPath string) (*httpapi.Public, error) {
	if mountPath == "" {
		mountPath = httpapi.DefaultMountPath
	}
	return httpapi.NewPublic(u.app, httpapi.PublicConfig{
		MountPath: mountPath,
		Icon:      u.icon,
		OnError:   u.cfg.Hooks.HTTPError,
	})
}

// AdminHandler 构造管理面处理器。
//
// ⚠ 宿主必须把它挂到**只绑回环地址**的监听上；处理器自身还会做一次
// fail-closed 校验（只认 RemoteAddr + Host 白名单）。
func (u *UserCenter) AdminHandler(mountPath string) (*httpapi.Admin, error) {
	if mountPath == "" {
		mountPath = httpapi.DefaultAdminMountPath
	}
	return httpapi.NewAdmin(u.app, httpapi.AdminConfig{
		MountPath: mountPath,
		Icon:      u.icon,
		OnError:   u.cfg.Hooks.HTTPError,
	})
}

// RegisterPublic 是 PublicHandler + Register 的便捷封装。
func (u *UserCenter) RegisterPublic(router fiber.Router, mountPath string) (*httpapi.Public, error) {
	h, err := u.PublicHandler(mountPath)
	if err != nil {
		return nil, err
	}
	h.Register(router)
	return h, nil
}

// RegisterAdmin 是 AdminHandler + Register 的便捷封装。
func (u *UserCenter) RegisterAdmin(router fiber.Router, mountPath string) (*httpapi.Admin, error) {
	h, err := u.AdminHandler(mountPath)
	if err != nil {
		return nil, err
	}
	h.Register(router)
	return h, nil
}

// VerifyToken 校验**普通用户**会话并返回 uid。
//
// 这是宿主保护自身业务资源的入口（旧版的 pkg.TokenValid 即转发到这里）。
// 管理会话（scope=admin）不会被接受——否则管理员登录会顺带获得所有用户资源的访问权。
func (u *UserCenter) VerifyToken(token string) (string, error) {
	return u.VerifyTokenContext(context.Background(), token)
}

// VerifyTokenContext 是 VerifyToken 的带 context 版本。
func (u *UserCenter) VerifyTokenContext(ctx context.Context, token string) (string, error) {
	sess, err := u.app.VerifyUserSession(ctx, token)
	if err != nil {
		return "", err
	}
	return sess.UID, nil
}

// VerifySessionContext 校验任意作用域的会话（宿主需要区分 scope 时使用）。
func (u *UserCenter) VerifySessionContext(ctx context.Context, token string) (*domain.Session, error) {
	return u.app.VerifySession(ctx, token)
}

// VerifySMTP 校验 SMTP 配置能否连接并认证（不发信）。
//
// 典型用途：宿主的"测试邮箱配置"按钮，或启动时的可选自检。
func VerifySMTP(ctx context.Context, cfg *MailConfig) error {
	if cfg == nil {
		return errors.New("usercenter: MailConfig is nil")
	}
	m, err := mail.NewSMTP(cfg.smtp())
	if err != nil {
		return err
	}
	return m.Verify(ctx)
}

// SendTestMail 用给定 SMTP 配置发送一封测试邮件。
//
// 这是排查"用户收不到邮件"最直接的手段：既验证了连通性与凭据，
// 也验证了发件人、SPF/DKIM 与收件方是否把它投进垃圾箱。
func SendTestMail(ctx context.Context, cfg *MailConfig, to string) error {
	if cfg == nil {
		return errors.New("usercenter: MailConfig is nil")
	}
	m, err := mail.NewSMTP(cfg.smtp())
	if err != nil {
		return err
	}
	if err := mail.ValidateAddress(to); err != nil {
		return err
	}
	return m.Send(ctx, mail.Message{
		To:      to,
		Subject: "SMTP 自检",
		Text: "这是一封 user-center 的 SMTP 自检邮件。\n\n" +
			"如果你收到了它，说明发信配置可用；" +
			"若它落在垃圾箱里，请检查 SPF / DKIM / DMARC 是否都已生效。\n",
	})
}

// Maintenance 执行一次清理（过期验证码/邀请/会话、已发送邮件、限流与审计日志）。
//
// 宿主可以周期调用，或使用 StartMaintenance。
func (u *UserCenter) Maintenance(ctx context.Context) error {
	db := u.store.DB()
	now := time.Now().UTC()

	if err := db.WithContext(ctx).
		Where("expires_at < ?", now.Add(-time.Hour)).Delete(&domain.OTPCode{}).Error; err != nil {
		return fmt.Errorf("usercenter: cleanup otp: %w", err)
	}
	if err := db.WithContext(ctx).
		Where("expires_at < ?", now.Add(-30*24*time.Hour)).Delete(&domain.Invitation{}).Error; err != nil {
		return fmt.Errorf("usercenter: cleanup invitations: %w", err)
	}
	if err := db.WithContext(ctx).
		Where("absolute_expires_at < ? OR (revoked_at IS NOT NULL AND revoked_at < ?)",
			now, now.Add(-30*24*time.Hour)).Delete(&domain.Session{}).Error; err != nil {
		return fmt.Errorf("usercenter: cleanup sessions: %w", err)
	}
	if err := db.WithContext(ctx).
		Where("status = ? AND sent_at IS NOT NULL AND sent_at < ?",
			domain.MailSent, now.Add(-30*24*time.Hour)).Delete(&domain.MailOutbox{}).Error; err != nil {
		return fmt.Errorf("usercenter: cleanup outbox: %w", err)
	}
	if err := db.WithContext(ctx).
		Where("created_at < ?", now.Add(-180*24*time.Hour)).Delete(&domain.MailLog{}).Error; err != nil {
		return fmt.Errorf("usercenter: cleanup mail_log: %w", err)
	}
	if err := db.WithContext(ctx).
		Where("created_at < ?", now.Add(-180*24*time.Hour)).Delete(&domain.AdminAudit{}).Error; err != nil {
		return fmt.Errorf("usercenter: cleanup audit: %w", err)
	}
	return nil
}

// QueueStats 返回队列概况（宿主用它做积压告警）。
func (u *UserCenter) QueueStats(ctx context.Context) (mail.Stats, error) {
	if ob := u.app.Outbox(); ob != nil {
		return ob.Stats(ctx)
	}
	return mail.Stats{}, nil
}
