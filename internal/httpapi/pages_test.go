package httpapi_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"testing"

	uc "github.com/tangthinker/user-center/v2"
	"github.com/tangthinker/user-center/v2/internal/app"
	"github.com/tangthinker/user-center/v2/internal/httpapi"
	"github.com/tangthinker/user-center/v2/internal/webui"
)

var placeholderRe = regexp.MustCompile(`\{\{[A-Z_]+\}\}`)

// 页面模板里的每个 {{...}} 都必须被处理器替换掉；漏一个就会在界面上留下空白，
// 而这种失效在人工点检时很容易被忽略。
func TestInvitePageSubstitutesEveryPlaceholder(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if _, err := e.app.AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	token := e.inviteToken(t, "alice@example.com")

	for _, tc := range []struct {
		name string
		path string
	}{
		{"有效邀请", "/api/v1/invite?token=" + token},
		{"无效邀请", "/api/v1/invite?token=bogus"},
		{"缺失 token", "/api/v1/invite"},
	} {
		res := e.do(t, http.MethodGet, tc.path, nil)
		if res.status != http.StatusOK {
			t.Fatalf("%s: 状态码 = %d", tc.name, res.status)
		}
		if leftover := placeholderRe.FindAllString(res.raw, -1); len(leftover) > 0 {
			t.Errorf("%s: 页面残留未替换占位符 %v", tc.name, leftover)
		}
	}

	// 有效邀请必须真的渲染出邮箱与有效期
	res := e.do(t, http.MethodGet, "/api/v1/invite?token="+token, nil)
	if !strings.Contains(res.raw, "alice@example.com") {
		t.Error("落地页未渲染受邀邮箱")
	}
	if !strings.Contains(res.raw, "有效期") {
		t.Error("落地页未渲染有效期")
	}
	// 落地页也要自报家门：同一个用户可能同时属于多个宿主
	if !strings.Contains(res.raw, "测试服务") {
		t.Error("落地页未渲染服务名")
	}
}

func TestAdminPageSubstitutesEveryPlaceholder(t *testing.T) {
	e := newEnv(t, nil)
	res := e.do(t, http.MethodGet, "/admin/", nil)
	if res.status != http.StatusOK {
		t.Fatalf("状态码 = %d", res.status)
	}
	if leftover := placeholderRe.FindAllString(res.raw, -1); len(leftover) > 0 {
		t.Errorf("管理页残留未替换占位符 %v", leftover)
	}
	if !strings.Contains(res.raw, `data-api="/admin"`) {
		t.Error("管理页未注入 API 前缀")
	}
	if !strings.Contains(res.raw, `src="/admin/assets/app.js"`) {
		t.Error("管理页未注入资源前缀")
	}
}

// 页面与脚本必须是"外链式"的：CSP 为 default-src 'none'; script-src 'self'; style-src 'self'，
// 内联样式与内联脚本会被浏览器直接丢弃（等于静默失效）。
func TestEmbeddedPagesAvoidInlineStyleAndScript(t *testing.T) {
	pages := map[string][]byte{}
	for name, load := range map[string]func() ([]byte, error){
		"admin/index.html": webui.AdminIndex,
		"invite.html":      webui.InvitePage,
	} {
		raw, err := load()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		pages[name] = raw
	}

	// RE2 不支持负向前瞻，这里先取出所有 <script ...> 开标签再逐个判断有没有 src
	scriptTag := regexp.MustCompile(`(?s)<script[^>]*>`)
	onHandler := regexp.MustCompile(`\son[a-z]+\s*=`)
	styleAttr := regexp.MustCompile(`\sstyle\s*=`)

	for name, raw := range pages {
		s := string(raw)
		if styleAttr.MatchString(s) {
			t.Errorf("%s: 含内联 style 属性（CSP 会丢弃，导致布局静默失效）", name)
		}
		for _, tag := range scriptTag.FindAllString(s, -1) {
			if !strings.Contains(tag, "src=") {
				t.Errorf("%s: 含无 src 的 <script>（内联脚本会被 CSP 阻止）：%s", name, tag)
			}
		}
		if onHandler.MatchString(s) {
			t.Errorf("%s: 含 on* 事件属性（会被 CSP 阻止）", name)
		}
	}
}

// 脚本一律用 textContent 渲染服务端数据，避免把接口内容拼进 innerHTML。
func TestAdminAndInviteScriptsAvoidInnerHTML(t *testing.T) {
	for name, load := range map[string]func() ([]byte, error){
		"admin/app.js": webui.AdminJS,
		"invite.js":    webui.InviteJS,
	} {
		raw, err := load()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// 只拦"实际赋值"，避免把注释里提到 innerHTML 的说明也算违规
		assignment := regexp.MustCompile(`\.(inner|outer)HTML\s*=|document\.write\s*\(`)
		if m := assignment.FindString(string(raw)); m != "" {
			t.Errorf("%s: 使用了 %s；渲染接口数据必须用 textContent", name, strings.TrimSpace(m))
		}
	}
}

// 样式表里引用的外部资源必须是零（无 CDN、无外部字体）。
func TestStylesheetHasNoExternalResources(t *testing.T) {
	css, err := webui.StyleCSS()
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	for _, bad := range []string{"@import", "http://", "https://", "url("} {
		if strings.Contains(s, bad) {
			t.Errorf("样式表含外部引用 %q（CSP 与离线可用性都会被破坏）", bad)
		}
	}
	// 必须同时给出明暗两套外观
	if !strings.Contains(s, "prefers-color-scheme: dark") {
		t.Error("样式表缺少深色外观")
	}
	if !strings.Contains(s, "prefers-reduced-motion") {
		t.Error("样式表未尊重「减弱动态效果」")
	}
	if !strings.Contains(s, "[hidden]") {
		t.Error("样式表未让 hidden 属性在作者样式前生效（会导致元素假隐藏）")
	}
}

// 服务名来自宿主配置，注入页面时必须转义（不能把配置当 HTML 执行）。
func TestServiceNameIsHTMLEscapedInPages(t *testing.T) {
	const nasty = `A & B <script>alert(1)</script>`
	e := newEnvWithServiceName(t, nasty)

	admin := e.do(t, http.MethodGet, "/admin/", nil)
	if strings.Contains(admin.raw, "<script>alert(1)</script>") {
		t.Fatal("管理页把服务名当 HTML 输出了")
	}
	if !strings.Contains(admin.raw, "A &amp; B &lt;script&gt;") {
		t.Fatal("管理页未渲染转义后的服务名")
	}

	ctx := context.Background()
	if _, err := e.app.AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	page := e.do(t, http.MethodGet, "/api/v1/invite?token="+e.inviteToken(t, "alice@example.com"), nil)
	if strings.Contains(page.raw, "<script>alert(1)</script>") {
		t.Fatal("落地页把服务名当 HTML 输出了")
	}
}

// 样式表的质量底线（对照 apple-design 技能的可访问性镜头与"再次自评"清单）。
func TestStylesheetMeetsQualityFloor(t *testing.T) {
	raw, err := webui.StyleCSS()
	if err != nil {
		t.Fatal(err)
	}
	css := string(raw)

	// 1) 文字必须随用户的"默认字号"缩放：px 不随该设置变化，rem 才会。
	fontInPx := regexp.MustCompile(`font(-size)?\s*:[^;{}]*?\d+px`)
	if m := fontInPx.FindString(css); m != "" {
		t.Errorf("字号不要用 px（不随浏览器默认字号缩放）：%s", strings.TrimSpace(m))
	}
	for _, tok := range []string{"--fs-base:", "--fs-small:", "--fs-tiny:", "--fs-title:"} {
		idx := strings.Index(css, tok)
		if idx < 0 {
			t.Errorf("缺少字号 token %s", tok)
			continue
		}
		value := strings.TrimSpace(css[idx+len(tok) : idx+len(tok)+12])
		if !strings.Contains(value, "rem") {
			t.Errorf("%s 应为 rem 值，实际 %q", tok, value)
		}
	}
	// 控件高度也要跟着字号长，否则放大字号会把文字裁掉
	for _, tok := range []string{"--h-control:", "--h-tap:"} {
		idx := strings.Index(css, tok)
		if idx < 0 || !strings.Contains(strings.TrimSpace(css[idx+len(tok):idx+len(tok)+12]), "rem") {
			t.Errorf("%s 应为 rem 值", tok)
		}
	}

	// 2) 键盘焦点必须可见（输入框另有一圈柔光）
	if !strings.Contains(css, ":focus-visible") {
		t.Error("缺少 :focus-visible")
	}
	if !strings.Contains(css, "box-shadow: 0 0 0 3px var(--accent-soft)") {
		t.Error("输入框缺少聚焦柔光")
	}

	// 3) 尊重"减弱动态效果"与"增强对比度"
	for _, want := range []string{"prefers-reduced-motion", "prefers-contrast"} {
		if !strings.Contains(css, want) {
			t.Errorf("缺少 %s", want)
		}
	}

	// 4) 明暗外观跟随系统，且不提供应用内开关
	if !strings.Contains(css, "prefers-color-scheme: dark") {
		t.Error("缺少深色外观")
	}

	// 5) 窄屏可用
	if !strings.Contains(css, "@media (max-width: 640px)") {
		t.Error("缺少窄屏重排")
	}

	// 6) 层级只留给真正"浮起来"的东西：内容分组不应带阴影
	groupRule := regexp.MustCompile(`\.group \{[^}]*\}`)
	if m := groupRule.FindString(css); strings.Contains(m, "box-shadow") {
		t.Errorf("内容分组不应带阴影（抬高只留给一次性链接面板/对话框/提示条）：%s", m)
	}

	// 7) 与参考实现一致的材质与语义化灰阶（本任务的风格要求）
	for _, want := range []string{"--blur-material", "backdrop-filter", "--separator", "--label-2", "#0071e3"} {
		if !strings.Contains(css, want) {
			t.Errorf("缺少参考设计语言的关键 token/属性：%s", want)
		}
	}
}

// pngIcon 是一个合法的 1x1 PNG（仅用于验证"配置了图标"这条路径）。
func pngIcon(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newEnvWithIcon(t *testing.T) *env {
	t.Helper()
	icon, err := httpapi.NewIcon(pngIcon(t), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	return newEnvFull(t, nil, func(pub *httpapi.PublicConfig, adm *httpapi.AdminConfig) {
		pub.Icon = icon
		adm.Icon = icon
	})
}

// 配置了图标：两个页面都要引用它，且图标由**同源**路由提供（外链会被 CSP 拦掉）。
func TestConfiguredIconIsUsedAndServedSameOrigin(t *testing.T) {
	e := newEnvWithIcon(t)

	admin := e.do(t, http.MethodGet, "/admin/", nil)
	if admin.status != http.StatusOK {
		t.Fatalf("管理页 = %d", admin.status)
	}
	for _, want := range []string{
		`<link rel="icon" href="/admin/assets/icon">`,
		`src="/admin/assets/icon"`,
		`class="app-mark app-mark--img"`,
		`class="toolbar__mark toolbar__mark--img"`,
	} {
		if !strings.Contains(admin.raw, want) {
			t.Errorf("管理页缺少 %q", want)
		}
	}
	if strings.Contains(admin.raw, "{{") {
		t.Errorf("管理页残留占位符")
	}

	icon := e.do(t, http.MethodGet, "/admin/assets/icon", nil)
	if icon.status != http.StatusOK {
		t.Fatalf("图标路由 = %d", icon.status)
	}
	if ct := icon.header.Get("Content-Type"); !strings.Contains(ct, "image/png") {
		t.Errorf("Content-Type = %q", ct)
	}
	if icon.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("图标必须声明 nosniff")
	}
	if icon.raw == "" {
		t.Error("图标响应体为空")
	}

	// CSP 必须放开同源图片（但仍不允许 data: 或外部源）
	csp := admin.header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self'") {
		t.Errorf("CSP 未允许同源图片：%s", csp)
	}
	if strings.Contains(csp, "img-src *") || strings.Contains(csp, "data:") {
		t.Errorf("CSP 放得过宽：%s", csp)
	}

	// 落地页同样使用该图标，且由公开面的资源路径提供
	ctx := context.Background()
	if _, err := e.app.AdminCreateUser(ctx, app.Actor{ID: 1, Email: "ops@example.com"}, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	page := e.do(t, http.MethodGet, "/api/v1/invite?token="+e.inviteToken(t, "alice@example.com"), nil)
	for _, want := range []string{
		`<link rel="icon" href="/api/v1/invite-assets/icon">`,
		`src="/api/v1/invite-assets/icon"`,
	} {
		if !strings.Contains(page.raw, want) {
			t.Errorf("落地页缺少 %q", want)
		}
	}
	if got := e.do(t, http.MethodGet, "/api/v1/invite-assets/icon", nil); got.status != http.StatusOK {
		t.Errorf("落地页图标路由 = %d", got.status)
	}
}

// 未配置图标：回退到服务名首字，图标路由返回 404（页面本来也不引用它）。
func TestIconFallsBackToServiceInitial(t *testing.T) {
	e := newEnv(t, nil)

	admin := e.do(t, http.MethodGet, "/admin/", nil)
	if !strings.Contains(admin.raw, `class="app-mark" aria-hidden="true">测<`) {
		t.Errorf("未配置图标时应显示服务名首字：%s", admin.raw[:200])
	}
	if strings.Contains(admin.raw, `rel="icon"`) {
		t.Error("未配置图标时不应输出 favicon 标签")
	}
	if strings.Contains(admin.raw, "{{") {
		t.Error("管理页残留占位符")
	}
	if got := e.do(t, http.MethodGet, "/admin/assets/icon", nil); got.status != http.StatusNotFound {
		t.Errorf("未配置图标时 /assets/icon = %d, want 404", got.status)
	}
}

// 非法图标必须在启动时就失败，而不是等到浏览器请求它。
func TestInvalidAppIconFailsAtStartup(t *testing.T) {
	cfg := uc.Config{
		DBPath: t.TempDir(), ServiceName: "x", PublicBaseURL: "https://x.example.com",
		HMACKey: []byte("0123456789abcdef0123456789abcdef"),
		AppIcon: []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>"),
	}
	if _, err := uc.New(cfg); err == nil || !strings.Contains(err.Error(), "SVG") {
		t.Fatalf("SVG 图标应在启动时报错，实际 %v", err)
	}

	cfg.AppIcon = []byte("not an image")
	if _, err := uc.New(cfg); err == nil {
		t.Fatal("非图片字节应在启动时报错")
	}
}
