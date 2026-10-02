package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// SMTPConfig 是 SMTP 发送配置。
//
// 阿里企业邮箱的取值（docs/auth-redesign.md §7.1）：
//
//	Host:       smtp.qiye.aliyun.com
//	Port:       465
//	ImplicitTLS: true
//	Username:   完整邮箱地址
//	Password:   「三方客户端安全密码」
//
// 注意：使用官方服务器地址时证书已预置，直连 465 即可；不要改用
// 自定义域名作 SMTP 地址（那需要自行上传证书）。
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	FromName string

	// ImplicitTLS 为 true 时使用隐式 TLS（465）；为 false 时使用 STARTTLS。
	ImplicitTLS bool

	// Timeout 是单次投递的整体超时，0 时使用 30s。
	Timeout time.Duration

	// AllowPlainAuthWithoutTLS 允许在未加密连接上认证。
	// 默认 false：凭据绝不应在明文信道上发送。
	AllowPlainAuthWithoutTLS bool
}

func (c SMTPConfig) withDefaults() SMTPConfig {
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.Port == 0 {
		if c.ImplicitTLS {
			c.Port = 465
		} else {
			c.Port = 25
		}
	}
	return c
}

// permanentIf5xx 把"5xx 永久性 SMTP 错误"标记为不可重试。
//
// 依据 RFC 5321：4xx 是临时性错误（值得退避重试），5xx 是永久性错误
// （重试多少次都不会成功）。认证失败（如网易的 526、通用的 535）属于 5xx，
// 若照常重试会白等 1 分钟~7 小时并让队列堆积。
func permanentIf5xx(err error) error {
	if err == nil {
		return nil
	}
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) && protoErr.Code >= 500 {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}
	return err
}

// authHint 针对常见服务商给出可执行的排错提示。
func authHint(host string) string {
	h := strings.ToLower(host)
	switch {
	case strings.Contains(h, "qiye.aliyun.com"), strings.Contains(h, "mxhichina.com"), strings.Contains(h, "aliyun"):
		// 依据阿里邮箱官方《退信提示：526 Authentication failure》的四个原因
		// https://help.aliyun.com/zh/document_detail/602363.html
		return "阿里企业邮箱认证失败（526）的四个常见原因：" +
			"① SMTP 用户名必须是**完整发信邮箱地址**，不能有多余空格；" +
			"② SMTP 密码：**未开启**三方客户端安全密码时用**邮箱登录密码**，**已开启**时必须用该功能生成的**新安全密码**；" +
			"③ 服务器应为 smtp.qiye.aliyun.com（默认 25，加密 465）；" +
			"④ 访问策略：确认已开启 SMTP 协议、策略适用范围包含该发信地址、未限制来源 IP，且频率不超过 10 次/秒"
	case strings.Contains(h, "163.com"), strings.Contains(h, "126.com"), strings.Contains(h, "yeah.net"):
		return "网易邮箱：需在「设置 → POP3/SMTP/IMAP」开启 SMTP 服务，并把它生成的「客户端授权码」" +
			"当作密码使用（不是登录密码）——错误码 526 通常就是授权码不对或 SMTP 服务未开启"
	case strings.Contains(h, "qq.com"), strings.Contains(h, "exmail"):
		return "腾讯邮箱：需在「设置 → 账户」开启 SMTP 服务，并使用生成的「授权码」"
	case strings.Contains(h, "gmail"), strings.Contains(h, "google"):
		return "Gmail：普通账号密码不能用于 SMTP，需要「应用专用密码」"
	case strings.Contains(h, "outlook"), strings.Contains(h, "office365"), strings.Contains(h, "hotmail"):
		return "Outlook/Office365：需使用应用密码，且账号需允许 SMTP AUTH"
	default:
		return "请确认 SMTP 用户名是完整邮箱地址，口令是该服务商生成的「客户端授权码 / 专用密码」而不是登录密码"
	}
}

// SMTPMailer 是基于 net/smtp 的默认实现。
type SMTPMailer struct {
	cfg SMTPConfig
}

// NewSMTP 校验并构造 SMTP 发送器。配置不完整时返回 error，绝不使用默认弱值。
func NewSMTP(cfg SMTPConfig) (*SMTPMailer, error) {
	cfg = cfg.withDefaults()
	if cfg.Host == "" {
		return nil, errors.New("mail: SMTPConfig.Host is required")
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("mail: invalid SMTPConfig.Port %d", cfg.Port)
	}
	if cfg.From == "" {
		return nil, errors.New("mail: SMTPConfig.From is required")
	}
	if err := ValidateAddress(cfg.From); err != nil {
		return nil, fmt.Errorf("mail: SMTPConfig.From: %w", err)
	}
	if cfg.Username != "" && cfg.Password == "" {
		return nil, errors.New("mail: SMTPConfig.Password is required when Username is set")
	}
	if !cfg.ImplicitTLS && !cfg.AllowPlainAuthWithoutTLS && cfg.Username != "" {
		// 使用 STARTTLS 时连接会先升级再认证，这是安全的；此分支只提示
		// 调用方：真正危险的组合是"既不加密又要认证"，由 Send 兜底拒绝。
	}
	return &SMTPMailer{cfg: cfg}, nil
}

// Verify 做一次"连接 + 认证 + 退出"的自检，不发送任何邮件。
//
// 用途：宿主在启动或后台提供"测试邮箱配置"时，先确认服务器可达且凭据正确
// （阿里企业邮箱要求使用「三方客户端安全密码」，填登录密码会在这里失败）。
func (m *SMTPMailer) Verify(ctx context.Context) error {
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	dialer := &net.Dialer{Timeout: m.cfg.Timeout}

	var client *smtp.Client
	if m.cfg.ImplicitTLS {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: m.cfg.Host,
			MinVersion: tls.VersionTLS12,
		})
		if err != nil {
			return fmt.Errorf("mail: tls dial %s: %w", addr, err)
		}
		client, err = smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("mail: smtp handshake: %w", err)
		}
	} else {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("mail: dial %s: %w", addr, err)
		}
		client, err = smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("mail: smtp handshake: %w", err)
		}
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{
				ServerName: m.cfg.Host,
				MinVersion: tls.VersionTLS12,
			}); err != nil {
				_ = client.Close()
				return fmt.Errorf("mail: starttls: %w", err)
			}
		} else if m.cfg.Username != "" && !m.cfg.AllowPlainAuthWithoutTLS {
			_ = client.Close()
			return errors.New("mail: refusing to authenticate over an unencrypted connection")
		}
	}
	defer func() { _ = client.Close() }()

	if m.cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)); err != nil {
			return fmt.Errorf("mail: 认证失败 @%s:%d（用户 %s）：%w｜%s",
				m.cfg.Host, m.cfg.Port, m.cfg.Username, err, authHint(m.cfg.Host))
		}
	}
	return client.Quit()
}

// Send 投递一封邮件。
func (m *SMTPMailer) Send(ctx context.Context, msg Message) error {
	raw, err := BuildRFC822(m.cfg.From, m.cfg.FromName, msg)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPermanent, err)
	}

	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	dialer := &net.Dialer{Timeout: m.cfg.Timeout}

	var client *smtp.Client
	if m.cfg.ImplicitTLS {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: m.cfg.Host,
			MinVersion: tls.VersionTLS12,
		})
		if err != nil {
			return fmt.Errorf("mail: tls dial %s: %w", addr, err)
		}
		client, err = smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("mail: smtp handshake: %w", err)
		}
	} else {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("mail: dial %s: %w", addr, err)
		}
		client, err = smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("mail: smtp handshake: %w", err)
		}
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{
				ServerName: m.cfg.Host,
				MinVersion: tls.VersionTLS12,
			}); err != nil {
				_ = client.Close()
				return fmt.Errorf("mail: starttls: %w", err)
			}
		} else if m.cfg.Username != "" && !m.cfg.AllowPlainAuthWithoutTLS {
			_ = client.Close()
			return errors.New("mail: refusing to authenticate over an unencrypted connection")
		}
	}
	defer func() { _ = client.Close() }()

	if m.cfg.Username != "" {
		auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return permanentIf5xx(fmt.Errorf(
				"mail: 认证失败 @%s:%d（用户 %s）：%w｜%s",
				m.cfg.Host, m.cfg.Port, m.cfg.Username, err, authHint(m.cfg.Host),
			))
		}
	}
	if err := client.Mail(m.cfg.From); err != nil {
		return permanentIf5xx(fmt.Errorf("mail: MAIL FROM <%s>: %w", m.cfg.From, err))
	}
	// RCPT 的 4xx（例如灰名单）是临时性错误，5xx（例如 550 用户不存在）是永久性的
	if err := client.Rcpt(msg.To); err != nil {
		return permanentIf5xx(fmt.Errorf("mail: RCPT TO <%s>: %w", msg.To, err))
	}

	w, err := client.Data()
	if err != nil {
		return permanentIf5xx(fmt.Errorf("mail: DATA: %w", err))
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return fmt.Errorf("mail: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return permanentIf5xx(fmt.Errorf("mail: 服务端拒收正文: %w", err))
	}
	return permanentIf5xx(client.Quit())
}

// From 返回发件地址。
func (m *SMTPMailer) From() string { return m.cfg.From }
