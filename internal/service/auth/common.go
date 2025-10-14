package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type TokenInfo struct {
	Uid      string
	SignedAt time.Time
	ExpireAt time.Time
}

func genToken(uid string, secret string, signTime time.Time) string {
	// 生成随机数增强唯一性
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		// 如果随机数生成失败，使用时间戳作为备选
		randomBytes = []byte(fmt.Sprintf("%d", signTime.UnixNano()))
	}

	// 使用分隔符防止字符串拼接导致的哈希冲突
	// 格式: uid|secret|timestamp|random
	data := fmt.Sprintf("%s|%s|%d|%s",
		uid,
		secret,
		signTime.UnixNano(), // 使用纳秒级时间戳
		hex.EncodeToString(randomBytes))

	// 使用 SHA-256 替代 MD5
	hash := sha256.Sum256([]byte(data))

	// 转换为十六进制字符串并转为大写
	token := strings.ToUpper(hex.EncodeToString(hash[:]))

	// 优化 token 格式，添加版本标识和分段
	// 格式: Thinker-v1-{hash}
	return fmt.Sprintf("Thinker-v1-%s", token)
}
