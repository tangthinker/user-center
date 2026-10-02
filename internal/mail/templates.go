package mail

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// 模板名。每一种都对应 docs/auth-redesign.md §5.6 的一张表。
const (
	TemplateInvite             = "invite"
	TemplateOTPCode            = "otp_code"
	TemplateWelcomeActivated   = "welcome_activated"
	TemplateWelcomeFirstLogin  = "welcome_first_login"
	TemplateEmailChangedNotice = "email_changed_notice"
	TemplateAdminLoginNotice   = "admin_login_notice"
	TemplateAdminActionNotice  = "admin_action_notice"
)

// 默认邀请落地页路径（宿主可覆盖）。
const DefaultInvitePath = "/api/v1/invite"

// Renderer 把模板名 + JSON payload 渲染成一封邮件。
//
// 宿主可以实现自己的 Renderer 来完全接管文案（L4/L6）。
type Renderer interface {
	Build(template string, payload []byte) (Message, error)
}

// RendererConfig 是默认渲染器的配置。
type RendererConfig struct {
	// ServiceName 出现在主题与正文中，让用户知道是哪项服务在给他发信。
	ServiceName string
	// PublicBaseURL 是该宿主自己的站点基址（用于拼接邀请链接）。
	PublicBaseURL string
	// InvitePath 是邀请落地页路径，为空时使用 DefaultInvitePath。
	InvitePath string
	// SupportEmail 可选：出现在正文中，供用户求助。
	SupportEmail string
}

// DefaultRenderer 是本库内置的中文模板渲染器。
type DefaultRenderer struct {
	cfg RendererConfig
}

// NewRenderer 构造默认渲染器。
func NewRenderer(cfg RendererConfig) (*DefaultRenderer, error) {
	if strings.TrimSpace(cfg.ServiceName) == "" {
		return nil, errors.New("mail: RendererConfig.ServiceName is required")
	}
	if strings.TrimSpace(cfg.PublicBaseURL) == "" {
		return nil, errors.New("mail: RendererConfig.PublicBaseURL is required")
	}
	if cfg.InvitePath == "" {
		cfg.InvitePath = DefaultInvitePath
	}
	if cfg.SupportEmail != "" {
		if err := ValidateAddress(cfg.SupportEmail); err != nil {
			return nil, fmt.Errorf("mail: RendererConfig.SupportEmail: %w", err)
		}
	}
	return &DefaultRenderer{cfg: cfg}, nil
}

// InviteURL 用公开基址拼接邀请链接。
func (r *DefaultRenderer) InviteURL(token string) string {
	return strings.TrimRight(r.cfg.PublicBaseURL, "/") + r.cfg.InvitePath +
		"?token=" + url.QueryEscape(token)
}

// --- payload 定义（同时充当渲染契约） ---

type InvitePayload struct {
	InviteURL string    `json:"invite_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type OTPPayload struct {
	Code       string `json:"code"`
	TTLMinutes int    `json:"ttl_minutes"`
}

type UIDPayload struct {
	UID string `json:"uid"`
}

type EmailChangedPayload struct {
	OldEmail string `json:"old_email"`
}

type AdminLoginPayload struct {
	IP string    `json:"ip"`
	UA string    `json:"ua"`
	At time.Time `json:"at"`
}

type AdminActionPayload struct {
	Action string    `json:"action"`
	Target string    `json:"target"`
	At     time.Time `json:"at"`
}

// EncodePayload 把 payload 结构体编码为 JSON，供出队时渲染。
//
// 已编码好的 []byte / json.RawMessage 会被原样接受——编排层常常已经编码过一次，
// 再次编码会把 JSON 变成 base64 字符串（很容易踩的坑）。
func EncodePayload(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return t, nil
	case json.RawMessage:
		return t, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("mail: encode payload: %w", err)
	}
	return raw, nil
}

// Build 渲染指定模板。
func (r *DefaultRenderer) Build(template string, payload []byte) (Message, error) {
	switch template {
	case TemplateInvite:
		var p InvitePayload
		if err := decode(payload, &p); err != nil {
			return Message{}, err
		}
		if p.InviteURL == "" {
			return Message{}, errors.New("mail: invite payload missing invite_url")
		}
		text := fmt.Sprintf(`%s 邀请你加入。

请点击下面的链接设置你的用户名并激活账号：
%s

该链接 %s 前有效，且只能使用一次。
如果你不认识发件方，请忽略本邮件。

—— %s
`, r.cfg.ServiceName, p.InviteURL, humanDeadline(p.ExpiresAt), r.cfg.ServiceName)
		// 收件人由出队方填写（见 outbox.go）
		return Message{Subject: r.subject("账号邀请：请设置你的用户名"), Text: text, HTML: htmlLink(text, p.InviteURL)}, nil

	case TemplateOTPCode:
		var p OTPPayload
		if err := decode(payload, &p); err != nil {
			return Message{}, err
		}
		if p.Code == "" {
			return Message{}, errors.New("mail: otp payload missing code")
		}
		text := fmt.Sprintf(`你的登录验证码是：

    %s

验证码 %d 分钟内有效，最多可尝试 5 次。
请勿把验证码告诉任何人——我们不会向你索要它。
如果这不是你本人的操作，请忽略本邮件。

—— %s
`, p.Code, p.TTLMinutes, r.cfg.ServiceName)
		// 注意：验证码**不放进主题**（主题会进入通知栏与预览）
		return Message{Subject: r.subject("登录验证码"), Text: text}, nil

	case TemplateWelcomeActivated:
		var p UIDPayload
		if err := decode(payload, &p); err != nil {
			return Message{}, err
		}
		text := fmt.Sprintf(`你的账号已激活。

用户名：%s

今后登录时，请回到 %s 输入本邮箱地址获取验证码。
本系统不保存密码，所有登录都通过邮箱一次性验证码完成。

—— %s
`, p.UID, strings.TrimRight(r.cfg.PublicBaseURL, "/"), r.cfg.ServiceName)
		return Message{Subject: r.subject("账号已激活"), Text: text}, nil

	case TemplateWelcomeFirstLogin:
		var p UIDPayload
		if err := decode(payload, &p); err != nil {
			return Message{}, err
		}
		text := fmt.Sprintf(`欢迎使用 %s，这是你的首次登录。

用户名：%s

几点说明：
1. 登录只需要邮箱验证码，没有密码需要记忆；
2. 如果收到不是你本人操作的验证码邮件，忽略即可，账号不会被他人登录；
3. 需要修改登录邮箱或停用账号，请联系管理员。

—— %s
`, r.cfg.ServiceName, p.UID, r.cfg.ServiceName)
		return Message{Subject: r.subject("欢迎使用"), Text: text}, nil

	case TemplateEmailChangedNotice:
		var p EmailChangedPayload
		if err := decode(payload, &p); err != nil {
			return Message{}, err
		}
		text := fmt.Sprintf(`这是一封安全通知：%s 的登录邮箱已被管理员修改。

修改前的邮箱：%s

修改后，该邮箱将无法再用于登录，且你此前的登录状态已全部失效。
如果这不是你本人申请的变更，请立即联系管理员%s。

—— %s
`, r.cfg.ServiceName, p.OldEmail, r.supportClause(), r.cfg.ServiceName)
		return Message{Subject: r.subject("安全通知：登录邮箱已变更"), Text: text}, nil

	case TemplateAdminLoginNotice:
		var p AdminLoginPayload
		if err := decode(payload, &p); err != nil {
			return Message{}, err
		}
		text := fmt.Sprintf(`有人登录了 %s 的管理界面。

时间：%s
来源 IP：%s
客户端：%s

如果你不是本人操作，请立即检查主机的本地访问权限。

—— %s
`, r.cfg.ServiceName, humanTime(p.At), orUnknown(p.IP), orUnknown(p.UA), r.cfg.ServiceName)
		return Message{Subject: r.subject("管理员登录通知"), Text: text}, nil

	case TemplateAdminActionNotice:
		var p AdminActionPayload
		if err := decode(payload, &p); err != nil {
			return Message{}, err
		}
		text := fmt.Sprintf(`%s 的管理界面执行了一次操作。

操作：%s
对象：%s
时间：%s

—— %s
`, r.cfg.ServiceName, p.Action, orUnknown(p.Target), humanTime(p.At), r.cfg.ServiceName)
		return Message{Subject: r.subject("管理员操作通知"), Text: text}, nil

	default:
		return Message{}, fmt.Errorf("mail: unknown template %q", template)
	}
}

func (r *DefaultRenderer) subject(suffix string) string {
	return "[" + r.cfg.ServiceName + "] " + suffix
}

func (r *DefaultRenderer) supportClause() string {
	if r.cfg.SupportEmail == "" {
		return ""
	}
	return "（" + r.cfg.SupportEmail + "）"
}

func decode(payload []byte, out any) error {
	if len(payload) == 0 {
		return errors.New("mail: payload is required")
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("mail: decode payload: %w", err)
	}
	return nil
}

func humanTime(t time.Time) string {
	if t.IsZero() {
		return "未知"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

func humanDeadline(t time.Time) string {
	if t.IsZero() {
		return "7 天"
	}
	return humanTime(t)
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未知"
	}
	return s
}

// htmlLink 生成极简 HTML 版本（不使用任何外部资源，避免追踪与供应链风险）。
func htmlLink(text, link string) string {
	body := escapeHTML(text)
	body = strings.ReplaceAll(body, "\n", "<br>")
	safeLink := escapeHTML(link)
	return `<html><body><p>` + body + `</p>` +
		`<p><a href="` + safeLink + `">` + safeLink + `</a></p>` +
		`</body></html>`
}

func escapeHTML(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	).Replace(s)
}
