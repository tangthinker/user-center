package httpapi

import (
	"errors"
	"html"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/v2/internal/app"
	"github.com/tangthinker/user-center/v2/internal/webui"
)

// DefaultMountPath 是公开面的默认挂载前缀（宿主实际挂在哪里就填哪里）。
const DefaultMountPath = "/api/v1"

// PublicConfig 是公开面的配置。
type PublicConfig struct {
	// MountPath 用于生成页面里的 API 前缀，必须与宿主实际挂载位置一致。
	// 它同时应等于 app.Config.InvitePath 去掉 "/invite" 之后的部分（见接入文档）。
	MountPath string

	// InvitePath / AssetsPath 是**相对**注册路径。
	InvitePath string
	AssetsPath string

	// Icon 是宿主提供的应用图标；为空时落地页用服务名首字作为标识。
	Icon *Icon

	// OnError 接收未预期的内部错误（宿主用它写日志/告警）。对外始终是通用文案。
	OnError func(error)
}

func (c PublicConfig) withDefaults() PublicConfig {
	if c.MountPath == "" {
		c.MountPath = DefaultMountPath
	}
	c.MountPath = "/" + strings.Trim(c.MountPath, "/")
	if c.InvitePath == "" {
		c.InvitePath = "/invite"
	}
	if c.AssetsPath == "" {
		c.AssetsPath = "/invite-assets"
	}
	return c
}

// Public 是公开面处理器。
type Public struct {
	app   *app.App
	cfg   PublicConfig
	page  string
	js    []byte
	style []byte
}

// NewPublic 构造公开面处理器。
func NewPublic(a *app.App, cfg PublicConfig) (*Public, error) {
	if a == nil {
		return nil, errors.New("httpapi: nil app")
	}
	cfg = cfg.withDefaults()

	page, err := webui.InvitePage()
	if err != nil {
		return nil, err
	}
	js, err := webui.InviteJS()
	if err != nil {
		return nil, err
	}
	style, err := webui.StyleCSS()
	if err != nil {
		return nil, err
	}
	return &Public{app: a, cfg: cfg, page: string(page), js: js, style: style}, nil
}

// Register 把公开面注册到给定路由器上（相对路径）。
//
// 宿主负责决定挂载前缀与监听地址；本库不做任何 Listen（L1）。
func (p *Public) Register(router fiber.Router) {
	router.Get(p.cfg.InvitePath, p.handleInvitePage)
	router.Post(p.cfg.InvitePath+"/check-uid", p.handleCheckUID)
	router.Post(p.cfg.InvitePath+"/accept", p.handleAccept)
	router.Get(p.cfg.AssetsPath+"/style.css", p.serveCSS)
	router.Get(p.cfg.AssetsPath+"/invite.js", p.serveInviteJS)
	router.Get(p.cfg.AssetsPath+"/icon", p.serveIcon)

	router.Post("/otp/send", p.handleOTPSend)
	router.Post("/otp/verify", p.handleOTPVerify)
	router.Post("/session/verify", p.handleSessionVerify)
	router.Post("/session/logout", p.handleSessionLogout)
}

// MountPath 返回用于页面注入的 API 前缀。
func (p *Public) MountPath() string { return p.cfg.MountPath }

type checkUIDReq struct {
	Token string `json:"token"`
	UID   string `json:"uid"`
}

type acceptReq struct {
	Token string `json:"token"`
	UID   string `json:"uid"`
}

type emailReq struct {
	Email string `json:"email"`
}

type verifyReq struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type tokenReq struct {
	Token string `json:"token"`
}

// handleInvitePage 渲染邀请落地页。
//
// **只读**：不消费 token（邮件安全网关的预取不会把链接烧掉）。
// 无论 token 是否有效都返回 200 的页面，错误信息渲染在页面里。
func (p *Public) handleInvitePage(c *fiber.Ctx) error {
	setPageHeaders(c)

	token := c.Query("token")
	name := p.app.ServiceName()
	assets := p.cfg.MountPath + p.cfg.AssetsPath
	values := map[string]string{
		"{{API}}":             p.cfg.MountPath,
		"{{ASSETS}}":          assets,
		"{{SERVICE_NAME}}":    html.EscapeString(name),
		"{{SERVICE_INITIAL}}": html.EscapeString(initialOf(name)),
		"{{FAVICON}}":         faviconTag(p.cfg.Icon, assets),
		"{{APP_MARK}}":        appMarkTag(p.cfg.Icon, assets, 64, "app-mark", initialOf(name)),
	}

	if token == "" {
		fillUnavailable(values, "链接不完整，请从邮件中重新打开。")
		return c.Type("html").SendString(render(p.page, values))
	}

	view, err := p.app.GetInvite(c.UserContext(), token)
	if err != nil {
		fillUnavailable(values, msgInviteInvalid)
		return c.Type("html").SendString(render(p.page, values))
	}

	values["{{FORM_HIDDEN}}"] = ""
	values["{{ERROR_BLOCK}}"] = ""
	values["{{EMAIL}}"] = html.EscapeString(view.Email)
	values["{{EXPIRY}}"] = html.EscapeString("链接有效期至 " + view.ExpiresAt)

	// 两种情况共用同一个页面与同一套令牌机制，只是文案不同：
	//   - 尚未激活的受邀用户 → "账号激活 / 激活账号"；
	//   - 已可用但还没有用户名的管理员 → "设置用户名 / 保存用户名"。
	if view.AlreadyActive {
		values["{{HEADLINE}}"] = "设置用户名"
		values["{{SUBTITLE}}"] = "账号已可使用，补一个用户名即可"
		values["{{SUBMIT_LABEL}}"] = "保存用户名"
		values["{{DONE_TEXT}}"] = "用户名已保存。请回到管理界面，用本邮箱获取验证码登录。"
	} else {
		values["{{HEADLINE}}"] = "账号激活"
		values["{{SUBTITLE}}"] = "设置你的用户名，完成账号激活"
		values["{{SUBMIT_LABEL}}"] = "激活账号"
		values["{{DONE_TEXT}}"] = "激活成功。请回到服务首页，用本邮箱获取登录验证码。"
	}
	if view.NeedsUID {
		values["{{LABEL_HIDDEN}}"] = ""
		values["{{INPUT_HIDDEN}}"] = ""
		values["{{HINT_HIDDEN}}"] = ""
		values["{{UID_HINT}}"] = ""
	} else {
		values["{{LABEL_HIDDEN}}"] = "hidden"
		values["{{INPUT_HIDDEN}}"] = "hidden"
		values["{{HINT_HIDDEN}}"] = "hidden"
		values["{{UID_HINT}}"] = "你的用户名已由管理员预设，直接激活即可。"
	}
	return c.Type("html").SendString(render(p.page, values))
}

// fillUnavailable 为"链接不可用"的页面填上全部占位符。
//
// 每个 {{...}} 都必须有值：漏一个就会在页面上留下空白，而这类失效很难被人工发现
// （httpapi 的 TestInvitePageSubstitutesEveryPlaceholder 会守住这一点）。
func fillUnavailable(values map[string]string, message string) {
	values["{{FORM_HIDDEN}}"] = "hidden"
	// 连外层容器一起输出：正常情形下 ERROR_BLOCK 为空串，不留空框
	values["{{ERROR_BLOCK}}"] = `<div class="invite__note"><p class="status status--err">` + message + `</p></div>`
	values["{{EMAIL}}"] = ""
	values["{{EXPIRY}}"] = ""
	values["{{LABEL_HIDDEN}}"] = "hidden"
	values["{{INPUT_HIDDEN}}"] = "hidden"
	values["{{HINT_HIDDEN}}"] = "hidden"
	values["{{UID_HINT}}"] = ""
	values["{{HEADLINE}}"] = "账号激活"
	values["{{SUBTITLE}}"] = "通过邮件里的链接完成设置"
	values["{{SUBMIT_LABEL}}"] = "激活账号"
	values["{{DONE_TEXT}}"] = ""
	values["{{FAVICON}}"] = ""
	// 服务名、首字母与图标标记由调用方预先填好；这里只兜底，确保不会出现未替换的占位符
	if _, ok := values["{{SERVICE_NAME}}"]; !ok {
		values["{{SERVICE_NAME}}"] = ""
	}
	if _, ok := values["{{SERVICE_INITIAL}}"]; !ok {
		values["{{SERVICE_INITIAL}}"] = "UC"
	}
	if _, ok := values["{{APP_MARK}}"]; !ok {
		values["{{APP_MARK}}"] = ""
	}
}

// initialOf 取服务名的首字符作为界面上的标识（英文取大写，中日韩取首字）。
func initialOf(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "UC"
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' {
			return string(r - 32)
		}
		return string(r)
	}
	return "UC"
}

func (p *Public) handleCheckUID(c *fiber.Ctx) error {
	var req checkUIDReq
	if err := c.BodyParser(&req); err != nil {
		return failStatus(c, http.StatusBadRequest, "请求格式不正确")
	}
	available, err := p.app.CheckUID(c.UserContext(), req.Token, req.UID)
	if err != nil {
		return fail(c, err, p.cfg.OnError)
	}
	return ok(c, fiber.Map{"available": available})
}

func (p *Public) handleAccept(c *fiber.Ctx) error {
	var req acceptReq
	if err := c.BodyParser(&req); err != nil {
		return failStatus(c, http.StatusBadRequest, "请求格式不正确")
	}
	if _, err := p.app.AcceptInvite(c.UserContext(), req.Token, req.UID, c.IP(), c.Get(fiber.HeaderUserAgent)); err != nil {
		return fail(c, err, p.cfg.OnError)
	}
	return ok(c, fiber.Map{"activated": true})
}

// handleOTPSend 请求登录验证码。
//
// 无论邮箱是否存在、是否可登录，响应体与状态码都完全一致（防枚举）。
func (p *Public) handleOTPSend(c *fiber.Ctx) error {
	var req emailReq
	if err := c.BodyParser(&req); err != nil {
		return failStatus(c, http.StatusBadRequest, "请求格式不正确")
	}
	if _, err := p.app.RequestLoginCode(c.UserContext(), req.Email, c.IP()); err != nil {
		return fail(c, err, p.cfg.OnError)
	}
	return ok(c, fiber.Map{"sent": true, "message": msgOTPSent})
}

func (p *Public) handleOTPVerify(c *fiber.Ctx) error {
	var req verifyReq
	if err := c.BodyParser(&req); err != nil {
		return failStatus(c, http.StatusBadRequest, "请求格式不正确")
	}
	res, err := p.app.VerifyLoginCode(c.UserContext(), req.Email, req.Code, c.IP(), c.Get(fiber.HeaderUserAgent))
	if err != nil {
		return fail(c, err, p.cfg.OnError)
	}
	return ok(c, fiber.Map{"token": res.Token, "uid": res.UID})
}

func (p *Public) handleSessionVerify(c *fiber.Ctx) error {
	var req tokenReq
	if err := c.BodyParser(&req); err != nil {
		return failStatus(c, http.StatusBadRequest, "请求格式不正确")
	}
	sess, err := p.app.VerifySession(c.UserContext(), req.Token)
	if err != nil {
		return fail(c, err, p.cfg.OnError)
	}
	return ok(c, fiber.Map{"uid": sess.UID, "scope": sess.Scope})
}

func (p *Public) handleSessionLogout(c *fiber.Ctx) error {
	var req tokenReq
	if err := c.BodyParser(&req); err != nil {
		return failStatus(c, http.StatusBadRequest, "请求格式不正确")
	}
	if err := p.app.Logout(c.UserContext(), req.Token); err != nil {
		return fail(c, err, p.cfg.OnError)
	}
	return ok(c, fiber.Map{"logged_out": true})
}

func (p *Public) serveCSS(c *fiber.Ctx) error {
	c.Set(fiber.HeaderContentType, "text/css; charset=utf-8")
	c.Set(fiber.HeaderCacheControl, "public, max-age=300")
	return c.Send(p.style)
}

func (p *Public) serveIcon(c *fiber.Ctx) error {
	return serveIconBytes(c, p.cfg.Icon)
}

func (p *Public) serveInviteJS(c *fiber.Ctx) error {
	c.Set(fiber.HeaderContentType, "application/javascript; charset=utf-8")
	c.Set(fiber.HeaderCacheControl, "public, max-age=300")
	return c.Send(p.js)
}

// setPageHeaders 设置落地页的安全响应头。
func setPageHeaders(c *fiber.Ctx) {
	c.Set(fiber.HeaderContentSecurityPolicy, pageCSP)
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set(fiber.HeaderReferrerPolicy, "no-referrer")
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set("X-Robots-Tag", "noindex, nofollow")
}

func render(page string, values map[string]string) string {
	out := page
	for k, v := range values {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}
