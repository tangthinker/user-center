// Package pkg 是 **v1 兼容垫片**：让既有宿主（如 cloud-core）只改 import 路径
// 就能升级到 v2，不必改动业务代码。
//
// 与 v1 的差异：
//   - RegisterUserCenter 现在返回 error（v1 忽略返回值即可，调用语句仍可编译）；
//   - v2 不再有 /login、/register、/modify-password、/uid-unique：
//     登录改为邮箱验证码（/otp/*），用户由管理员通过邀请创建；
//   - TokenValid 的签名与语义保持不变，但**只接受普通用户会话**
//     （管理会话不会被当作下游业务凭据）。
//
// 本包是全库**唯一**允许持有包级状态与读取环境变量的地方——这是兼容性所需；
// 新宿主请直接使用根包的 usercenter.New。
package pkg

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	usercenter "github.com/tangthinker/user-center/v2"
	"github.com/tangthinker/user-center/v2/internal/httpapi"
)

// 兼容层使用的环境变量（新宿主不需要它们）。
const (
	EnvServiceName   = "UC_SERVICE_NAME"
	EnvPublicBaseURL = "UC_PUBLIC_BASE_URL"
	EnvHMACKey       = "UC_HMAC_KEY"
	EnvUAHashSalt    = "UC_UA_HASH_SALT"
	EnvSupportEmail  = "UC_SUPPORT_EMAIL"
	EnvBootstrapMail = "UC_ADMIN_EMAIL"

	EnvSMTPHost     = "UC_SMTP_HOST"
	EnvSMTPPort     = "UC_SMTP_PORT"
	EnvSMTPUser     = "UC_SMTP_USER"
	EnvSMTPPass     = "UC_SMTP_PASS"
	EnvSMTPFrom     = "UC_SMTP_FROM"
	EnvSMTPFromName = "UC_SMTP_FROM_NAME"
	EnvSMTPImplicit = "UC_SMTP_IMPLICIT_TLS"

	// EnvMountPath 是公开面的挂载前缀，用于生成邀请链接与页面内 API 前缀。
	// 默认与 cloud-core 的挂载方式一致（"/api/v1"）。
	EnvMountPath = "UC_MOUNT_PATH"
)

// Options 允许宿主在调用 RegisterUserCenter 之前覆盖兼容层行为。
type Options struct {
	ServiceName   string
	PublicBaseURL string
	HMACKey       []byte
	BootstrapMail string
	MountPath     string
	Mail          *usercenter.MailConfig
	// NoEnv 为 true 时忽略环境变量（只使用本结构里的配置）。
	NoEnv bool
}

var (
	mu             sync.Mutex
	defaultUC      *usercenter.UserCenter
	defaultOptions Options
)

// Configure 在 RegisterUserCenter 之前注入配置（可选）。
func Configure(opts Options) {
	mu.Lock()
	defer mu.Unlock()
	defaultOptions = opts
}

// Instance 返回兼容层持有的默认实例（未注册时为 nil）。
func Instance() *usercenter.UserCenter {
	mu.Lock()
	defer mu.Unlock()
	return defaultUC
}

// RegisterUserCenter 兼容 v1 的入口：把公开面挂到给定路由器上。
//
// v1 的调用方式 userPkg.RegisterUserCenter(authGroup, rootPath) 无需修改即可编译
// （Go 允许忽略返回值）；但**务必处理 error**，否则配置缺失时用户中心会静默不工作。
func RegisterUserCenter(router fiber.Router, userDBRootPth string) error {
	uc, mount, err := ensure(userDBRootPth)
	if err != nil {
		// 兼容层是唯一允许打日志的地方：静默失败比日志噪音危险得多。
		log.Printf("[user-center] 初始化失败，用户中心未挂载: %v", err)
		return err
	}

	// 注意：宿主传入的 router 已经带有它自己的前缀（cloud-core 传的是
	// app.Group("/api/v1/")），因此这里**只能注册相对路径**，不能再套一层。
	// mount 仅用于生成邀请链接与页面内的 API 前缀。
	if _, err := uc.RegisterPublic(router, mount); err != nil {
		log.Printf("[user-center] 注册公开面失败: %v", err)
		return err
	}
	return nil
}

// TokenValid 兼容 v1 的会话校验入口。
//
// 语义：返回该 token 对应的 uid；无效/过期/已吊销/管理会话都返回错误。
func TokenValid(token string) (string, error) {
	mu.Lock()
	uc := defaultUC
	mu.Unlock()
	if uc == nil {
		return "", errors.New("pkg: user-center 尚未初始化（请先调用 RegisterUserCenter）")
	}
	return uc.VerifyToken(token)
}

// RegisterAdmin 把管理面挂到给定路由器上（宿主应把它挂在只绑回环的监听上）。
//
// v1 没有管理面，这是 v2 新增能力；新代码建议直接用 uc.RegisterAdmin。
func RegisterAdmin(router fiber.Router, mountPath string) error {
	uc, _, err := ensure("")
	if err != nil {
		return err
	}
	if mountPath == "" {
		mountPath = httpapi.DefaultAdminMountPath
	}
	_, err = uc.RegisterAdmin(router.Group(mountPath), mountPath)
	return err
}

// Close 关闭兼容层实例（进程退出前调用；可重复调用）。
func Close() error {
	mu.Lock()
	uc := defaultUC
	defaultUC = nil
	mu.Unlock()
	if uc == nil {
		return nil
	}
	return uc.Close()
}

// ensure 惰性创建默认实例（每个进程一次）。
func ensure(dbDir string) (*usercenter.UserCenter, string, error) {
	mu.Lock()
	defer mu.Unlock()

	if defaultUC != nil {
		return defaultUC, mountPathOf(defaultOptions), nil
	}

	cfg, mount, err := buildConfig(defaultOptions, dbDir)
	if err != nil {
		return nil, "", err
	}
	uc, err := usercenter.New(cfg)
	if err != nil {
		return nil, "", err
	}
	defaultUC = uc
	return uc, mount, nil
}

func mountPathOf(opts Options) string {
	if opts.MountPath != "" {
		return normalizeMount(opts.MountPath)
	}
	if !opts.NoEnv {
		if v := strings.TrimSpace(os.Getenv(EnvMountPath)); v != "" {
			return normalizeMount(v)
		}
	}
	return httpapi.DefaultMountPath
}

func normalizeMount(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	return "/" + strings.Trim(p, "/")
}

// buildConfig 组装兼容层的实例配置：显式 Options 优先，其次环境变量。
func buildConfig(opts Options, dbDir string) (usercenter.Config, string, error) {
	get := func(explicit string, envKey string) string {
		if strings.TrimSpace(explicit) != "" {
			return strings.TrimSpace(explicit)
		}
		if opts.NoEnv {
			return ""
		}
		return strings.TrimSpace(os.Getenv(envKey))
	}

	mount := normalizeMount(mountPathOf(opts))
	if mount == "" {
		mount = httpapi.DefaultMountPath
	}
	// 邀请落地页的公开绝对路径必须与挂载点一致
	invitePath := mount + "/invite"

	cfg := usercenter.Config{
		DBPath:              dbDir,
		ServiceName:         get(opts.ServiceName, EnvServiceName),
		PublicBaseURL:       get(opts.PublicBaseURL, EnvPublicBaseURL),
		InvitePath:          invitePath,
		SupportEmail:        get("", EnvSupportEmail),
		BootstrapAdminEmail: get(opts.BootstrapMail, EnvBootstrapMail),
		UAHashSalt:          get("", EnvUAHashSalt),
	}

	if len(opts.HMACKey) > 0 {
		cfg.HMACKey = opts.HMACKey
	} else if !opts.NoEnv {
		cfg.HMACKey = []byte(os.Getenv(EnvHMACKey))
	}

	// 邮件配置：显式优先，其次环境变量（全都为空则不发送邮件）
	if opts.Mail != nil {
		cfg.Mail = opts.Mail
	} else if !opts.NoEnv {
		if host := strings.TrimSpace(os.Getenv(EnvSMTPHost)); host != "" {
			port := 465
			if v := strings.TrimSpace(os.Getenv(EnvSMTPPort)); v != "" {
				if n, err := strconv.Atoi(v); err == nil {
					port = n
				}
			}
			implicit := true
			if v := strings.TrimSpace(os.Getenv(EnvSMTPImplicit)); v != "" {
				implicit = v == "1" || strings.EqualFold(v, "true")
			}
			cfg.Mail = &usercenter.MailConfig{
				Host:        host,
				Port:        port,
				Username:    strings.TrimSpace(os.Getenv(EnvSMTPUser)),
				Password:    os.Getenv(EnvSMTPPass),
				From:        strings.TrimSpace(os.Getenv(EnvSMTPFrom)),
				FromName:    strings.TrimSpace(os.Getenv(EnvSMTPFromName)),
				ImplicitTLS: implicit,
				Timeout:     30 * time.Second,
			}
		}
	}

	if err := cfg.Validate(); err != nil {
		return cfg, mount, fmt.Errorf("pkg: 兼容层配置不完整：%w（请设置 %s / %s / %s，或改用 pkg.Configure）",
			err, EnvServiceName, EnvPublicBaseURL, EnvHMACKey)
	}
	return cfg, mount, nil
}
