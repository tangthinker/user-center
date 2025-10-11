package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/tangthinker/user-center/internal/db"
	"github.com/tangthinker/user-center/internal/schema"
	"gorm.io/gorm"
)

type UserModel struct {
	DB *gorm.DB
}

func NewUserModel() *UserModel {
	d := db.GetDB()
	if err := d.AutoMigrate(&schema.User{}); err != nil {
		panicStr := fmt.Errorf("UserModel: Init User table error:%w", err)
		panic(panicStr)
	}

	return &UserModel{
		DB: d,
	}
}

func (u *UserModel) Create(ctx context.Context, user *schema.User) error {
	return u.DB.WithContext(ctx).Create(user).Error
}

func (u *UserModel) Update(ctx context.Context, user *schema.User) error {
	return u.DB.WithContext(ctx).Save(user).Error
}

func (u *UserModel) Delete(ctx context.Context, user *schema.User) error {
	return u.DB.WithContext(ctx).Delete(user).Error
}

func (u *UserModel) GetByID(ctx context.Context, ID int64) (*schema.User, error) {
	user := &schema.User{}
	if err := u.DB.WithContext(ctx).First(user, ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return user, nil
}

func (u *UserModel) GetByUid(ctx context.Context, uid string) (*schema.User, error) {
	user := &schema.User{}
	if err := u.DB.WithContext(ctx).Where("uid = ?", uid).First(user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return user, nil
}
