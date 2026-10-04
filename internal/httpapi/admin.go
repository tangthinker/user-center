package httpapi

import (
	"errors"
	"html"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/v2/internal/app"
	"github.com/tangthinker/user-center/v2/internal/domain"
	"github.com/tangthinker/user-center/v2/internal/webui"
)

// DefaultAdminMountPath 是管理面的默认挂载前缀。
const DefaultAdminMountPath = "/admin"

// defaultAllowedHosts 是管理面允许的 Host 主机名（不含端口）。
//
// 这是"挡 DNS rebinding"的那一层：即使请求来自本机，Host 若指向外部域名
// 也应拒绝。
var defaultAllowedHosts = []string{"127.0.0.1", "localhost", "::1", "[::1]"}

// AdminConfig 是管理面配置。
type AdminConfig struct {
	// MountPath 用于生成页面里的 API 前缀，必须与宿主实际挂载位置一致。
	MountPath string

	// AllowedHosts 覆盖默认的 Host 白名单（不含端口）。
	AllowedHosts []string

	// Icon 是宿主提供的应用图标；为空时界面用服务名首字作为标识。
	Icon *Icon

	// OnError 接收未预期的内部错误（宿主用它写日志/告警）。
	OnError func(error)
}

func (c AdminConfig) withDefaults() AdminConfig {
	if c.MountPath == "" {
		c.MountPath = DefaultAdminMountPath
	}
	c.MountPath = "/" + strings.Trim(c.MountPath, "/")
	if len(c.AllowedHosts) == 0 {
		c.AllowedHosts = defaultAllowedHosts
	}
	return c
}

// Admin 是管理面处理器。
type Admin struct {
	app   *app.App
	cfg   AdminConfig
	index string
	js    []byte
	style []byte
}

// NewAdmin 构造管理面处理器。
func NewAdmin(a *app.App, cfg AdminConfig) (*Admin, error) {
	if a == nil {
		return nil, errors.New("httpapi: nil app")
	}
	cfg = cfg.withDefaults()

	index, err := webui.AdminIndex()
	if err != nil {
		return nil, err
	}
	js, err := webui.AdminJS()
	if err != nil {
		return nil, err
	}
	style, err := webui.StyleCSS()
	if err != nil {
		return nil, err
	}
	return &Admin{app: a, cfg: cfg, index: string(index), js: js, style: style}, nil
}

// Register 把管理面注册到给定路由器上。
//
// **宿主必须把它挂到只绑回环地址的监听上**（§3.1）。即便如此，本处理器仍会
// 独立做一次本地校验（fail-closed），避免"误挂到公网监听"直接导致管理面暴露。
func (a *Admin) Register(router fiber.Router) {
	router.Use(a.LocalOnly())
	router.Use(SameOriginOnly())

	router.Get("/", a.handleIndex)
	router.Get("/assets/style.css", a.serveCSS)
	router.Get("/assets/app.js", a.serveJS)
	// 图标与页面同源提供：宿主的静态资源在另一个监听上时，外链会被本页 CSP 拦掉
	router.Get("/assets/icon", a.serveIcon)

	// 登录相关：不需要已有管理会话，但同样受本地/同源限制
	router.Post("/otp/send", a.handleSendCode)
	router.Post("/otp/verify", a.handleVerifyCode)

	// 需要管理会话的路由：用包装函数显式声明，避免依赖中间件注册顺序
	auth := a.withAdmin
	router.Post("/logout", auth(a.handleLogout))
	router.Get("/stats", auth(a.handleStats))
	router.Get("/users", auth(a.handleListUsers))
	router.Post("/users", auth(a.handleCreateUser))
	router.Post("/users/:id/invite/resend", auth(a.handleResendInvite))
	router.Post("/users/:id/invite/link", auth(a.handleRegenerateLink))
	router.Post("/users/:id/disable", auth(a.handleDisable))
	router.Post("/users/:id/enable", auth(a.handleEnable))
	router.Post("/users/:id/email", auth(a.handleChangeEmail))
	router.Post("/users/:id/sessions/revoke", auth(a.handleRevokeSessions))
	router.Get("/users/:id/sessions", auth(a.handleListSessions))
	router.Delete("/users/:id", auth(a.handleDeleteUser))
	router.Get("/audit", auth(a.handleAudit))
}

// withAdmin 把处理器包上"必须携带有效管理会话"的前置检查。
//
// 注意：Fiber 的处理器链以"返回 nil"表示**继续**执行后续处理器，因此
// 前置检查不能靠"写完响应再 return nil"来中断——那样业务处理器仍会被调用
// （并且用 200 覆盖掉刚才写的 401）。这里用布尔返回值显式区分"已拒绝"与"放行"。
func (a *Admin) withAdmin(h fiber.Handler) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if !a.requireAdmin(c) {
			// 响应已由 requireAdmin 写好；本处理器是链上唯一处理器，返回 nil 即终止。
			return nil
		}
		return h(c)
	}
}

// LocalOnly 是"仅允许本机访问"的硬兜底。
//
// ⚠ 判定**只认 `RemoteAddr`**，绝不读取 `X-Forwarded-For` / `X-Real-IP`：
// 本库运行在宿主进程内，`c.IP()` 的取值取决于宿主的 Fiber 代理配置
// （`EnableTrustedProxyCheck` / `ProxyHeader`），库既无法控制也无法校验。
// 若用 `c.IP()` 判定，任何配了 `ProxyHeader` 却没开 `EnableTrustedProxyCheck`
// 的宿主，都会被 `X-Forwarded-For: 127.0.0.1` 直接穿透。
func (a *Admin) LocalOnly() fiber.Handler {
	return func(c *fiber.Ctx) error {
		remote := ""
		if addr := c.Context().RemoteAddr(); addr != nil {
			remote = addr.String()
		}
		host := c.Hostname()
		if !allowLocal(remote, host, a.cfg.AllowedHosts) {
			_ = failStatus(c, http.StatusForbidden, msgForbidden)
			return nil // 已写响应：返回 nil 即终止链（不再调用 Next）
		}
		return c.Next()
	}
}

// allowLocal 是纯粹的判定函数（便于单测覆盖各种地址形态）。
func allowLocal(remoteAddr, host string, allowedHosts []string) bool {
	if !isLoopbackAddr(remoteAddr) {
		return false
	}
	return hostAllowed(host, allowedHosts)
}

func isLoopbackAddr(remoteAddr string) bool {
	if remoteAddr == "" {
		return false
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	for _, a := range allowed {
		if strings.ToLower(strings.Trim(strings.TrimSpace(a), "[]")) == host {
			return true
		}
	}
	return false
}

// SameOriginOnly 拒绝跨源请求。
//
// 管理界面不使用 cookie，因此本来就没有 CSRF 面；这一层是纵深防御：
// 浏览器以外的客户端通常不带 Origin，直接放行；带了就必须与 Host 同源。
func SameOriginOnly() fiber.Handler {
	return func(c *fiber.Ctx) error {
		origin := c.Get(fiber.HeaderOrigin)
		if origin == "" {
			return c.Next()
		}
		u, err := url.Parse(origin)
		if err != nil {
			_ = failStatus(c, http.StatusForbidden, msgForbidden)
			return nil
		}
		if !strings.EqualFold(u.Host, c.Get(fiber.HeaderHost)) {
			_ = failStatus(c, http.StatusForbidden, msgForbidden)
			return nil
		}
		return c.Next()
	}
}

// requireAdmin 要求请求携带有效的**管理会话**令牌；返回 false 表示已拒绝
// 并已写好响应。
//
// 令牌走 Authorization 头（不使用 cookie），与设计 §8.3 一致。
func (a *Admin) requireAdmin(c *fiber.Ctx) bool {
	token := bearerToken(c)
	if token == "" {
		_ = failStatus(c, http.StatusUnauthorized, msgSessionInvalid)
		return false
	}
	sess, err := a.app.VerifySession(c.UserContext(), token)
	if err != nil {
		_ = failStatus(c, http.StatusUnauthorized, msgSessionInvalid)
		return false
	}
	if sess.Scope != domain.ScopeAdmin {
		_ = failStatus(c, http.StatusForbidden, msgForbidden)
		return false
	}
	c.Locals(ctxKeyAdminSession, sess)
	return true
}

const ctxKeyAdminSession = "uc.admin.session"

func bearerToken(c *fiber.Ctx) string {
	auth := c.Get(fiber.HeaderAuthorization)
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return strings.TrimSpace(auth[len(prefix):])
	}
	return ""
}

func (a *Admin) actor(c *fiber.Ctx) app.Actor {
	sess, _ := c.Locals(ctxKeyAdminSession).(*domain.Session)
	actor := app.Actor{IP: c.IP(), UA: c.Get(fiber.HeaderUserAgent)}
	if sess != nil {
		actor.ID = sess.UserID
		actor.Email = sess.UID
	}
	return actor
}

// --- 页面与静态资源 ---

func (a *Admin) handleIndex(c *fiber.Ctx) error {
	setPageHeaders(c)
	c.Set(fiber.HeaderContentSecurityPolicy, pageCSP)
	name := a.app.ServiceName()
	assets := a.cfg.MountPath + "/assets"
	return c.Type("html").SendString(render(a.index, map[string]string{
		"{{API}}":             a.cfg.MountPath,
		"{{ASSETS}}":          assets,
		"{{SERVICE_NAME}}":    html.EscapeString(name),
		"{{SERVICE_INITIAL}}": html.EscapeString(initialOf(name)),
		"{{FAVICON}}":         faviconTag(a.cfg.Icon, assets),
		"{{APP_MARK}}":        appMarkTag(a.cfg.Icon, assets, 64, "app-mark", initialOf(name)),
		"{{APP_MARK_SM}}":     appMarkTag(a.cfg.Icon, assets, 26, "toolbar__mark", initialOf(name)),
	}))
}

func (a *Admin) serveCSS(c *fiber.Ctx) error {
	c.Set(fiber.HeaderContentType, "text/css; charset=utf-8")
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Send(a.style)
}

// serveIcon 提供宿主配置的应用图标。
//
// nosniff + 长缓存：图标在进程生命周期内不变，且必须是浏览器认定的图片类型。
func (a *Admin) serveIcon(c *fiber.Ctx) error {
	return serveIconBytes(c, a.cfg.Icon)
}

func (a *Admin) serveJS(c *fiber.Ctx) error {
	c.Set(fiber.HeaderContentType, "application/javascript; charset=utf-8")
	c.Set(fiber.HeaderCacheControl, "no-store")
	return c.Send(a.js)
}

// --- 登录 ---

func (a *Admin) handleSendCode(c *fiber.Ctx) error {
	var req emailReq
	if err := c.BodyParser(&req); err != nil {
		return failStatus(c, http.StatusBadRequest, "请求格式不正确")
	}
	if _, err := a.app.RequestAdminLoginCode(c.UserContext(), req.Email, c.IP()); err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{"sent": true, "message": msgOTPSent})
}

func (a *Admin) handleVerifyCode(c *fiber.Ctx) error {
	var req verifyReq
	if err := c.BodyParser(&req); err != nil {
		return failStatus(c, http.StatusBadRequest, "请求格式不正确")
	}
	res, err := a.app.VerifyAdminCode(c.UserContext(), req.Email, req.Code, c.IP(), c.Get(fiber.HeaderUserAgent))
	if err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{"token": res.Token, "uid": res.UID})
}

func (a *Admin) handleLogout(c *fiber.Ctx) error {
	if err := a.app.Logout(c.UserContext(), bearerToken(c)); err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{"logged_out": true})
}

// --- 数据接口 ---

// adminUser 是管理接口返回的用户视图（显式字段，避免泄漏内部结构）。
type adminUser struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	UID         string `json:"uid"`
	Status      string `json:"status"`
	IsAdmin     bool   `json:"is_admin"`
	CreatedAt   string `json:"created_at"`
	LastLoginAt string `json:"last_login_at"`
	// OnlineDevices 是该用户当前在线设备数（按设备指纹归并，不是会话数）。
	OnlineDevices int `json:"online_devices"`
}

func toAdminUser(u domain.User) adminUser {
	out := adminUser{
		ID:        u.ID,
		Email:     u.Email,
		UID:       u.UIDValue(),
		Status:    u.Status,
		IsAdmin:   u.IsAdmin,
		CreatedAt: utcStamp(u.CreatedAt),
	}
	if u.LastLoginAt != nil {
		out.LastLoginAt = utcStamp(*u.LastLoginAt)
	}
	return out
}

func (a *Admin) handleStats(c *fiber.Ctx) error {
	stats, err := a.app.AdminStats(c.UserContext(), a.actor(c))
	if err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{
		"users_by_status": stats.UsersByStatus,
		"admins":          stats.Admins,
		"queue": fiber.Map{
			"pending": stats.Queue.Pending,
			"sending": stats.Queue.Sending,
			"sent":    stats.Queue.Sent,
			"failed":  stats.Queue.Failed,
		},
		"mails_last_24h": stats.MailsLast24h,
	})
}

func (a *Admin) handleListUsers(c *fiber.Ctx) error {
	limit, offset := pageParams(c)
	users, err := a.app.AdminListUsers(c.UserContext(), a.actor(c), limit, offset)
	if err != nil {
		return fail(c, err, a.cfg.OnError)
	}

	// 在线设备数一次分组查询取回，避免逐行查询（用户列表最多 500 行）。
	ids := make([]int64, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	counts, err := a.app.AdminOnlineDeviceCounts(c.UserContext(), ids)
	if err != nil {
		return fail(c, err, a.cfg.OnError)
	}

	out := make([]adminUser, 0, len(users))
	for _, u := range users {
		item := toAdminUser(u)
		item.OnlineDevices = counts[u.ID]
		out = append(out, item)
	}
	return ok(c, fiber.Map{"users": out, "limit": limit, "offset": offset})
}

func (a *Admin) handleCreateUser(c *fiber.Ctx) error {
	var req emailReq
	if err := c.BodyParser(&req); err != nil {
		return failStatus(c, http.StatusBadRequest, "请求格式不正确")
	}
	res, err := a.app.AdminCreateUser(c.UserContext(), a.actor(c), req.Email)
	if err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{
		"user":        toAdminUser(*res.User),
		"invite_url":  a.app.InviteURL(res.InviteToken),
		"expires_at":  res.ExpiresAt,
		"mail_queued": res.MailQueued,
	})
}

func (a *Admin) handleResendInvite(c *fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return failStatus(c, http.StatusBadRequest, "无效的用户 ID")
	}
	res, err := a.app.AdminResendInvite(c.UserContext(), a.actor(c), id)
	if err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{
		"invite_url":  a.app.InviteURL(res.InviteToken),
		"expires_at":  res.ExpiresAt,
		"mail_queued": res.MailQueued,
	})
}

func (a *Admin) handleRegenerateLink(c *fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return failStatus(c, http.StatusBadRequest, "无效的用户 ID")
	}
	res, err := a.app.AdminRegenerateInviteLink(c.UserContext(), a.actor(c), id)
	if err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{
		"invite_url": a.app.InviteURL(res.InviteToken),
		"expires_at": res.ExpiresAt,
		"notice":     "旧链接已立即失效",
	})
}

func (a *Admin) handleDisable(c *fiber.Ctx) error { return a.setStatus(c, domain.UserStatusDisabled) }
func (a *Admin) handleEnable(c *fiber.Ctx) error  { return a.setStatus(c, domain.UserStatusActive) }

func (a *Admin) setStatus(c *fiber.Ctx, status string) error {
	id, err := pathID(c)
	if err != nil {
		return failStatus(c, http.StatusBadRequest, "无效的用户 ID")
	}
	if err := a.app.AdminSetUserStatus(c.UserContext(), a.actor(c), id, status); err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{"status": status})
}

func (a *Admin) handleChangeEmail(c *fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return failStatus(c, http.StatusBadRequest, "无效的用户 ID")
	}
	var req emailReq
	if err := c.BodyParser(&req); err != nil {
		return failStatus(c, http.StatusBadRequest, "请求格式不正确")
	}
	if err := a.app.AdminChangeEmail(c.UserContext(), a.actor(c), id, req.Email); err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{"changed": true})
}

func (a *Admin) handleRevokeSessions(c *fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return failStatus(c, http.StatusBadRequest, "无效的用户 ID")
	}
	n, err := a.app.AdminRevokeSessions(c.UserContext(), a.actor(c), id)
	if err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{"revoked": n})
}

// handleListSessions 返回某用户的**在线设备**。
//
// 一个"设备"= 同一 (scope, 设备指纹) 下的全部在线的会话：验证码登录每次都会
// 签发新会话且不吊销旧的，直接列会话会把"一台手机"显示成"十台设备"（§5.4）。
func (a *Admin) handleListSessions(c *fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return failStatus(c, http.StatusBadRequest, "无效的用户 ID")
	}
	devices, err := a.app.AdminListSessions(c.UserContext(), a.actor(c), id)
	if err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	out := make([]fiber.Map, 0, len(devices))
	for _, d := range devices {
		out = append(out, fiber.Map{
			"scope":          d.Scope,
			"type":           d.Type,
			"model":          d.Model,
			"os":             d.OS,
			"browser":        d.Browser,
			"ip":             d.IP,
			"sessions":       d.Sessions,
			"login_at":       utcStamp(d.LoginAt),
			"first_login_at": utcStamp(d.FirstLoginAt),
			"last_seen":      utcStamp(d.LastSeenAt),
		})
	}
	return ok(c, fiber.Map{"devices": out})
}

func (a *Admin) handleDeleteUser(c *fiber.Ctx) error {
	id, err := pathID(c)
	if err != nil {
		return failStatus(c, http.StatusBadRequest, "无效的用户 ID")
	}
	if err := a.app.AdminDeleteUser(c.UserContext(), a.actor(c), id); err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	return ok(c, fiber.Map{"deleted": true})
}

func (a *Admin) handleAudit(c *fiber.Ctx) error {
	limit, offset := pageParams(c)
	entries, err := a.app.AdminAuditLog(c.UserContext(), a.actor(c), limit, offset)
	if err != nil {
		return fail(c, err, a.cfg.OnError)
	}
	out := make([]fiber.Map, 0, len(entries))
	for _, e := range entries {
		item := fiber.Map{
			"created_at":  utcStamp(e.CreatedAt),
			"actor_email": e.ActorEmail,
			"action":      e.Action,
		}
		if e.Target != nil {
			item["target"] = *e.Target
		}
		if e.IP != nil {
			item["ip"] = *e.IP
		}
		// 注意：detail 可能含内部字段，管理界面不需要它，这里刻意不返回。
		out = append(out, item)
	}
	return ok(c, fiber.Map{"entries": out, "limit": limit, "offset": offset})
}

// utcStamp 是接口里所有时间字段的唯一格式：RFC 3339 / UTC。
//
// 一律返回 UTC 让调用方（浏览器）按**客户端自己的**时区显示；早先这里写过
// 本地墙上时间，管理界面上就会凭空少 8 小时（见 CreateUserResult.ExpiresAt 的注释）。
func utcStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func pathID(c *fiber.Ctx) (int64, error) {
	return strconv.ParseInt(c.Params("id"), 10, 64)
}

func pageParams(c *fiber.Ctx) (limit, offset int) {
	limit, _ = strconv.Atoi(c.Query("limit"))
	offset, _ = strconv.Atoi(c.Query("offset"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
