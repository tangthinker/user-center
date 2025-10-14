package auth

import (
	"fmt"
	"sync"
	"time"

	"github.com/tangthinker/user-center/internal/constrant"
)

type MemoryAuth struct {
	tokenMap sync.Map
}

var memoryAuth *MemoryAuth = &MemoryAuth{}

func NewMemoryAuth() Auth {
	return memoryAuth
}

func (m *MemoryAuth) Sign(uid string) (string, error) {
	now := time.Now()
	expireAt := now.Add(constrant.DefaultTokenTTL)
	tokenInfo := TokenInfo{
		Uid:      uid,
		SignedAt: now,
		ExpireAt: expireAt,
	}

	token := genToken(uid, constrant.TokenSecret, now)
	m.tokenMap.Store(token, tokenInfo)
	return token, nil
}

func (m *MemoryAuth) Verify(token string) (string, error) {
	value, ok := m.tokenMap.Load(token)
	if !ok {
		return "", fmt.Errorf("memoery auth: token is invalid: %s", token)
	}
	info, ok := value.(TokenInfo)
	if !ok {
		return "", fmt.Errorf("memoery auth: token is invalid: %s", token)
	}
	if info.ExpireAt.Before(time.Now()) {
		return "", fmt.Errorf("memory auth: token is expired: %s", token)
	}

	info.ExpireAt = time.Now().Add(constrant.DefaultTokenTTL)
	m.tokenMap.Store(token, info)

	return info.Uid, nil
}
