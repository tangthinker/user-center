# user-center v2

一个**被宿主服务内嵌**的用户中心库：邮箱验证码登录、管理员邀请制、会话可吊销、管理面仅限本机。

- 认证方式：**邮箱一次性验证码**，全系统不存储任何口令
- 用户来源：**仅管理员邀请**（用户自己设置 uid，激活后方可登录）
- 会话：只存 `sha256(token)`、双过期（idle + absolute）、可即时吊销
- 存储：每个宿主一份 SQLite(WAL)，库内自带版本化迁移
- 管理面：内嵌静态界面，**只允许本机访问**（库侧 fail-closed 兜底）
- 形态：Go 依赖库，按 tag 版本被宿主引用；**不提供独立服务二进制**

> 设计文档见 [`docs/auth-redesign.md`](docs/auth-redesign.md)，开发过程与决策记录见
> [`docs/dev-log/v2.0.0.md`](docs/dev-log/v2.0.0.md)。

---

## 1. 快速开始

```go
package main

import (
	"log"
	"os"

	"github.com/gofiber/fiber/v2"
	uc "github.com/tangthinker/user-center/v2"
)

func main() {
	instance, err := uc.New(uc.Config{
		DBPath:              "/var/lib/svc-a",                 // 目录，或 ".../custom.db"
		ReservedUIDs:        []string{"云盘小助手"},             // 可选：追加你自己的保留用户名
		AppIcon:             iconPNG,                          // 可选：管理界面/落地页的应用图标（[]byte）
		ServiceName:         "云盘",                            // 出现在邮件主题与正文
		PublicBaseURL:       "https://files.example.com",      // 该宿主自己的域名
		InvitePath:          "/api/v1/invite",                 // 必须与下面挂载点一致
		BootstrapAdminEmail: "ops@example.com",                // 仅首次生效（幂等）
		HMACKey:             []byte(os.Getenv("UC_HMAC_KEY")), // ≥16 字节，建议 32
		UAHashSalt:          os.Getenv("UC_UA_HASH_SALT"),
		Mail: &uc.MailConfig{
			Host:        "smtp.qiye.aliyun.com",
			Port:        465,
			ImplicitTLS: true,
			Username:    "noreply@example.com",
			Password:    os.Getenv("UC_SMTP_PASS"), // 三方客户端安全密码
			From:        "noreply@example.com",
			FromName:    "云盘",
		},
	})
	if err != nil {
		log.Fatalf("user-center: %v", err)
	}
	defer func() { _ = instance.Close() }()

	// ① 公共面：挂在公网监听上（经 Caddy 反代）
	publicApp := fiber.New()
	if _, err := instance.RegisterPublic(publicApp.Group("/api/v1"), "/api/v1"); err != nil {
		log.Fatal(err)
	}
	go func() { _ = publicApp.Listen("127.0.0.1:9999") }()

	// ② 管理面：挂在**只绑回环**的第二个监听上
	adminApp := fiber.New()
	if _, err := instance.RegisterAdmin(adminApp.Group("/admin"), "/admin"); err != nil {
		log.Fatal(err)
	}
	log.Fatal(adminApp.Listen("127.0.0.1:9998"))
}
```

保护宿主自己的资源：

```go
api := app.Group("/api/v1/storage", func(c *fiber.Ctx) error {
	token := strings.TrimPrefix(c.Get("Authorization"), "Bearer ")
	uid, err := instance.VerifyToken(token) // 只接受普通用户会话
	if err != nil {
		return c.Status(fiber.StatusForbidden).SendString("Forbidden")
	}
	c.Locals("uid", uid)
	return c.Next()
})
```

可直接跑起来的版本：`go run ./examples/demo -admin-email you@example.com`

演示程序支持两种发信模式，**在 `examples/demo/main.go` 顶部的 `emailAccount` 里直接改**（演示用，不需要环境变量）：

```go
var emailAccount = emailSettings{
	Server:   "smtp.qiye.aliyun.com",   // 留空则退化为"打印到终端"
	Port:     465,
	Username: "noreply@your-domain.com",
	Password: "你的三方客户端安全密码",
	From:     "",                        // 留空使用 Username
	FromName: "演示服务",
}
```

演示程序在启动时会先做两件事：**本地配置检查**（用户名是否为完整邮箱地址、端口是否为 465 等，
写错立刻退出并说明原因）与 **SMTP 连接+认证自检**（失败只告警、不阻塞启动，便于你仍能进管理界面
复制邀请链接人工送达）。

- **留空**：邮件打印到终端（含验证码与邀请链接），无需任何邮箱即可走通全链路；
- **填好**：走真实 SMTP 发信。先用自检确认配置是否正确：

  ```bash
  go run ./examples/demo -admin-email you@example.com -test-mail
  ```

  它会实际投递一封测试邮件——既验证连通性与凭据，也验证它是否落进了垃圾箱
  （SPF/DKIM/DMARC 是否生效）。同样地，宿主代码里也可以用
  `uc.VerifySMTP(ctx, &cfg)`（只连接认证、不发信）与 `uc.SendTestMail(ctx, &cfg, to)`。

> ⚠ 演示程序把口令写在代码里只为方便本地试跑，**不要把填好口令的文件提交到仓库**；
> 真实部署请让宿主从配置/密钥管理注入。

---

## 2. 公开面接口

| 方法 | 路径（默认挂载 `/api/v1`） | 说明 |
|---|---|---|
| GET | `/invite?token=…` | 邀请落地页（服务端渲染）。**只读，不消费 token** |
| POST | `/invite/check-uid` | `{token, uid}` → `{available}`（每 token 限 20 次） |
| POST | `/invite/accept` | `{token, uid}` → 激活（不发放会话） |
| POST | `/otp/send` | `{email}` → 统一响应（未知邮箱也一样） |
| POST | `/otp/verify` | `{email, code}` → `{token, uid}` |
| POST | `/session/verify` | `{token}` → `{uid, scope}` |
| POST | `/session/logout` | `{token}` → 吊销当前会话 |

响应统一为 `{"code":0,"msg":"success","data":{…}}`，**并同时返回正确的 HTTP 状态码**：

| 场景 | HTTP |
|---|---|
| 成功 | 200 |
| 邮箱/验证码不正确 | 401 |
| 会话无效/过期/已吊销 | 401 |
| uid 已被占用 / 邮箱已被占用 | 409 |
| 邀请链接无效、过期或已使用 | 410 |
| uid 不合规、邮箱格式错误 | 400（uid 是唯一会解释细节的地方） |
| 触发限流 | 429 + `Retry-After` |
| 服务端异常 | 500（文案统一，**不回显内部错误**） |

---

## 3. 管理面接口

挂在只绑回环的监听上（例如 `127.0.0.1:9998/admin`）。界面已内嵌，直接用浏览器打开即可。

登录：`POST /admin/otp/send` → `POST /admin/otp/verify` 取回令牌；之后所有接口用
`Authorization: Bearer <token>`（**不使用 cookie**，因此没有 CSRF 面）。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/users` | 用户列表 |
| POST | `/admin/users` | `{email}` → 建用户并发送邀请，返回 `invite_url` |
| POST | `/admin/users/:id/invite/resend` | 重发邀请（旧链接立即失效） |
| POST | `/admin/users/:id/invite/link` | **重新生成**链接供复制（不发邮件，提示旧链接失效） |
| POST | `/admin/users/:id/disable` / `enable` | 停用（同时吊销全部会话）/ 启用 |
| POST | `/admin/users/:id/email` | 改邮箱（通知旧地址 + 吊销全部会话） |
| POST | `/admin/users/:id/sessions/revoke` | 踢下线 |
| GET | `/admin/users/:id/sessions` | 会话列表 |
| DELETE | `/admin/users/:id` | 删除用户 |
| GET | `/admin/audit` | 审计（不返回 detail，避免误带内部字段） |
| GET | `/admin/stats` | 用户/队列/发信量概览 |

### 内嵌界面

管理界面与邀请落地页由库自带（`internal/webui`，经 `embed.FS` 打进二进制），因此宿主不需要构建前端。

**应用图标**：`Config.AppIcon` 传图标的原始字节（PNG/JPEG/WebP/GIF/ICO，≤256 KiB），
库会在**同源**路径下提供它，同时用作浏览器标签页图标与界面标识：

```go
iconPNG, _ := os.ReadFile("icon.png")
uc.New(uc.Config{ /* … */ AppIcon: iconPNG })
```

- 不配置时自动回退为**服务名首字**的方块标识；
- 库在同源路径提供（`<管理面>/assets/icon`、`<公开面>/invite-assets/icon`），
  因此**不需要**把宿主的静态资源域名写进 CSP——把图标挂在另一个监听上的外链会被 CSP 拦掉；
- **不支持 SVG**：SVG 可携带脚本，而图标是会被直接打开的地址；非法图标在 `uc.New` 时就会报错，不会拖到浏览器请求时才暴露。

设计语言是 **macOS / iOS 系统风**（用户中心与宿主 `seven-eleven-server` 的后台保持一致的观感）：
系统字体栈、`--label`/`--separator` 语义化灰阶、Apple 蓝 `#0071e3`、13px 密度与 30px 控件、
6px 控件圆角、半透明材质工具栏（`saturate(180%) blur(20px)`）、sheet 式对话框。

- **零外部资源**：无 CDN、无第三方字体、无追踪像素，可放心用严格 CSP
  （`default-src 'none'; script-src 'self'; style-src 'self'`）；
- **不使用 cookie**：令牌只存页面内存，随 `Authorization` 头发送 ⇒ 没有 CSRF 面；
- **明暗外观跟随系统**，不提供应用内开关；尊重「减弱动态效果」与「增强对比度」；
- 正文对比度：浅色 17.9:1、深色 14.7:1；控件描边 3.25:1（满足 WCAG 1.4.11）；
- **界面自报家门**：标题、顶栏与首字标识都用宿主配置的 `ServiceName`——本库的常态是"每个宿主一套后台"，
  管理员要一眼看出自己在管哪一项服务；
- 键盘与读屏可用：显式焦点环、原生 `<form>` 回车提交、操作后**焦点自动回到原按钮**、
  行内按钮的 `aria-label` 带上目标邮箱（否则一行 6 个同名按钮无法区分）、
  取数时置 `aria-busy`、提示条可键盘关闭；破坏性操作走原生 `<dialog>`（有取消与后果说明）；
- 文字随系统字号缩放（字号与控件高度均为 `rem`，默认 16px 下与参考实现的 13px/30px 等价），窄屏重排到 320px。

宿主若要自定义文案与品牌，替换自己的静态资源即可（`PublicHandler`/`AdminHandler` 只提供 JSON 接口与页面路由）。

---

## 4. 部署要求（Caddy 反代）

```caddyfile
id.example.com {
    encode zstd gzip
    header {
        Strict-Transport-Security "max-age=31536000; includeSubDomains"
        X-Content-Type-Options    nosniff
        Referrer-Policy           no-referrer
        -Server
    }
    # 管理面绝不代理
    @admin path /admin/*
    respond @admin 404

    reverse_proxy 127.0.0.1:9999 {
        # 覆盖式写入真实客户端 IP（不要追加）
        header_up X-Real-IP {http.request.remote.host}
        header_up X-Forwarded-For {http.request.remote.host}
    }
}
```

宿主的 Fiber 配置（**必读**，否则按 IP 限流与真实 IP 判定都会被伪造头绕过）：

```go
fiber.New(fiber.Config{
    EnableTrustedProxyCheck: true,
    TrustedProxies:          []string{"127.0.0.1"}, // Caddy 所在主机
    ProxyHeader:             "X-Real-IP",
    EnableIPValidation:      true,
})
```

其它要求：

1. 公网端口只对 Caddy 开放（否则 TLS、限流、日志全部可被绕过）。
2. 管理端口**不要**加任何反向代理：`LocalOnly` 只认 `RemoteAddr`，若前面再挂一层本机反代，所有请求都会看起来来自回环。
3. **不要在 `/invite` 路径上记录访问日志**：Caddy 默认日志含查询串，会把邀请 token 写进日志。

---

## 5. 阿里企业邮箱（SMTP）

| 项 | 值 |
|---|---|
| 服务器 | `smtp.qiye.aliyun.com`（旧地址 `smtp.mxhichina.com` 仍可用） |
| 端口 | **465（SSL，推荐）**；25 为非加密；**80/587 未开通** |
| 用户名 | 完整邮箱地址 |
| 口令 | **三方客户端安全密码**（不是登录密码） |

两个必须先做的后台设置（否则会一直报"用户名或密码错误"）：

1. 邮箱管理员为该账号开启「**第三方客户端登录权限**」；
2. 开启该账号 **POP3/IMAP 权限**，并设置「**三方客户端安全密码**」。

DNS（到达率的关键；MX 应已随域名绑定完成）：SPF
`v=spf1 include:spf.qiye.aliyun.com -all`、后台生成的 DKIM 记录、DMARC
`v=DMARC1; p=none; rua=mailto:dmarc@你的域名`（观察两周后升级到 `quarantine`/`reject`）。

配额：社区口径为每账号 200 封/日（官方未明确，**上线前实测**）。本库只需要极低的发信量：
一个用户从邀请到首次登录共 3 封，之后每次登录 1 封。

### 发信失败排查

先跑自检（只做连接+认证，不发信）：`uc.VerifySMTP(ctx, &cfg)`；
或发一封真实测试邮件：`uc.SendTestMail(ctx, &cfg, "你的邮箱")`
（演示程序对应 `go run ./examples/demo -admin-email you@example.com -test-mail`）。

| 现象 | 原因与处置 |
|---|---|
| `526 Authentication failure`（阿里邮箱） | 官方列出的四个原因：① **SMTP 用户名必须是完整发信邮箱地址**，不能有多余空格；② **SMTP 密码**——*未开启*三方客户端安全密码时用**邮箱登录密码**，*已开启*时必须用该功能生成的**新安全密码**；③ 服务器应为 `smtp.qiye.aliyun.com`（默认 25，加密 465）；④ **访问策略**：确认已开启 SMTP 协议、策略适用范围包含该发信地址、未限制来源 IP，且频率不超过 10 次/秒（[官方文档](https://help.aliyun.com/zh/document_detail/602363.html)） |
| `535 Error: authentication failed` | 口令不对或该服务商要求专用口令 |
| 网易/QQ 邮箱认证失败 | 必须使用邮箱设置里生成的**客户端授权码**，而不是登录密码 |
| 连接超时 / connection refused | 端口不通：阿里企业邮箱用 465，**80/587 未开通** |

> **认证失败按永久性错误处理，不再重试**（依据 RFC 5321：5xx 为永久错误）。
> 修正配置后重新触发即可（重新请求验证码 / 在管理界面重发邀请）；
> 队列里的失败记录会保留 `last_error` 供排查。

配额：社区口径为每账号 200 封/日（官方未明确，**上线前实测**）。本库只需要极低的发信量：
一个用户从邀请到首次登录共 3 封，之后每次登录 1 封。

---

## 6. 从 v1 升级

1. 改 import 路径：`github.com/tangthinker/user-center/pkg` → `.../v2/pkg`
   （Go modules 要求主版本 ≥2 带 `/v2` 后缀）。
2. `RegisterUserCenter(router, dir)` 的**调用语句无需修改**（返回值可忽略），但建议处理 error。
3. `TokenValid(token)` 签名与语义不变。
4. 必须提供配置（`pkg.Configure` 或环境变量），否则用户中心不会挂载：

   | 变量 | 说明 |
   |---|---|
   | `UC_SERVICE_NAME` | 邮件里显示的服务名 |
   | `UC_PUBLIC_BASE_URL` | 该宿主自己的站点基址（用于邀请链接） |
   | `UC_HMAC_KEY` | 验证码 HMAC 密钥，≥16 字节 |
   | `UC_MOUNT_PATH` | 公开面挂载前缀，默认 `/api/v1` |
   | `UC_ADMIN_EMAIL` | 首个管理员邮箱（幂等）；首次启动会收到「设置用户名」邮件 |
   | `UC_SMTP_*` | 邮件配置（不配置则只入队不投递，`mail_queued=false`） |

5. **破坏性变更**：`/login`、`/register`、`/modify-password`、`/uid-unique` 已移除；
   前端登录页需改为"邮箱 + 验证码"，用户由管理员在管理界面创建。

---

## 7. 运维要点

- **启动即给管理员发一封「设置用户名」邮件**：管理员账号在创建时就是 **active**（可以直接登录管理界面），
  但仍需要设置自己的用户名；库会在启动时发一封 7 天有效的一次性链接过去。
  - 重启不会重复发送——如果对方邮箱里那条链接还没过期，启动会跳过（既不刷屏，也不会把链接作废）；
  - 邮件丢了怎么办：管理员本来就**能登录**（账号是 active），进管理界面后在自己那一行点「重新生成链接」即可；
  - 也可以在 `usercenter.New` 之后读 `instance.BootstrapResult()` 打一条启动日志，知道这一步做了什么。
- **待激活用户**：管理员建完用户后，对方需在 7 天内点击邀请链接设置 uid；
  管理员自己那条链接走的是同一个页面，只是文案变成「设置用户名 / 保存用户名」。
- **复制邀请链接**的语义是"重新生成"：服务端只存 `sha256(token)`，明文无法二次取回；
  点击后旧链接立即失效。
- **踢下线**：停用用户、改邮箱、管理员手动踢出都会即时吊销该用户全部会话。
- **邮件积压**：`GET /admin/stats` 的 `queue.pending` / `queue.failed` 是最需要告警的两个数字；
  也可以通过 `Hooks.MailFailed` / `Hooks.WorkerError` 接到宿主自己的告警体系。
- **清理任务**：宿主可周期调用 `instance.Maintenance(ctx)`（清理过期验证码/邀请/会话、
  已发送邮件、限流日志与审计）。
- **备份与回滚**：备份 `user-center.db`（WAL 模式，建议用 `VACUUM INTO` 做一致性快照）。
  回滚到更早的库版本时**必须连带恢复数据库备份**——旧版本库遇到更新的 schema 会拒绝启动。

---

## 8. 库形态约束（给维护者）

本库寄生在宿主进程内，且每个宿主有自己的用户与管理员，因此：

- 不 panic、不 `os.Exit`、不接管宿主日志（用 `Hooks`）；
- 没有包级状态（可同进程多实例）；例外是 `pkg` 兼容垫片；
- 不读全局环境变量（新 API 全部走 `Config`）；
- 不自己 `Listen`（由宿主挂载）；
- 启动的 goroutine 由 `Close()` 回收；
- 同一主版本内 schema **只做加法**，旧版本库遇到更新的 schema 会拒绝启动。

发布新 tag 前的自检清单见设计文档 §12.3。
