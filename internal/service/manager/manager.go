package manager

import (
	"context"
	"fmt"

	"github.com/tangthinker/user-center/internal/data"
	"github.com/tangthinker/user-center/internal/helper/pwdencry"
	"github.com/tangthinker/user-center/internal/model"
	"github.com/tangthinker/user-center/internal/schema"
	"github.com/tangthinker/user-center/internal/service/auth"
)

type Manager interface {
	Login(ctx context.Context, params *data.LoginReq) (*data.LoginResp, error)
	Register(ctx context.Context, params *data.RegisterReq) (*data.RegisterResp, error)
	ModifyPassword(ctx context.Context, params *data.ModifyPasswordReq) (*data.ModifyPasswordResp, error)
	UidUnique(ctx context.Context, params *data.UidUniqueReq) (*data.UidUniqueResp, error)
}

type CommonManager struct {
	userModel    *model.UserModel
	auth         auth.Auth
	pwdEncryptor pwdencry.Encryptor
}

func NewCommonManager() Manager {
	return &CommonManager{
		userModel:    model.NewUserModel(),
		auth:         auth.NewMemoryAuth(),
		pwdEncryptor: pwdencry.NewCommonEncryptor(),
	}
}

func (m *CommonManager) Login(ctx context.Context, params *data.LoginReq) (*data.LoginResp, error) {
	user, err := m.userModel.GetByUid(ctx, params.Uid)
	if err != nil {
		return nil, fmt.Errorf("get user by uid error: %w", err)
	}

	if user == nil {
		return nil, fmt.Errorf("uid not found")
	}

	encryptedPwd, err := m.pwdEncryptor.Encrypt(params.Password)
	if err != nil {
		return nil, fmt.Errorf("encrypt password error: %w", err)
	}

	if user.Password != encryptedPwd {
		return nil, fmt.Errorf("password not match")
	}

	token, err := m.auth.Sign(params.Uid)
	if err != nil {
		return nil, fmt.Errorf("sign token error: %w", err)
	}

	return &data.LoginResp{
		Token: token,
	}, nil
}

func (m *CommonManager) Register(ctx context.Context, params *data.RegisterReq) (*data.RegisterResp, error) {
	password, err := m.pwdEncryptor.Encrypt(params.Password)
	if err != nil {
		return nil, fmt.Errorf("encrypt password error: %w", err)
	}
	user := &schema.User{
		Uid:      params.Uid,
		Password: password,
	}

	err = m.userModel.Create(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("create user error: %w", err)
	}

	return &data.RegisterResp{}, nil
}

func (m *CommonManager) ModifyPassword(ctx context.Context, params *data.ModifyPasswordReq) (*data.ModifyPasswordResp, error) {
	tx := m.userModel.DB.WithContext(ctx).Begin()

	txModel := model.UserModel{
		DB: tx,
	}
	needRollback := true

	defer func() {
		if needRollback {
			tx.Rollback()
		}
	}()

	user, err := txModel.GetByUid(ctx, params.Uid)
	if err != nil {
		return nil, fmt.Errorf("get user by uid error: %w", err)
	}

	if user == nil {
		return nil, fmt.Errorf("uid not found")
	}

	encryptedPwd, err := m.pwdEncryptor.Encrypt(params.OldPassword)
	if err != nil {
		return nil, fmt.Errorf("encrypt password error: %w", err)
	}

	if user.Password != encryptedPwd {
		return nil, fmt.Errorf("password not match")
	}

	newPassword, err := m.pwdEncryptor.Encrypt(params.NewPassword)
	if err != nil {
		return nil, fmt.Errorf("encrypt password error: %w", err)
	}

	user.Password = newPassword
	err = txModel.Update(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("update user error: %w", err)
	}

	if err := tx.Commit().Error; err != nil {
		return nil, fmt.Errorf("commit transaction error: %w", err)
	}
	needRollback = false

	return &data.ModifyPasswordResp{}, nil
}

func (m *CommonManager) UidUnique(ctx context.Context, params *data.UidUniqueReq) (*data.UidUniqueResp, error) {
	user, err := m.userModel.GetByUid(ctx, params.Uid)
	if err != nil {
		return nil, fmt.Errorf("get user by uid error: %w", err)
	}
	return &data.UidUniqueResp{
		Unique: user == nil,
	}, nil
}
