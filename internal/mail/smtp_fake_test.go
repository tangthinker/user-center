package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startFakeSMTP 起一个极简 SMTP 服务器：EHLO 广告 AUTH，AUTH 一律返回给定应答。
// 用于验证"服务端拒绝认证"时的错误分类与文案，无需真实网络。
func startFakeSMTP(t *testing.T, authReply string) (addr string, stop func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				_, _ = c.Write([]byte("220 fake ESMTP\r\n"))
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
						_, _ = c.Write([]byte("250-fake\r\n250 AUTH PLAIN LOGIN\r\n"))
					case strings.HasPrefix(cmd, "AUTH"):
						_, _ = c.Write([]byte(authReply + "\r\n"))
					case strings.HasPrefix(cmd, "QUIT"):
						_, _ = c.Write([]byte("221 Bye\r\n"))
						return
					default:
						_, _ = c.Write([]byte("250 OK\r\n"))
					}
				}
			}(conn)
		}
	}()

	return ln.Addr().String(), func() { _ = ln.Close() }
}

func newMailerForAddr(t *testing.T, addr string) *SMTPMailer {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewSMTP(SMTPConfig{
		Host: host, Port: port,
		Username: "noreply@example.com", Password: "whatever",
		From:                     "noreply@example.com",
		ImplicitTLS:              false,
		AllowPlainAuthWithoutTLS: true, // 测试用明文连接
		Timeout:                  3 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSMTP: %v", err)
	}
	return m
}

// 服务端拒绝认证（阿里/网易的 5xx）必须被判定为**永久性错误**：
// 不再退避重试，错误信息里要能直接看到该服务商的排查清单。
func TestSendAuthFailureIsPermanentAndActionable(t *testing.T) {
	addr, stop := startFakeSMTP(t, "526 Authentication failure[0]")
	defer stop()

	m := newMailerForAddr(t, addr)
	err := m.Send(context.Background(), Message{To: "alice@example.com", Subject: "s", Text: "t"})
	if err == nil {
		t.Fatal("Send should fail when the server rejects authentication")
	}
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent（认证失败不应重试）", err)
	}
	msg := err.Error()
	for _, want := range []string{"认证失败", addr, "526", "请确认 SMTP 用户名是完整邮箱地址"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息缺少 %q：%s", want, msg)
		}
	}
}

// Verify 走同一套错误文案（宿主自检时看到的是同一份指引）。
func TestVerifyAuthFailureIsActionable(t *testing.T) {
	addr, stop := startFakeSMTP(t, "535 Error: authentication failed")
	defer stop()

	m := newMailerForAddr(t, addr)
	err := m.Verify(context.Background())
	if err == nil {
		t.Fatal("Verify should fail on bad credentials")
	}
	if !strings.Contains(err.Error(), "认证失败") || !strings.Contains(err.Error(), "535") {
		t.Fatalf("Verify error should carry the server response and a hint: %v", err)
	}
}

// 会话建立失败（连接被拒）是**临时性**错误，应当继续重试。
func TestSendDialFailureIsRetryable(t *testing.T) {
	addr, stop := startFakeSMTP(t, "220 ok")
	stop() // 立刻关掉，制造连接被拒

	m := newMailerForAddr(t, addr)
	err := m.Send(context.Background(), Message{To: "alice@example.com", Subject: "s", Text: "t"})
	if err == nil {
		t.Fatal("Send should fail against a closed port")
	}
	if errors.Is(err, ErrPermanent) {
		t.Fatalf("网络错误不应被标记为永久失败：%v", err)
	}
}
