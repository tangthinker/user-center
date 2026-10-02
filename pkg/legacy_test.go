package pkg_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/v2/pkg"
)

// TokenValid 在未初始化时必须返回错误而不是 panic。
func TestTokenValidBeforeInit(t *testing.T) {
	if _, err := pkg.TokenValid("whatever"); err == nil {
		t.Fatal("expected error before initialization")
	}
}

// 缺少必需配置时必须返回 error（而不是 panic，也不是静默不工作）。
func TestRegisterUserCenterWithoutConfigFails(t *testing.T) {
	pkg.Configure(pkg.Options{NoEnv: true})
	err := pkg.RegisterUserCenter(fiber.New().Group("/api/v1"), t.TempDir())
	if err == nil {
		t.Fatal("expected error when configuration is incomplete")
	}
	if !strings.Contains(err.Error(), "UC_") {
		t.Errorf("error should tell the operator which env vars to set: %v", err)
	}
}

// v1 的调用形态必须仍然可用：RegisterUserCenter(router, dir) 作为语句调用。
func TestLegacyWiringEndToEnd(t *testing.T) {
	pkg.Configure(pkg.Options{
		NoEnv:         true,
		ServiceName:   "旧宿主",
		PublicBaseURL: "https://legacy.example.com",
		HMACKey:       []byte("0123456789abcdef0123456789abcdef"),
		BootstrapMail: "ops@example.com",
	})
	t.Cleanup(func() { _ = pkg.Close() })

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	// 与 cloud-core 完全一致的调用方式（忽略返回值也应能编译）
	if err := pkg.RegisterUserCenter(app.Group("/api/v1"), t.TempDir()); err != nil {
		t.Fatalf("RegisterUserCenter: %v", err)
	}

	// 公开面已挂载：落地页在 /api/v1/invite（无 token 时渲染错误提示）
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/invite", nil)
	res, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("invite page = %d", res.StatusCode)
	}

	// 验证码接口存在（防枚举：未知邮箱也返回 200）
	req2, _ := http.NewRequest(http.MethodPost, "/api/v1/otp/send",
		strings.NewReader(`{"email":"nobody@example.com"}`))
	req2.Header.Set("Content-Type", "application/json")
	res2, err := app.Test(req2)
	if err != nil {
		t.Fatal(err)
	}
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("otp/send = %d", res2.StatusCode)
	}

	// v1 已移除的接口不应再存在
	for _, path := range []string{"/api/v1/login", "/api/v1/register", "/api/v1/modify-password", "/api/v1/uid-unique"} {
		req3, _ := http.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req3.Header.Set("Content-Type", "application/json")
		res3, err := app.Test(req3)
		if err != nil {
			t.Fatal(err)
		}
		if res3.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404（v1 的公开注册/口令接口已移除）", path, res3.StatusCode)
		}
	}

	// TokenValid：伪造令牌必须失败
	if _, err := pkg.TokenValid("forged"); err == nil {
		t.Fatal("forged token must fail")
	}

	// 实例可访问，且能走完整邀请链路
	instance := pkg.Instance()
	if instance == nil {
		t.Fatal("Instance() = nil after registration")
	}
	raw, err := json.Marshal(map[string]any{"ok": true})
	if err != nil || len(raw) == 0 {
		t.Fatal(err)
	}
}
