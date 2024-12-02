package main

import (
	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/internal/api/manager"
	"log"
)

func main() {

	app := fiber.New()

	app.Get("/", func(ctx *fiber.Ctx) error {
		return ctx.SendString("Hello, World!")
	})

	rootRouter := app.Group("/api/v1/")

	authApi := manager.NewApi()

	rootRouter.Post("/login", authApi.Login)
	rootRouter.Post("/register", authApi.Register)
	rootRouter.Post("/modify-password", authApi.ModifyPassword)

	rootRouter.Post("/token-valid", authApi.Verify)

	log.Fatal(app.Listen(":9999"))

}
