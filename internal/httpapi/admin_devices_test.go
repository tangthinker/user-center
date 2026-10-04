package httpapi_test

import (
	"net/http"
	"testing"
	"time"
)

// 真实世界的两个 UA：管理界面要显示"设备类型 / 型号"，服务端必须给出可读的结论。
const (
	uaIPhoneSafari = `Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1`
	uaMacSafari    = `Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15`
)

func withUserAgent(ua string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("User-Agent", ua) }
}

// 管理界面的核心诉求：一个用户有几台在线设备、分别是什么。
//
// 这里走完整链路（登录 → 列表 → 展开设备），因为最容易出错的地方不是解析，
// 而是"验证码登录每次都签发新会话"：同一台手机登录两次必须在界面上仍然是一台。
func TestAdminListsOnlineDevices(t *testing.T) {
	e := newEnv(t, nil)
	token := e.adminToken(t)
	e.seedActiveUser(t, "alice@example.com", "alice")

	login := func(ua string) {
		t.Helper()
		if res := e.do(t, http.MethodPost, "/api/v1/otp/send",
			map[string]string{"email": "alice@example.com"}); res.status != http.StatusOK {
			t.Fatalf("otp send = %d %s", res.status, res.raw)
		}
		code := e.otpCode(t, "alice@example.com")
		res := e.do(t, http.MethodPost, "/api/v1/otp/verify",
			map[string]string{"email": "alice@example.com", "code": code}, withUserAgent(ua))
		if res.status != http.StatusOK {
			t.Fatalf("otp verify = %d %s", res.status, res.raw)
		}
	}
	login(uaIPhoneSafari)
	e.clock.Advance(time.Minute) // 让两次登入时间可区分
	login(uaIPhoneSafari)        // 同一台手机再登录一次
	e.clock.Advance(time.Minute)
	login(uaMacSafari)

	users := e.do(t, http.MethodGet, "/admin/users", nil, withToken(token))
	if users.status != http.StatusOK {
		t.Fatalf("users = %d %s", users.status, users.raw)
	}
	alice := findUser(t, users, "alice@example.com")
	if got := alice["online_devices"].(float64); got != 2 {
		t.Errorf("alice 的在线设备 = %v, want 2（手机 1 台 + 电脑 1 台，两次手机登录算一台）", got)
	}
	// 管理员自己：Go 的 http 客户端 UA 是 Go-http-client/1.1，必须被认成脚本客户端
	// 而不是"未知设备"——否则界面上会出现一个说不清是什么的东西。
	ops := findUser(t, users, "ops@example.com")
	if got := ops["online_devices"].(float64); got != 1 {
		t.Errorf("管理员的在线设备 = %v, want 1", got)
	}

	devices := e.do(t, http.MethodGet,
		"/admin/users/"+itoaID(alice)+"/sessions", nil, withToken(token))
	if devices.status != http.StatusOK {
		t.Fatalf("devices = %d %s", devices.status, devices.raw)
	}
	list, _ := devices.data()["devices"].([]any)
	if len(list) != 2 {
		t.Fatalf("设备数 = %d, want 2；payload=%s", len(list), devices.raw)
	}

	byModel := map[string]map[string]any{}
	for _, item := range list {
		d, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("设备项不是对象：%T", item)
		}
		byModel[d["model"].(string)] = d
	}

	phone, ok := byModel["iPhone"]
	if !ok {
		t.Fatalf("没有 iPhone 设备：%s", devices.raw)
	}
	if phone["type"] != "mobile" || phone["os"] != "iOS 17.5" || phone["browser"] != "Safari 17.5" {
		t.Errorf("iPhone 设备字段不对：%v", phone)
	}
	if phone["sessions"].(float64) != 2 {
		t.Errorf("iPhone 的会话数 = %v, want 2", phone["sessions"])
	}
	if phone["scope"] != "user" {
		t.Errorf("scope = %v, want user", phone["scope"])
	}
	if phone["ip"] == "" {
		t.Error("设备缺少来源 IP")
	}

	// 界面上要显示"登入时间"，因此每个设备都必须带上可解析的时间戳
	loginAt, err := time.Parse(time.RFC3339, phone["login_at"].(string))
	if err != nil {
		t.Fatalf("login_at 不是 RFC3339：%v", phone["login_at"])
	}
	firstAt, err := time.Parse(time.RFC3339, phone["first_login_at"].(string))
	if err != nil {
		t.Fatalf("first_login_at 不是 RFC3339：%v", phone["first_login_at"])
	}
	if !firstAt.Before(loginAt) {
		t.Errorf("同一台手机的两次登录必须体现在时间上：first=%v last=%v", firstAt, loginAt)
	}
	if _, err := time.Parse(time.RFC3339, phone["last_seen"].(string)); err != nil {
		t.Errorf("last_seen 不是 RFC3339：%v", phone["last_seen"])
	}

	mac, ok := byModel["Mac"]
	if !ok {
		t.Fatalf("没有 Mac 设备：%s", devices.raw)
	}
	if mac["type"] != "desktop" || mac["sessions"].(float64) != 1 {
		t.Errorf("Mac 设备字段不对：%v", mac)
	}
}

// 踢下线之后，设备列表必须为空——界面上的徽标与展开内容都不能留下幽灵设备。
func TestAdminDevicesEmptyAfterRevoke(t *testing.T) {
	e := newEnv(t, nil)
	token := e.adminToken(t)
	e.seedActiveUser(t, "alice@example.com", "alice")

	if res := e.do(t, http.MethodPost, "/api/v1/otp/send",
		map[string]string{"email": "alice@example.com"}); res.status != http.StatusOK {
		t.Fatalf("otp send = %d", res.status)
	}
	code := e.otpCode(t, "alice@example.com")
	if res := e.do(t, http.MethodPost, "/api/v1/otp/verify",
		map[string]string{"email": "alice@example.com", "code": code},
		withUserAgent(uaIPhoneSafari)); res.status != http.StatusOK {
		t.Fatalf("otp verify = %d %s", res.status, res.raw)
	}

	users := e.do(t, http.MethodGet, "/admin/users", nil, withToken(token))
	alice := findUser(t, users, "alice@example.com")
	if got := alice["online_devices"].(float64); got != 1 {
		t.Fatalf("登录后的在线设备 = %v, want 1", got)
	}

	revoke := e.do(t, http.MethodPost,
		"/admin/users/"+itoaID(alice)+"/sessions/revoke", nil, withToken(token))
	if revoke.status != http.StatusOK {
		t.Fatalf("revoke = %d %s", revoke.status, revoke.raw)
	}

	devices := e.do(t, http.MethodGet,
		"/admin/users/"+itoaID(alice)+"/sessions", nil, withToken(token))
	list, _ := devices.data()["devices"].([]any)
	if len(list) != 0 {
		t.Fatalf("踢下线后仍有 %d 台设备：%s", len(list), devices.raw)
	}

	users = e.do(t, http.MethodGet, "/admin/users", nil, withToken(token))
	if got := findUser(t, users, "alice@example.com")["online_devices"].(float64); got != 0 {
		t.Errorf("踢下线后的在线设备 = %v, want 0", got)
	}
}

func findUser(t *testing.T, res response, email string) map[string]any {
	t.Helper()
	list, _ := res.data()["users"].([]any)
	for _, item := range list {
		if u, ok := item.(map[string]any); ok && u["email"] == email {
			return u
		}
	}
	t.Fatalf("用户列表里没有 %s：%s", email, res.raw)
	return nil
}

func itoaID(user map[string]any) string {
	return itoa64(int64(user["id"].(float64)))
}
