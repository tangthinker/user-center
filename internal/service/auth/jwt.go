package auth

import (
	"github.com/tangthinker/jwt-model/core"
	"github.com/tangthinker/user-center/internal/constrant"
)

type JWTAuth struct {
	jwtAuthor core.Author
}

func NewJWTAuth() Auth {
	return &JWTAuth{
		jwtAuthor: core.NewJWTAuthor(constrant.DefaultTokenTTL, constrant.TokenSecret),
	}
}

func (c *JWTAuth) Sign(uid string) (string, error) {
	return c.jwtAuthor.AuthString(uid, "")
}

func (c *JWTAuth) Verify(token string) (string, error) {
	uid, _, err := c.jwtAuthor.Verify(token)
	if err != nil {
		return "", err
	}

	return uid, nil
}
