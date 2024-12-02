package pwdencry

import "testing"

func TestCommonEncryptor(t *testing.T) {

	e := NewCommonEncryptor()

	pwd := "123456"

	encryPwd, err := e.Encrypt(pwd)

	if err != nil {
		t.Error(err)
		return
	}

	t.Log(encryPwd)

}
