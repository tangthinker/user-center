package model

import (
	"github.com/tangthinker/user-center/internal/schema"
	"os"
	"testing"
)

func TestUserModel(t *testing.T) {

	wd, _ := os.Getwd()

	t.Log(wd)

	userModel := NewUserModel()

	user := &schema.User{
		Uid:      "tangthinker",
		Password: "333",
	}

	if err := userModel.Create(user); err != nil {
		t.Error(err)
		return
	}

	userInDB, err := userModel.GetByUid(user.Uid)
	if err != nil {
		t.Error(err)
		return
	}

	t.Log(*userInDB)

}
