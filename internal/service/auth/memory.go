package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tangthinker/user-center/internal/constrant"
)

type TokenInfo struct {
	Uid      string
	SignedAt time.Time
	ExpireAt time.Time
}

type MemoryAuth struct {
	tokenMap map[string]*TokenInfo
	mu       *sync.Mutex
}

var memoryAuth *MemoryAuth = &MemoryAuth{
	tokenMap: make(map[string]*TokenInfo),
	mu:       new(sync.Mutex),
}

func NewMemoryAuth() Auth {
	return memoryAuth
}

func (m *MemoryAuth) Sign(uid string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	expireAt := now.Add(constrant.DefaultTokenTTL)
	tokenInfo := &TokenInfo{
		Uid:      uid,
		SignedAt: now,
		ExpireAt: expireAt,
	}

	token := genToken(uid, constrant.TokenSecret, now)
	m.tokenMap[token] = tokenInfo
	fmt.Println(token, uid, expireAt.Format(time.DateTime))
	return token, nil
}

func (m *MemoryAuth) Verify(token string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.tokenMap[token]
	if !ok {
		return "", fmt.Errorf("memoery auth: token is invalid: %s", token)
	}
	if info.ExpireAt.Before(time.Now()) {
		return "", fmt.Errorf("memory auth: token is expired: %s", token)
	}

	info.ExpireAt = time.Now().Add(constrant.DefaultTokenTTL)
	m.tokenMap[token] = info
	for t, i := range m.tokenMap {
		fmt.Println(t, i.Uid, i.SignedAt.Format(time.DateTime), i.ExpireAt.Format(time.DateTime))
	}

	return info.Uid, nil
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
