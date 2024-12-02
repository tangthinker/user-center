package pwdencry

import (
	"crypto/md5"
	"fmt"
	"strings"
)

type Encryptor interface {
	Encrypt(pwd string) (string, error)
}

type CommonEncryptor struct {
}

func NewCommonEncryptor() Encryptor {
	return &CommonEncryptor{}
}

func (e *CommonEncryptor) Encrypt(pwd string) (string, error) {

	hash := md5.New()

	hash.Write([]byte(pwd))

	pwd = fmt.Sprintf("%x", hash.Sum(nil))

	pwd = strings.ToUpper(pwd)

	return pwd, nil
}
