package manager

import (
	"github.com/tangthinker/user-center/internal/helper/pwdencry"
	"github.com/tangthinker/user-center/internal/model"
	"github.com/tangthinker/user-center/internal/schema"
	"github.com/tangthinker/user-center/internal/service/auth"
)

type Manager interface {
	Login(uid string, password string) (string, error)
	Register(uid string, password string) error
	ModifyPassword(uid string, oldPassword string, newPassword string) error
	UidUnique(uid string) bool
}

type CommonManager struct {
	userModel    *model.UserModel
	auth         auth.Auth
	pwdEncryptor pwdencry.Encryptor
}

func NewCommonManager() Manager {
	return &CommonManager{
		userModel:    model.NewUserModel(),
		auth:         auth.NewCommonAuth(),
		pwdEncryptor: pwdencry.NewCommonEncryptor(),
	}
}

func (m *CommonManager) Login(uid string, password string) (string, error) {
	user, err := m.userModel.GetByUid(uid)
	if err != nil {
		return "", err
	}

	if user == nil {
		return "", nil
	}

	encryptedPwd, err := m.pwdEncryptor.Encrypt(password)
	if err != nil {
		return "", err
	}

	if user.Password != encryptedPwd {
		return "", nil
	}

	return m.auth.Sign(uid)
}

func (m *CommonManager) Register(uid string, password string) error {
	password, err := m.pwdEncryptor.Encrypt(password)
	if err != nil {
		return err
	}
	user := &schema.User{
		Uid:      uid,
		Password: password,
	}

	return m.userModel.Create(user)
}

func (m *CommonManager) ModifyPassword(uid string, oldPassword string, newPassword string) error {
	user, err := m.userModel.GetByUid(uid)
	if err != nil {
		return err
	}

	if user == nil {
		return nil
	}

	encryptedPwd, err := m.pwdEncryptor.Encrypt(oldPassword)
	if err != nil {
		return err
	}

	if user.Password != encryptedPwd {
		return nil
	}

	newPassword, err = m.pwdEncryptor.Encrypt(newPassword)
	if err != nil {
		return err
	}

	user.Password = newPassword
	return m.userModel.Update(user)
}

func (m *CommonManager) UidUnique(uid string) bool {
	user, err := m.userModel.GetByUid(uid)
	if err != nil {
		return false
	}
	return user == nil
}
