package main

import (
	"strings"
	"testing"
)

func TestEmailSettingsValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     emailSettings
		wantErr string // 空表示应当通过
	}{
		{
			name: "未配置（走打印模式）",
			cfg:  emailSettings{},
		},
		{
			name: "合法配置",
			cfg: emailSettings{
				Server: "smtp.qiye.aliyun.com", Port: 465,
				Username: "cloud@example.com", Password: "auth-code",
			},
		},
		{
			name: "用户名只写了账号名（阿里 526 的头号原因）",
			cfg: emailSettings{
				Server: "smtp.qiye.aliyun.com", Port: 465,
				Username: "tangthinker-cloud", Password: "auth-code",
			},
			wantErr: "完整发信邮箱地址",
		},
		{
			name: "端口不是 SMTP 端口",
			cfg: emailSettings{
				Server: "smtp.qiye.aliyun.com", Port: 51574,
				Username: "cloud@example.com", Password: "auth-code",
			},
			wantErr: "465",
		},
		{
			name: "口令为空",
			cfg: emailSettings{
				Server: "smtp.qiye.aliyun.com", Port: 465,
				Username: "cloud@example.com",
			},
			wantErr: "Password 不能为空",
		},
		{
			name: "From 只写了账号名（阿里会拒收）",
			cfg: emailSettings{
				Server: "smtp.qiye.aliyun.com", Port: 465,
				Username: "cloud@example.com", Password: "x", From: "cloud",
			},
			wantErr: "留空**即自动使用 Username（cloud@example.com）",
		},
		{
			name: "From 与 Username 不同域（会被判 501 non-local account）",
			cfg: emailSettings{
				Server: "smtp.qiye.aliyun.com", Port: 465,
				Username: "cloud@example.com", Password: "x", From: "me@other.com",
			},
			wantErr: "同域别名",
		},
		{
			name: "From 与 Username 同域但不同人（允许，属于别名）",
			cfg: emailSettings{
				Server: "smtp.qiye.aliyun.com", Port: 465,
				Username: "cloud@example.com", Password: "x", From: "noreply@example.com",
			},
		},
	}

	for _, c := range cases {
		err := c.cfg.validate()
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: 期望通过，实际 %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 期望报错（含 %q），实际通过", c.name, c.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: 错误信息 %q 应包含 %q", c.name, err.Error(), c.wantErr)
		}
	}
}

func TestEmailSettingsMailConfig(t *testing.T) {
	cfg := emailSettings{
		Server: " smtp.qiye.aliyun.com ", Port: 465,
		Username: " cloud@example.com ", Password: " auth-code ",
	}
	got := cfg.mailConfig("演示服务")

	if got.Host != "smtp.qiye.aliyun.com" {
		t.Errorf("Host = %q，应去掉两端空白", got.Host)
	}
	if got.Username != "cloud@example.com" {
		t.Errorf("Username = %q，应去掉两端空白", got.Username)
	}
	if got.From != "cloud@example.com" {
		t.Errorf("From = %q，留空时应回退为 Username", got.From)
	}
	if got.Password != " auth-code " {
		t.Errorf("Password = %q，口令必须原样保留（空白可能是有效字符）", got.Password)
	}
	if !got.ImplicitTLS {
		t.Error("465 必须使用隐式 TLS")
	}
	if got.FromName != "演示服务" {
		t.Errorf("FromName = %q", got.FromName)
	}

	// 端口为 0 时回退到 465
	fallback := emailSettings{Server: "smtp.qiye.aliyun.com", Username: "a@example.com"}.mailConfig("x")
	if fallback.Port != 465 {
		t.Errorf("Port = %d，期望回退为 465", fallback.Port)
	}

	// describe 绝不能包含口令
	desc := emailSettings{
		Server: "smtp.qiye.aliyun.com", Port: 465,
		Username: "cloud@example.com", Password: "super-secret-code",
	}.describe()
	if strings.Contains(desc, "super-secret-code") {
		t.Fatalf("describe() 泄漏了口令：%s", desc)
	}
	if !strings.Contains(desc, "smtp.qiye.aliyun.com:465") || !strings.Contains(desc, "cloud@example.com") {
		t.Errorf("describe() = %q，应包含服务器与发件人", desc)
	}
}

func TestDomainOf(t *testing.T) {
	cases := map[string]string{
		"cloud@example.com": "example.com",
		"Cloud@Example.COM": "example.com",
		"no-at-sign":        "",
		"a@b@example.com":   "example.com",
		"  x@example.com  ": "example.com",
	}
	for in, want := range cases {
		if got := domainOf(in); got != want {
			t.Errorf("domainOf(%q) = %q, want %q", in, got, want)
		}
	}
}
