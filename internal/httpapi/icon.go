package httpapi

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// MaxIconBytes 是应用图标允许的最大体积。图标很小，超出多半是配置错了文件。
const MaxIconBytes = 256 * 1024

// Icon 是宿主提供的应用图标（用于浏览器标签页与界面标识）。
//
// 为什么用"字节"而不是"URL"：本库的宿主可能把静态资源挂在**另一个**监听上
// （管理面在 127.0.0.1:9998、公开面在 127.0.0.1:9999），不同端口即不同源，
// 外链图标会被本页的 CSP 直接拦掉。由库在**同源**路径下提供字节，最稳。
type Icon struct {
	raw         []byte
	contentType string
}

// 允许的图标类型。
//
// 刻意**不支持 SVG**：SVG 可以携带脚本，而图标是会被直接导航打开的 URL；
// 用 PNG/WebP 这类位图，风险面小得多。需要矢量效果就自行导出位图。
var allowedIconTypes = map[string]string{
	"image/png":                "image/png",
	"image/jpeg":               "image/jpeg",
	"image/webp":               "image/webp",
	"image/gif":                "image/gif",
	"image/x-icon":             "image/x-icon",
	"image/vnd.microsoft.icon": "image/x-icon",
	"image/ico":                "image/x-icon",
}

// NewIcon 校验并构造图标。raw 为空表示不配置（界面回退到服务名首字）。
func NewIcon(raw []byte, declaredType string) (*Icon, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > MaxIconBytes {
		return nil, fmt.Errorf("httpapi: 图标 %d 字节，超过上限 %d 字节", len(raw), MaxIconBytes)
	}

	contentType := normalizeIconType(declaredType)
	if contentType == "" {
		// 未声明或声明不合法时按内容嗅探
		contentType = normalizeIconType(http.DetectContentType(raw))
	}
	if contentType == "" {
		// 无论宿主是否声明类型，只要看起来是 SVG 就明确说明原因——
		// 否则运维只会看到一句"无法识别的类型"，不知道下一步该做什么。
		if looksLikeSVG(raw) || strings.Contains(strings.ToLower(declaredType), "svg") {
			return nil, errors.New("httpapi: 不支持 SVG 图标（SVG 可携带脚本，而图标是会被直接打开的地址）；请改用 PNG/WebP")
		}
		return nil, fmt.Errorf("httpapi: 无法识别的图标类型 %q（支持 PNG/JPEG/WebP/GIF/ICO）", declaredType)
	}
	if !hasIconMagic(raw, contentType) {
		return nil, fmt.Errorf("httpapi: 图标内容与类型 %s 不符", contentType)
	}
	return &Icon{raw: raw, contentType: contentType}, nil
}

// looksLikeSVG 通过内容判断是否为 SVG（宿主常常不会声明 content type）。
func looksLikeSVG(raw []byte) bool {
	head := raw
	if len(head) > 512 {
		head = head[:512]
	}
	text := strings.ToLower(strings.TrimSpace(string(head)))
	return strings.Contains(text, "<svg") || strings.HasPrefix(text, "<?xml")
}

// normalizeIconType 把声明值或嗅探结果归一到允许的类型；不在允许表内返回空串。
func normalizeIconType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if i := strings.IndexByte(t, ';'); i >= 0 { // 去掉 charset 等参数
		t = strings.TrimSpace(t[:i])
	}
	if canonical, ok := allowedIconTypes[t]; ok {
		return canonical
	}
	return ""
}

// hasIconMagic 用魔数复核，避免"声明成 PNG 实际是别的东西"。
func hasIconMagic(raw []byte, contentType string) bool {
	switch contentType {
	case "image/png":
		return len(raw) >= 8 && string(raw[:8]) == "\x89PNG\r\n\x1a\n"
	case "image/jpeg":
		return len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF
	case "image/gif":
		return len(raw) >= 6 && (string(raw[:6]) == "GIF87a" || string(raw[:6]) == "GIF89a")
	case "image/webp":
		return len(raw) >= 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP"
	case "image/x-icon":
		return len(raw) >= 4 && raw[0] == 0x00 && raw[1] == 0x00 && raw[2] == 0x01 && raw[3] == 0x00
	}
	return false
}

// Enabled 表示宿主确实提供了图标。
func (i *Icon) Enabled() bool { return i != nil && len(i.raw) > 0 }

// ContentType 返回可直接写入响应头的类型。
func (i *Icon) ContentType() string {
	if !i.Enabled() {
		return ""
	}
	return i.contentType
}

// Bytes 返回图标原始字节（调用方不应修改）。
func (i *Icon) Bytes() []byte {
	if !i.Enabled() {
		return nil
	}
	return i.raw
}

// pageCSP 是管理面与邀请落地页共用的内容安全策略。
//
// img-src 'self'：图标由本页同源提供（见 Icon 的说明），因此不需要放宽到 data: 或外部源。
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// serveIconBytes 输出图标；未配置时返回 404（页面本来也不会引用它）。
func serveIconBytes(c *fiber.Ctx, icon *Icon) error {
	if !icon.Enabled() {
		return failStatus(c, http.StatusNotFound, msgNotFound)
	}
	c.Set(fiber.HeaderContentType, icon.ContentType())
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff") // 图标必须是浏览器认定的图片类型
	c.Set(fiber.HeaderCacheControl, "public, max-age=3600")
	return c.Send(icon.Bytes())
}

// faviconTag 生成 <link rel="icon">；未配置图标时返回空串。
func faviconTag(icon *Icon, assets string) string {
	if !icon.Enabled() {
		return ""
	}
	return `<link rel="icon" href="` + assets + `/icon">`
}

// appMarkTag 生成界面标识：有图标就用图片，没有就用服务名首字的方块。
//
// 两种形态共用同一个 class，因此尺寸、圆角、对齐都来自同一处样式；
// width/height 一并写出，避免图片加载完成时布局跳动。
func appMarkTag(icon *Icon, assets string, size int, class string, initial string) string {
	if icon.Enabled() {
		return `<img class="` + class + ` ` + class + `--img" src="` + assets + `/icon" alt="" width="` +
			itoa(size) + `" height="` + itoa(size) + `">`
	}
	return `<span class="` + class + `" aria-hidden="true">` + html.EscapeString(initial) + `</span>`
}
