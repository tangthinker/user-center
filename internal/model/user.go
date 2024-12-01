package model

import (
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

func (u *UserModel) Create(user *schema.User) error {
	return u.DB.Create(user).Error
}

func (u *UserModel) Update(user *schema.User) error {
	return u.DB.Save(user).Error
}

func (u *UserModel) Delete(user *schema.User) error {
	return u.DB.Delete(user).Error
}

func (u *UserModel) GetByID(ID int64) (*schema.User, error) {
	user := &schema.User{}
	if err := u.DB.First(user, ID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return user, nil
}

func (u *UserModel) GetByUid(uid string) (*schema.User, error) {
	user := &schema.User{}
	if err := u.DB.Where("uid = ?", uid).First(user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return user, nil
}
