package auth

import (
	"crypto/md5"
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

var memoryAuth *MemoryAuth
var once sync.Once

func NewMemoryAuth() Auth {
	once.Do(func() {
		memoryAuth = &MemoryAuth{
			tokenMap: make(map[string]*TokenInfo),
			mu:       new(sync.Mutex),
		}
	})
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
	hash := md5.New()

	hash.Write([]byte(uid))
	hash.Write([]byte(secret))
	hash.Write([]byte(fmt.Sprintf("%d", signTime.UnixMilli())))

	token := fmt.Sprintf("%x", hash.Sum(nil))

	token = strings.ToUpper(token)

	return fmt.Sprintf("Thinker-%s", token)
}
