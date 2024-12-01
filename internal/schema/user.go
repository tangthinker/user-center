package schema

import "gorm.io/gorm"

type User struct {
	gorm.Model
	Uid      string `json:"uid" gorm:"type:char(40);not null;uniqueIndex"`
	Password string `json:"password" gorm:"type:varchar(100);not null"`
}

func (u *User) TableName() string {
	return "tb_user"
}
