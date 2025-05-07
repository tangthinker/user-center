package pkg

import (
	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/internal/api/manager"
	"github.com/tangthinker/user-center/internal/db"
	"github.com/tangthinker/user-center/internal/service/auth"
)

func RegisterUserCenter(router fiber.Router, userDBRootPth string) {
	db.SetDBPath(userDBRootPth)

	authApi := manager.NewApi()

	authRouter := router.Group("/auth")

	authRouter.Post("/uid-unique", authApi.UidUnique)
	authRouter.Post("/login", authApi.Login)
	authRouter.Post("/register", authApi.Register)
	authRouter.Post("/modify-password", authApi.ModifyPassword)

	authRouter.Post("/token-valid", authApi.Verify)
}

func TokenValid(token string) (string, error) {
	author := auth.NewCommonAuth()
	return author.Verify(token)
}
