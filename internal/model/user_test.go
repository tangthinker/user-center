package model

import (
	"context"
	"os"
	"testing"

	"github.com/tangthinker/user-center/internal/schema"
)

func TestUserModel(t *testing.T) {

	wd, _ := os.Getwd()

	t.Log(wd)

	userModel := NewUserModel()

	user := &schema.User{
		Uid:      "tangthinker",
		Password: "333",
	}

	if err := userModel.Create(context.Background(), user); err != nil {
		t.Error(err)
		return
	}

	userInDB, err := userModel.GetByUid(context.Background(), user.Uid)
	if err != nil {
		t.Error(err)
		return
	}

	t.Log(*userInDB)

}
