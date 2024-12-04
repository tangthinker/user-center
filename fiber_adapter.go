package main

import (
	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/internal/api/manager"
)

func RegisterUserCenter(router fiber.Router) {
	authApi := manager.NewApi()

	authRouter := router.Group("/auth")

	authRouter.Post("/uid-unique", authApi.UidUnique)
	authRouter.Post("/login", authApi.Login)
	authRouter.Post("/register", authApi.Register)
	authRouter.Post("/modify-password", authApi.ModifyPassword)

	authRouter.Post("/token-valid", authApi.Verify)
}
