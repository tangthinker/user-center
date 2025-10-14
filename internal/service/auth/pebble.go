package auth

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/tangthinker/user-center/internal/constrant"
	"github.com/tangthinker/user-center/internal/db"
)

type PebbleAuth struct {
	db     *pebble.DB
	dbpath string
}

var (
	once sync.Once
	auth *PebbleAuth
)

func GetPebbleAuth() *PebbleAuth {
	once.Do(func() {
		dbRootPath := db.GetDBPath()
		pebblePath := "pebble-token-auth.db"
		if dbRootPath != "" {
			pebblePath = dbRootPath + string(filepath.Separator) + pebblePath
		}
		pebbleDB, err := pebble.Open(pebblePath, &pebble.Options{})
		if err != nil {
			panic("open pebble: " + err.Error())
		}
		auth = &PebbleAuth{
			db:     pebbleDB,
			dbpath: pebblePath,
		}
	})
	return auth
}

func (a *PebbleAuth) Get(key string) (string, error) {
	value, closer, err := a.db.Get([]byte(key))
	if err != nil {
		return "", err
	}

	defer closer.Close()

	valueCopy := make([]byte, len(value))
	copy(valueCopy, value)
	return string(valueCopy), nil
}

func (a *PebbleAuth) Set(key string, value string) error {
	return a.db.Set([]byte(key), []byte(value), pebble.NoSync)
}

func (a *PebbleAuth) SetAny(key string, value any) error {
	jsData, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return a.Set(key, string(jsData))
}

func (a *PebbleAuth) Del(key string) error {
	return a.db.Delete([]byte(key), pebble.Sync)
}

func (a *PebbleAuth) Sign(uid string) (string, error) {
	now := time.Now()
	token := genToken(uid, constrant.TokenSecret, now)
	tokenInfo := &TokenInfo{
		Uid:      uid,
		SignedAt: now,
		ExpireAt: now.Add(constrant.DefaultTokenTTL),
	}

	err := a.SetAny(token, tokenInfo)
	if err != nil {
		return "", err
	}
	return token, nil
}

func (a *PebbleAuth) Verify(token string) (string, error) {
	tokenInfoJson, err := a.Get(token)
	if errors.Is(err, pebble.ErrNotFound) {
		return "", errors.New("token not found")
	}
	if err != nil {
		return "", err
	}
	var tokenInfo TokenInfo
	err = json.Unmarshal([]byte(tokenInfoJson), &tokenInfo)
	if err != nil {
		return "", err
	}
	now := time.Now()
	if now.After(tokenInfo.ExpireAt) {
		return "", errors.New("token expired")
	}
	return tokenInfo.Uid, nil
}
