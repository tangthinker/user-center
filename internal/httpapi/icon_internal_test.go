package httpapi

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// 1x1 的合法 PNG / GIF / JPEG / WebP 头部（只取够魔数校验的长度）
var (
	pngBytes  = mustDecode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==")
	gifBytes  = []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	jpegBytes = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	webpBytes = append([]byte("RIFF\x24\x00\x00\x00WEBP"), []byte("VP8 ")...)
)

func mustDecode(b64 string) []byte {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestNewIconAcceptsSupportedTypes(t *testing.T) {
	cases := []struct {
		name     string
		raw      []byte
		declared string
		want     string
	}{
		{"png with type", pngBytes, "image/png", "image/png"},
		{"png sniffed", pngBytes, "", "image/png"},
		{"jpeg", jpegBytes, "image/jpeg", "image/jpeg"},
		{"gif", gifBytes, "image/gif", "image/gif"},
		{"webp", webpBytes, "image/webp", "image/webp"},
		{"ico", []byte{0x00, 0x00, 0x01, 0x00, 0x01, 0x00}, "image/x-icon", "image/x-icon"},
		{"type with charset", pngBytes, "image/png; charset=binary", "image/png"},
		{"declared lies, sniff wins", pngBytes, "application/octet-stream", "image/png"},
	}
	for _, c := range cases {
		icon, err := NewIcon(c.raw, c.declared)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if icon.ContentType() != c.want {
			t.Errorf("%s: ContentType = %q, want %q", c.name, icon.ContentType(), c.want)
		}
		if !bytes.Equal(icon.Bytes(), c.raw) {
			t.Errorf("%s: 字节不一致", c.name)
		}
	}
}

func TestNewIconRejectsBadInput(t *testing.T) {
	// 空 = 不配置，不是错误
	if icon, err := NewIcon(nil, ""); err != nil || icon != nil {
		t.Fatalf("空图标应返回 nil, nil，实际 %v, %v", icon, err)
	}

	// SVG：可携带脚本，明确拒绝并给出可执行的提示
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	if _, err := NewIcon(svg, "image/svg+xml"); err == nil || !strings.Contains(err.Error(), "SVG") {
		t.Errorf("SVG 应被拒绝并点明原因，实际 %v", err)
	}

	// 声明与内容不符
	if _, err := NewIcon([]byte("not an image at all"), "image/png"); err == nil {
		t.Error("内容与声明不符时应报错")
	}
	if _, err := NewIcon([]byte("plain text"), ""); err == nil {
		t.Error("无法识别的类型应报错")
	}

	// 体积上限
	big := append(bytes.Repeat([]byte{0}, MaxIconBytes), pngBytes...)
	_ = big
	oversized := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{0}, MaxIconBytes)...)
	if _, err := NewIcon(oversized, "image/png"); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Errorf("超限应报错，实际 %v", err)
	}
}

func TestAppMarkTagFallsBackToInitial(t *testing.T) {
	// 未配置图标 → 首字方块（与服务端渲染的 {{SERVICE_INITIAL}} 一致）
	tag := appMarkTag(nil, "/admin/assets", 64, "app-mark", "云")
	if !strings.Contains(tag, ">云<") || !strings.Contains(tag, `class="app-mark"`) {
		t.Errorf("无图标时应输出首字方块，实际 %s", tag)
	}
	// 首字母必须转义
	escaped := appMarkTag(nil, "/admin/assets", 64, "app-mark", `<script>`)
	if strings.Contains(escaped, "<script>") {
		t.Errorf("首字未转义：%s", escaped)
	}

	// 有图标 → img，带尺寸且不内联样式（CSP 会丢内联 style）
	icon, err := NewIcon(pngBytes, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	tag = appMarkTag(icon, "/admin/assets", 64, "app-mark", "云")
	for _, want := range []string{`<img `, `src="/admin/assets/icon"`, `width="64"`, `height="64"`, "app-mark--img"} {
		if !strings.Contains(tag, want) {
			t.Errorf("图标标记缺少 %q：%s", want, tag)
		}
	}
	if strings.Contains(tag, "style=") {
		t.Error("不得内联样式（CSP 会丢弃）")
	}
	if favicon := faviconTag(icon, "/admin/assets"); !strings.Contains(favicon, `rel="icon"`) {
		t.Errorf("favicon 缺失：%s", favicon)
	}
	if faviconTag(nil, "/admin/assets") != "" {
		t.Error("无图标时不应输出 favicon 标签")
	}
}
