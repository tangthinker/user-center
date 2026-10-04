// 本地演示：把 user-center 按"宿主服务"的方式跑起来。
//
//	go run ./examples/demo -admin-email you@example.com
//
// 然后：
//   - 管理界面： http://127.0.0.1:9998/admin/
//   - 公开面：   http://127.0.0.1:9999/api/v1/
//
// 邮件有两种模式，由下面的 emailAccount 决定：
//   - 填了账号 → 用**真实 SMTP（阿里企业邮箱）**发信；
//   - 留空     → 把邮件打印到终端（含验证码与邀请链接），无需任何邮箱即可走通全链路。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	uc "github.com/tangthinker/user-center/v2"
)

// ============================================================================
//
//	邮箱配置：直接改这里即可（不需要环境变量）
//
// ============================================================================
//
// 阿里企业邮箱取值：
//
//	Server    smtp.qiye.aliyun.com
//	Port      465（SSL，推荐；80/587 未开通）
//	Username  完整邮箱地址，例如 noreply@your-domain.com
//	Password  「三方客户端安全密码」——**不是邮箱登录密码**
//	          需先在邮箱管理后台完成两件事，否则会一直报"用户名或密码错误"：
//	            1) 管理员为该账号开启「第三方客户端登录权限」
//	            2) 开启该账号 POP3/IMAP 权限，并生成「三方客户端安全密码」
//	From      发件地址，通常与 Username 相同（留空则自动取 Username）
//
// ⚠ 这是演示程序，口令直接写在代码里只为方便本地试跑。
//
//	真实部署请让宿主从密钥管理/配置中心注入（usercenter.Config 由宿主构造），
//	并且**不要把填好口令的这份文件提交到仓库**。
//
// ⚠ 填好之后邮件会真的发出去：请只邀请你自己的邮箱做验证。
var emailAccount = emailSettings{
	Server: "smtp.qiye.aliyun.com",
	Port:   465, // 阿里企业邮箱：465（SSL）；80/587 未开通
	// ⚠ 必须是**完整发信邮箱地址**，不能只写账号名（否则阿里会返回 526）
	Username: "cloud@tangthinker.com",
	Password: "BBaaKqE8LsQqoZxT",      // 三方客户端安全密码（未开启该功能时改用登录密码）
	From:     "cloud@tangthinker.com", // 留空则使用 Username
	FromName: "演示服务",
}

type emailSettings struct {
	Server   string
	Port     int
	Username string
	Password string
	From     string
	FromName string
}

// enabled 报告是否配置了真实发信账号。
func (e emailSettings) enabled() bool {
	return strings.TrimSpace(e.Server) != "" && strings.TrimSpace(e.Username) != ""
}

// mailConfig 把演示配置转换为库的 SMTP 配置。
func (e emailSettings) mailConfig(serviceName string) *uc.MailConfig {
	from := strings.TrimSpace(e.From)
	if from == "" {
		from = strings.TrimSpace(e.Username)
	}
	fromName := strings.TrimSpace(e.FromName)
	if fromName == "" {
		fromName = serviceName
	}
	port := e.Port
	if port == 0 {
		port = 465
	}
	return &uc.MailConfig{
		Host:        strings.TrimSpace(e.Server),
		Port:        port,
		Username:    strings.TrimSpace(e.Username),
		Password:    e.Password, // 原样使用：口令里的空白可能是有效字符
		From:        from,
		FromName:    fromName,
		ImplicitTLS: true, // 465 隐式 TLS
		Timeout:     30 * time.Second,
	}
}

// validate 在启动前做本地检查，把"配置写错"与"服务端拒绝"区分开。
//
// 这两条正是阿里企业邮箱 526 最常见的两个原因：
//   - SMTP 用户名必须是完整邮箱地址（只写账号名会被拒）
//   - 端口必须是 465（SSL），80/587 未开通
func (e emailSettings) validate() error {
	if !e.enabled() {
		return nil // 未配置 → 走"打印到终端"模式
	}
	username := strings.TrimSpace(e.Username)
	if !strings.Contains(username, "@") {
		return fmt.Errorf(
			"Username 必须是**完整发信邮箱地址**（当前为 %q）：阿里企业邮箱只写账号名会返回 526 Authentication failure",
			e.Username)
	}
	switch e.Port {
	case 25, 465, 587, 994, 2525:
	default:
		return fmt.Errorf("Port=%d 不是常见 SMTP 端口：阿里企业邮箱请用 465（SSL），80/587 未开通", e.Port)
	}
	if strings.TrimSpace(e.Password) == "" {
		return errors.New("Password 不能为空：未开启三方客户端安全密码时填**邮箱登录密码**，开启后填**三方客户端安全密码**")
	}
	if from := strings.TrimSpace(e.From); from != "" {
		if !strings.Contains(from, "@") || !strings.Contains(from[strings.Index(from, "@"):], ".") {
			return fmt.Errorf(
				"From 必须是完整邮箱地址（当前为 %q）；**留空**即自动使用 Username（%s）",
				e.From, username)
		}
		// 阿里企业邮箱要求发件人是认证邮箱本人或同域别名，否则发信会被拒：
		// 501 "MAIL FROM" is non-local account
		if fromDomain, userDomain := domainOf(from), domainOf(username); fromDomain != userDomain {
			return fmt.Errorf(
				"From（%s）与 Username（%s）不同域；阿里企业邮箱要求发件人是认证邮箱本人或同域别名，"+
					"否则会被判 501 \"MAIL FROM\" is non-local account；建议把 From 留空",
				from, username)
		}
	}
	return nil
}

// domainOf 取邮箱的域名部分（无 @ 时返回空串）。
func domainOf(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(addr[at+1:]))
}

// describe 返回一行可打印的配置摘要（**不包含口令**）。
func (e emailSettings) describe() string {
	from := strings.TrimSpace(e.From)
	if from == "" {
		from = strings.TrimSpace(e.Username)
	}
	port := e.Port
	if port == 0 {
		port = 465
	}
	return fmt.Sprintf("%s:%d，发件人 %s", strings.TrimSpace(e.Server), port, from)
}

// ============================================================================

// stdoutMailer 把邮件打印到终端（未配置 SMTP 时的演示模式）。
type stdoutMailer struct{}

func (stdoutMailer) Send(_ context.Context, msg uc.Message) error {
	fmt.Printf("\n===== 邮件（未配置 SMTP，仅打印）=====\nTo:      %s\nSubject: %s\n%s\n====================================\n\n",
		msg.To, msg.Subject, msg.Text)
	return nil
}

func main() {
	var (
		dir         = flag.String("dir", "./.demo-data", "数据目录")
		publicAddr  = flag.String("public-addr", "127.0.0.1:9999", "公开面监听地址")
		adminAddr   = flag.String("admin-addr", "127.0.0.1:9998", "管理面监听地址（务必只绑回环）")
		adminEmail  = flag.String("admin-email", "", "首个管理员邮箱（必填，幂等）")
		mountPath   = flag.String("mount", "/api/v1", "公开面挂载前缀")
		serviceName = flag.String("service-name", "演示服务", "邮件中显示的服务名")
		selfTest    = flag.Bool("test-mail", false, "启动后给管理员邮箱发一封测试邮件，然后退出")
		iconPath    = flag.String("icon", "", "管理界面与落地页的应用图标（PNG/JPEG/WebP/GIF/ICO 文件路径）")
		timeZone    = flag.String("tz", "", "邮件/页面里时间的显示时区（IANA 名，如 Asia/Shanghai；留空=跟随本机）")
	)
	flag.Parse()

	if *adminEmail == "" {
		log.Fatal("请用 -admin-email 指定首个管理员邮箱")
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		log.Fatal(err)
	}
	// 先做本地配置检查：配置写错时立刻退出，而不是等异步发信时才失败。
	if err := emailAccount.validate(); err != nil {
		log.Fatalf("邮箱配置有误：%v\n（配置位置：examples/demo/main.go 顶部的 emailAccount）", err)
	}

	// 应用图标：可选。由宿主注入字节，库在**同源**路径下提供给页面与浏览器标签页。
	var appIcon []byte
	if *iconPath != "" {
		raw, err := os.ReadFile(*iconPath)
		if err != nil {
			log.Fatalf("读取图标失败: %v", err)
		}
		appIcon = raw
	}

	cfg := uc.Config{
		DBPath:              *dir,
		AppIcon:             appIcon,
		ServiceName:         *serviceName,
		PublicBaseURL:       "http://" + *publicAddr,
		InvitePath:          *mountPath + "/invite",
		TimeZone:            *timeZone,
		BootstrapAdminEmail: *adminEmail,
		// 演示用的固定密钥；真实部署请让宿主注入 32 字节随机值。
		HMACKey:    []byte("demo-only-hmac-key-please-replace"),
		UAHashSalt: "demo-salt",
		Hooks: uc.Hooks{
			MailSent: func(to, tmpl string) {
				log.Printf("已发送: %s (%s)", to, tmpl)
			},
			MailFailed: func(to, tmpl string, err error, attempts int, final bool) {
				log.Printf("发送失败: %s (%s) 第 %d 次 final=%v: %v", to, tmpl, attempts, final, err)
			},
			WorkerError: func(err error) { log.Printf("邮件队列: %v", err) },
			HTTPError:   func(err error) { log.Printf("http: %v", err) },
		},
	}

	// 有真实账号就用真实 SMTP，否则退化为"打印到终端"。
	if emailAccount.enabled() {
		cfg.Mail = emailAccount.mailConfig(*serviceName)
		cfg.WorkerInterval = 5 * time.Second
		log.Printf("邮件模式: 真实 SMTP（%s）", emailAccount.describe())
	} else {
		cfg.Mailer = stdoutMailer{}
		// 打印模式把轮询间隔缩短，免得每次都要等 10 秒。
		cfg.WorkerInterval = time.Second
		log.Printf("邮件模式: 仅打印到终端（如需真实发信，请填写 examples/demo/main.go 顶部的 emailAccount）")
	}

	// 配置了真实账号时，启动就先做一次"连接 + 认证"自检，把问题暴露在启动阶段。
	if emailAccount.enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		verifyErr := uc.VerifySMTP(ctx, cfg.Mail)
		cancel()
		if verifyErr != nil {
			log.Printf("⚠ SMTP 自检未通过（服务仍会启动；可先在管理界面复制邀请链接人工送达）：%v", verifyErr)
		} else {
			log.Printf("SMTP 自检通过：%s", emailAccount.describe())
		}
	}

	instance, err := uc.New(cfg)
	if err != nil {
		log.Fatalf("user-center: %v", err)
	}
	defer func() { _ = instance.Close() }()

	// 时区提示：邮件/页面里的时间按哪个时区显示。写错了不会有人报障，
	// 只会被默默误解（"我中午登录的，怎么邮件写着凌晨 4 点"），所以启动就打出来。
	if loc := instance.DisplayTimeZone(); loc != nil {
		log.Printf("时间显示时区: %s（现在是 %s）", loc, time.Now().In(loc).Format("2006-01-02 15:04:05 -07:00"))
		if loc == time.UTC {
			log.Printf("提示：当前按 UTC 显示时间；若你在东八区，请用 -tz Asia/Shanghai（或给宿主机设置 TZ）")
		}
	}

	// 引导结果：管理员是谁、是否已设置用户名、本次是否发了"设置用户名"的邮件
	if res := instance.BootstrapResult(); res != nil && res.Email != "" {
		switch {
		case res.UIDSet && res.Action == "noop":
			log.Printf("管理员 %s 已就绪（用户名已设置）", res.Email)
		case res.InviteSent:
			log.Printf("已向管理员 %s 发送「设置用户名」邮件，请在 7 天内点击其中的链接", res.Email)
		case res.ExistingInvite:
			log.Printf("管理员 %s 仍有一条未过期的「设置用户名」链接，本次未重复发送", res.Email)
		default:
			log.Printf("管理员 %s（%s），尚未设置用户名，可在管理界面重新生成链接", res.Email, res.Action)
		}
	}

	// -test-mail：只验证 SMTP 配置是否正确，发完即退出。
	if *selfTest {
		if !emailAccount.enabled() {
			log.Fatal("-test-mail 需要先在 examples/demo/main.go 顶部填写 emailAccount")
		}
		if err := uc.SendTestMail(context.Background(), cfg.Mail, *adminEmail); err != nil {
			log.Fatalf("SMTP 自检失败: %v", err)
		}
		log.Printf("SMTP 自检通过：已向 %s 发出一封测试邮件（请检查收件箱与垃圾箱）", *adminEmail)
		return
	}

	publicApp := fiber.New(fiber.Config{DisableStartupMessage: true})
	if _, err := instance.RegisterPublic(publicApp.Group(*mountPath), *mountPath); err != nil {
		log.Fatal(err)
	}

	adminApp := fiber.New(fiber.Config{DisableStartupMessage: true})
	if _, err := instance.RegisterAdmin(adminApp.Group("/admin"), "/admin"); err != nil {
		log.Fatal(err)
	}

	go func() {
		log.Printf("公开面:  http://%s%s/", *publicAddr, *mountPath)
		if err := publicApp.Listen(*publicAddr); err != nil {
			log.Printf("公开面退出: %v", err)
		}
	}()
	go func() {
		log.Printf("管理面:  http://%s/admin/  (仅本机可访问)", *adminAddr)
		if err := adminApp.Listen(*adminAddr); err != nil {
			log.Printf("管理面退出: %v", err)
		}
	}()

	// 周期性清理过期数据
	maintenance := time.NewTicker(6 * time.Hour)
	defer maintenance.Stop()
	go func() {
		for range maintenance.C {
			if err := instance.Maintenance(context.Background()); err != nil {
				log.Printf("maintenance: %v", err)
			}
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("正在退出…")
	_ = publicApp.Shutdown()
	_ = adminApp.Shutdown()
}
