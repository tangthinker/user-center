package main

import (
	"github.com/gofiber/fiber/v3"
	"log"
)

func main() {

	app := fiber.New()

	app.Get("/", func(ctx fiber.Ctx) error {
		return ctx.SendString("Hello, World!")
	})

	log.Fatal(app.Listen(":3000"))

}
