// Package secure 提供令牌、验证码与哈希的密码学原语。
//
// 所有随机数一律来自 crypto/rand；本包**不提供**任何降级到时间戳或
// math/rand 的"兜底"路径（对照旧实现 internal/service/auth/common.go:20-24
// 在 rand 失败时静默退化为纳秒时间戳的问题）。
package secure

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// TokenBytes 是令牌与会话 token 的随机字节数（128 位以上）。
const TokenBytes = 32

// NewToken 生成一个新的不透明 token，返回明文与其 sha256。
//
// 明文只应出现在返回给用户的响应或邮件链接中；数据库只保存哈希
// （设计 §4.2 不变量 I3）。
func NewToken() (plain string, hash []byte, err error) {
	buf := make([]byte, TokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("secure: read random: %w", err)
	}
	plain = base64.RawURLEncoding.EncodeToString(buf)
	return plain, HashToken(plain), nil
}

// HashToken 计算 token 明文的 sha256。
func HashToken(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}

// NewNumericCode 生成指定长度的十进制验证码（含前导零）。
func NewNumericCode(digits int) (string, error) {
	if digits <= 0 || digits > 12 {
		return "", fmt.Errorf("secure: invalid code length %d", digits)
	}
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return "", fmt.Errorf("secure: read random: %w", err)
	}
	return fmt.Sprintf("%0*d", digits, n), nil
}

// CodeHMAC 计算验证码的 HMAC，并把 email 与 purpose 绑定进去。
//
// 绑定用途与邮箱的作用：即使数据库泄漏，攻击者也无法把某个 HMAC
// 搬到另一个 (email, purpose) 组合上使用。
func CodeHMAC(key []byte, purpose, email, code string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(purpose))
	mac.Write([]byte{0})
	mac.Write([]byte(email))
	mac.Write([]byte{0})
	mac.Write([]byte(code))
	return mac.Sum(nil)
}

// Equal 是恒定时间比较，用于校验 HMAC 与验证码。
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// EqualString 是字符串形式的恒定时间比较。
func EqualString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// HashEmail 返回邮箱的短哈希，用于 mail_log 等"需要统计但不必留明文"的场景。
func HashEmail(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:16])
}
