package mail

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"mime"

	"strings"
	"time"
)

// BuildRFC822 把 Message 组装为可直接投递的 RFC 822 报文。
//
// 单独抽出来是为了能在**不连网络**的前提下测试头部构造、
// UTF-8 主题编码、多部分正文以及头部注入防护。
func BuildRFC822(from, fromName string, msg Message) ([]byte, error) {
	if err := ValidateAddress(from); err != nil {
		return nil, fmt.Errorf("mail: from address: %w", err)
	}
	if err := ValidateAddress(msg.To); err != nil {
		return nil, fmt.Errorf("mail: to address: %w", err)
	}

	to, err := sanitizeHeader("To", msg.To)
	if err != nil {
		return nil, err
	}
	subject, err := sanitizeHeader("Subject", msg.Subject)
	if err != nil {
		return nil, err
	}
	if subject == "" {
		return nil, fmt.Errorf("mail: Subject is required")
	}
	if len(subject) > maxSubjectLength {
		subject = subject[:maxSubjectLength]
	}

	fromHeaderValue := from
	if name, err := sanitizeHeader("FromName", fromName); err != nil {
		return nil, err
	} else if name != "" {
		fromHeaderValue = fmt.Sprintf("%s <%s>", mime.QEncoding.Encode("utf-8", name), from)
	}

	var b strings.Builder
	writeHeader := func(k, v string) {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteString("\r\n")
	}

	writeHeader("From", fromHeaderValue)
	writeHeader("To", to)
	// 主题一律用 RFC 2047 编码，避免非 ASCII 主题被破坏
	writeHeader("Subject", mime.QEncoding.Encode("utf-8", subject))
	writeHeader("Date", time.Now().UTC().Format(time.RFC1123Z))
	writeHeader("Message-ID", newMessageID(from))
	writeHeader("MIME-Version", "1.0")
	writeHeader("Auto-Submitted", "auto-generated")
	writeHeader("X-Auto-Response-Suppress", "All")

	if msg.HTML == "" {
		writeHeader("Content-Type", `text/plain; charset="utf-8"`)
		writeHeader("Content-Transfer-Encoding", "base64")
		b.WriteString("\r\n")
		b.WriteString(wrapBase64(msg.Text))
		b.WriteString("\r\n")
		return []byte(b.String()), nil
	}

	boundary := newBoundary()
	writeHeader("Content-Type", fmt.Sprintf(`multipart/alternative; boundary="%s"`, boundary))
	b.WriteString("\r\n")

	writePart := func(contentType, body string) {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + contentType + "\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		b.WriteString(wrapBase64(body))
		b.WriteString("\r\n")
	}
	writePart(`text/plain; charset="utf-8"`, msg.Text)
	writePart(`text/html; charset="utf-8"`, msg.HTML)
	b.WriteString("--" + boundary + "--\r\n")

	return []byte(b.String()), nil
}

// wrapBase64 按 RFC 2045 要求每行不超过 76 字符。
func wrapBase64(s string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(s))
	var out strings.Builder
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		out.WriteString(encoded[i:end])
		out.WriteString("\r\n")
	}
	return out.String()
}

func newMessageID(from string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// 退化为时间戳：Message-ID 只用于去重展示，不承担安全职责
		return fmt.Sprintf("<%d@user-center>", time.Now().UnixNano())
	}
	domain := "user-center"
	if _, d, ok := strings.Cut(from, "@"); ok && d != "" {
		domain = d
	}
	return "<" + hex.EncodeToString(buf) + "@" + domain + ">"
}

func newBoundary() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("boundary-%d", time.Now().UnixNano())
	}
	return "uc-" + hex.EncodeToString(buf)
}
