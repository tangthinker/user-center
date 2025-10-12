package auth

import (
	"crypto/md5"
	"fmt"
	"github.com/tangthinker/user-center/internal/constrant"
	"strings"
	"sync"
	"time"
)

type TokenInfo struct {
	Uid      string
	SignedAt time.Time
	ExpireAt time.Time
}

type MemoryAuth struct {
	tokenMap map[string]*TokenInfo
	mu       *sync.RWMutex
}

var memoryAuth *MemoryAuth
var once sync.Once

func NewMemoryAuth() Auth {
	once.Do(func() {
		memoryAuth = &MemoryAuth{
			tokenMap: make(map[string]*TokenInfo),
			mu:       new(sync.RWMutex),
		}
	})
	return memoryAuth
}

func (m *MemoryAuth) Sign(uid string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	expireAt := time.Now().Add(constrant.DefaultTokenTTL)
	tokenInfo := &TokenInfo{
		Uid:      uid,
		SignedAt: time.Now(),
		ExpireAt: expireAt,
	}

	token := genToken(uid, constrant.TokenSecret)
	m.tokenMap[token] = tokenInfo
	return token, nil
}

func (m *MemoryAuth) Verify(token string) (string, error) {
	m.mu.RLock()
	info, ok := m.tokenMap[token]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("memoery auth: token is invalid: %s", token)
	}
	if info.ExpireAt.Before(time.Now()) {
		return "", fmt.Errorf("memory auth: token is expired: %s", token)
	}

	info.ExpireAt = time.Now().Add(constrant.DefaultTokenTTL)
	m.mu.Lock()
	m.tokenMap[token] = info
	m.mu.Unlock()

	return info.Uid, nil
}

func genToken(uid string, secret string) string {
	hash := md5.New()

	hash.Write([]byte(uid))
	hash.Write([]byte(secret))

	token := fmt.Sprintf("%x", hash.Sum(nil))

	token = strings.ToUpper(token)

	return fmt.Sprintf("Thinker-%s", token)
}
