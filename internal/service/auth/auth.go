package auth

import (
	"github.com/tangthinker/jwt-model/core"
	"github.com/tangthinker/user-center/internal/constrant"
)

type Auth interface {
	Sign(uid string) (string, error)
	Verify(token string) (string, error)
}

type CommonAuth struct {
	jwtAuthor core.Author
}

func NewCommonAuth() Auth {
	return &CommonAuth{
		jwtAuthor: core.NewJWTAuthor(constrant.DefaultTokenTTL, constrant.TokenSecret),
	}
}

func (c *CommonAuth) Sign(uid string) (string, error) {
	return c.jwtAuthor.AuthString(uid, "")
}

func (c *CommonAuth) Verify(token string) (string, error) {
	uid, _, err := c.jwtAuthor.Verify(token)
	if err != nil {
		return "", err
	}

	return uid, nil
}
