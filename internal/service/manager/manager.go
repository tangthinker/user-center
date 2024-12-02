package manager

import (
	"fmt"
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
		return "", fmt.Errorf("get user by uid error: %w", err)
	}

	if user == nil {
		return "", fmt.Errorf("uid not found")
	}

	encryptedPwd, err := m.pwdEncryptor.Encrypt(password)
	if err != nil {
		return "", fmt.Errorf("encrypt password error: %w", err)
	}

	if user.Password != encryptedPwd {
		return "", fmt.Errorf("password not match")
	}

	return m.auth.Sign(uid)
}

func (m *CommonManager) Register(uid string, password string) error {
	password, err := m.pwdEncryptor.Encrypt(password)
	if err != nil {
		return fmt.Errorf("encrypt password error: %w", err)
	}
	user := &schema.User{
		Uid:      uid,
		Password: password,
	}

	err = m.userModel.Create(user)
	if err != nil {
		return fmt.Errorf("create user error: %w", err)
	}

	return nil
}

func (m *CommonManager) ModifyPassword(uid string, oldPassword string, newPassword string) error {
	user, err := m.userModel.GetByUid(uid)
	if err != nil {
		return fmt.Errorf("get user by uid error: %w", err)
	}

	if user == nil {
		return fmt.Errorf("uid not found")
	}

	encryptedPwd, err := m.pwdEncryptor.Encrypt(oldPassword)
	if err != nil {
		return fmt.Errorf("encrypt password error: %w", err)
	}

	if user.Password != encryptedPwd {
		return fmt.Errorf("password not match")
	}

	newPassword, err = m.pwdEncryptor.Encrypt(newPassword)
	if err != nil {
		return fmt.Errorf("encrypt password error: %w", err)
	}

	user.Password = newPassword
	err = m.userModel.Update(user)
	if err != nil {
		return fmt.Errorf("update user error: %w", err)
	}

	return nil
}

func (m *CommonManager) UidUnique(uid string) bool {
	user, err := m.userModel.GetByUid(uid)
	if err != nil {
		return false
	}
	return user == nil
}
