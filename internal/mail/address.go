package mail

import (
	"errors"
	"fmt"
	"strings"
)

// 地址与头部的长度上限（防御性约束）。
const (
	maxAddressLength = 254
	maxSubjectLength = 300
)

// ErrInvalidAddress 表示邮箱地址不合法。
var ErrInvalidAddress = errors.New("mail: invalid email address")

// ValidateAddress 做基本的邮箱格式校验。
//
// 校验目标是"能安全地放进 SMTP 信封"，而不是 RFC 5322 完备性：
//   - 必须恰好一个 '@'，本地部分与域名部分都非空；
//   - 域名部分必须含 '.'；
//   - 不得含空白、控制字符、CR/LF（防头部注入）；
//   - 长度受限。
//
// 注意：邮箱是本系统唯一的登录凭据，因此这个校验点也是
// "管理员建用户时打错字"的第一道拦截。
func ValidateAddress(addr string) error {
	if addr == "" {
		return fmt.Errorf("%w: empty", ErrInvalidAddress)
	}
	if len(addr) > maxAddressLength {
		return fmt.Errorf("%w: too long", ErrInvalidAddress)
	}
	if strings.ContainsAny(addr, " \t\r\n\x00") {
		return fmt.Errorf("%w: contains whitespace or control characters", ErrInvalidAddress)
	}
	if strings.Count(addr, "@") != 1 {
		return fmt.Errorf("%w: must contain exactly one '@'", ErrInvalidAddress)
	}

	local, domain, _ := strings.Cut(addr, "@")
	if local == "" || domain == "" {
		return fmt.Errorf("%w: empty local or domain part", ErrInvalidAddress)
	}
	if !strings.Contains(domain, ".") {
		return fmt.Errorf("%w: domain has no dot", ErrInvalidAddress)
	}
	if strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") ||
		strings.Contains(domain, "..") {
		return fmt.Errorf("%w: malformed domain", ErrInvalidAddress)
	}
	if strings.ContainsAny(local, "<>,\"") || strings.ContainsAny(domain, "<>,\"/\\") {
		return fmt.Errorf("%w: illegal characters", ErrInvalidAddress)
	}
	return nil
}

// sanitizeHeader 拒绝会破坏头部结构的取值（CR/LF 注入）。
func sanitizeHeader(name, value string) (string, error) {
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("mail: %s must not contain CR/LF (header injection)", name)
	}
	return strings.TrimSpace(value), nil
}
