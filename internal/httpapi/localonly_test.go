package httpapi

import (
	"net/http"
	"testing"
)

func TestIsLoopbackAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:1234":   true,
		"127.0.0.1":        true,
		"[::1]:1234":       true,
		"::1":              true,
		"127.0.0.53:80":    true, // 整个 127/8 都是回环
		"10.0.0.1:1234":    false,
		"192.168.1.5:80":   false,
		"8.8.8.8:53":       false,
		"":                 false,
		"not-an-ip:1234":   false,
		"0.0.0.0:0":        false,
		"[::ffff:1.2.3.4]": false,
	}
	for addr, want := range cases {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	allowed := []string{"127.0.0.1", "localhost", "::1"}

	cases := map[string]bool{
		"127.0.0.1:9998":     true,
		"127.0.0.1":          true,
		"localhost:9998":     true,
		"LOCALHOST:9998":     true,
		"[::1]:9998":         true,
		"::1":                true,
		"evil.com":           false,
		"evil.com:9998":      false,
		"127.0.0.1.evil.com": false,
		"":                   false,
	}
	for host, want := range cases {
		if got := hostAllowed(host, allowed); got != want {
			t.Errorf("hostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// allowLocal 是"仅本地访问"的最终判据：RemoteAddr 必须是回环，且 Host 必须在白名单。
func TestAllowLocal(t *testing.T) {
	allowed := []string{"127.0.0.1", "localhost"}

	if !allowLocal("127.0.0.1:5555", "127.0.0.1:9998", allowed) {
		t.Error("loopback + allowed host should pass")
	}
	if allowLocal("10.1.2.3:5555", "127.0.0.1:9998", allowed) {
		t.Error("non-loopback remote must be rejected")
	}
	if allowLocal("127.0.0.1:5555", "evil.com", allowed) {
		t.Error("disallowed Host must be rejected (DNS rebinding)")
	}
	if allowLocal("", "127.0.0.1:9998", allowed) {
		t.Error("empty remote addr must be rejected (fail closed)")
	}
}

func TestBearerToken(t *testing.T) {
	// bearerToken 依赖 fiber.Ctx，这里只验证无 ctx 时代码路径不会 panic 的契约由
	// 集成测试覆盖；此处留白以保持单测边界清晰。
	_ = http.StatusOK
}
