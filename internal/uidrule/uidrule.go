// Package uidrule 校验用户自选 uid。
//
// 规则来自 docs/auth-redesign.md §5.2（已确认的用户决策）：
//   - **大小写敏感**（Alice ≠ alice）
//   - 最少 3 个字符
//   - 不能以数字开头
//   - 最长 32 个字符，首字符为字母，其余为字母/数字/下划线/连字符
//   - 保留字**大小写不敏感**匹配（admin / Admin / ADMIN 全部拒绝）
//
// 最后一条与"大小写敏感"是配套的：若保留字也按大小写敏感比较，
// 攻击者就能抢注 `Admin` 来仿冒 `admin`。
package uidrule

import (
	"errors"
	"fmt"
	"strings"
)

// 长度边界。
const (
	MinLen = 3
	MaxLen = 32
)

// 校验失败的具体原因。uid 格式是本系统**唯一**允许向用户返回具体
// 原因的地方（其余认证错误一律统一文案，见设计 §6.6）。
var (
	ErrEmpty     = errors.New("uid 不能为空")
	ErrTooShort  = fmt.Errorf("uid 至少需要 %d 个字符", MinLen)
	ErrTooLong   = fmt.Errorf("uid 最多 %d 个字符", MaxLen)
	ErrFirstChar = errors.New("uid 必须以字母开头")
	ErrCharset   = errors.New("uid 只能包含字母、数字、下划线和连字符")
	ErrSpace     = errors.New("uid 不能包含空白字符")
	ErrReserved  = errors.New("uid 是保留字，请换一个")
)

// reserved 是**默认**保留字表，比较时统一转小写。
//
// 收录判据只有一条：用户取了之后，别人可能**误以为是系统或运营方的身份**
// （admin / support / noreply / system …）。
//
// 因此这里刻意**不包含**任何宿主专有名词、产品名或作者的个人 ID——
// 本库会被多个宿主引用，把某一个宿主或作者的名字写进默认表，等于替别人的
// 部署做决定。宿主若想保护自己的名字，用 Config.ReservedUIDs 追加。
var reserved = map[string]struct{}{
	"admin": {}, "administrator": {}, "root": {}, "system": {}, "sys": {},
	"api": {}, "www": {}, "mail": {}, "smtp": {}, "noreply": {}, "no-reply": {},
	"support": {}, "help": {}, "service": {}, "official": {}, "security": {},
	"null": {}, "undefined": {}, "true": {}, "false": {},
	"invite": {}, "invitation": {}, "otp": {}, "login": {}, "logout": {},
	"register": {}, "signup": {}, "session": {}, "token": {},
	"static": {}, "assets": {}, "public": {}, "private": {},
	"user": {}, "users": {}, "me": {}, "self": {}, "guest": {}, "anonymous": {},
	"test": {}, "demo": {},
}

// IsReservedExtra 判断 uid 是否命中宿主追加的保留字（大小写不敏感）。
//
// 空值与空白项一律忽略：宿主名单里一个空行不应该让"空 uid"也变成保留字。
func IsReservedExtra(uid string, extra []string) bool {
	if len(extra) == 0 {
		return false
	}
	target := strings.ToLower(strings.TrimSpace(uid))
	if target == "" {
		return false
	}
	for _, e := range extra {
		candidate := strings.ToLower(strings.TrimSpace(e))
		if candidate != "" && candidate == target {
			return true
		}
	}
	return false
}

// Validate 校验 uid 是否合规。
//
// 注意：它**不检查唯一性**（唯一性由数据库部分唯一索引裁决，见
// internal/invite 的 Consume）。
func Validate(uid string) error {
	if uid == "" {
		return ErrEmpty
	}
	if strings.ContainsAny(uid, " \t\r\n\u00a0") {
		return ErrSpace
	}
	if len([]rune(uid)) < MinLen {
		return ErrTooShort
	}
	if len([]rune(uid)) > MaxLen {
		return ErrTooLong
	}

	runes := []rune(uid)
	if !isLetter(runes[0]) {
		return ErrFirstChar
	}
	for _, r := range runes {
		if !isLetter(r) && !isDigit(r) && r != '_' && r != '-' {
			return ErrCharset
		}
	}
	if IsReserved(uid) {
		return ErrReserved
	}
	return nil
}

// IsReserved 判断 uid 是否为保留字（大小写不敏感）。
func IsReserved(uid string) bool {
	_, ok := reserved[strings.ToLower(uid)]
	return ok
}

// IsValid 是 Validate 的布尔形式。
func IsValid(uid string) bool { return Validate(uid) == nil }

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }
