package mail

import (
	"errors"
	"net"
	"net/textproto"
	"strings"
	"testing"
)

// 5xx 是永久性错误（认证失败、用户不存在）：必须标记为不可重试，
// 否则会白等 1 分钟~7 小时并让队列堆积。
func TestPermanentIf5xx(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"网易 526 认证失败", &textproto.Error{Code: 526, Msg: "Authentication failure[0]"}, true},
		{"通用 535 认证失败", &textproto.Error{Code: 535, Msg: "Error: authentication failed"}, true},
		{"550 收件人不存在", &textproto.Error{Code: 550, Msg: "No such user"}, true},
		{"552 超配额", &textproto.Error{Code: 552, Msg: "Exceeded storage"}, true},
		{"450 灰名单（临时）", &textproto.Error{Code: 450, Msg: "Try again later"}, false},
		{"421 服务不可用（临时）", &textproto.Error{Code: 421, Msg: "Service not available"}, false},
		{"451 临时本地错误", &textproto.Error{Code: 451, Msg: "Temporary failure"}, false},
		{"网络错误（临时）", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, false},
		{"普通错误", errors.New("boom"), false},
		{"nil", nil, false},
	}

	for _, c := range cases {
		got := permanentIf5xx(c.err)
		if c.err == nil {
			if got != nil {
				t.Errorf("%s: want nil", c.name)
			}
			continue
		}
		if isPermanent := errors.Is(got, ErrPermanent); isPermanent != c.permanent {
			t.Errorf("%s: ErrPermanent = %v, want %v (err=%v)", c.name, isPermanent, c.permanent, got)
		}
	}

	// 包装后仍然可判定（fmt.Errorf("%w") 链）
	wrapped := permanentIf5xx(&textproto.Error{Code: 535, Msg: "auth"})
	if !errors.Is(wrapped, ErrPermanent) {
		t.Error("wrapped 5xx must stay detectable")
	}
}

// 认证失败的提示必须点明具体服务商该做什么，而不是一句泛泛的"认证失败"。
func TestAuthHintIsProviderAware(t *testing.T) {
	cases := map[string][]string{
		// 阿里官方 526 的四个原因都要能提示到
		"smtp.qiye.aliyun.com": {"完整发信邮箱地址", "登录密码", "三方客户端安全密码", "访问策略", "smtp.qiye.aliyun.com"},
		"smtp.mxhichina.com":   {"三方客户端安全密码"},
		"smtp.163.com":         {"客户端授权码", "526"},
		"smtp.126.com":         {"客户端授权码"},
		"smtp.yeah.net":        {"客户端授权码", "526"},
		"smtp.qq.com":          {"授权码"},
		"smtp.gmail.com":       {"应用专用密码"},
		"smtp.unknown.tld":     {"登录密码"},
	}
	for host, wants := range cases {
		hint := authHint(host)
		for _, want := range wants {
			if !strings.Contains(hint, want) {
				t.Errorf("authHint(%q) = %q，应包含 %q", host, hint, want)
			}
		}
	}
	// 提示里不应出现口令本身（本函数只接收 host，这里做一次契约性断言）
	if strings.Contains(authHint("smtp.163.com"), "password") {
		t.Error("hint must never contain credential-looking text")
	}
}
