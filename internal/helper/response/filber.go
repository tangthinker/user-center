package response

import "github.com/gofiber/fiber/v2"

func Success(ctx *fiber.Ctx, data interface{}) error {
	if data == nil {
		return ctx.JSON(fiber.Map{
			"code": 0,
			"msg":  "success",
		})
	}
	return ctx.JSON(fiber.Map{
		"code": 0,
		"msg":  "success",
		"data": data,
	})
}

func Error(ctx *fiber.Ctx, code int, msg string) error {
	return ctx.JSON(fiber.Map{
		"code": code,
		"msg":  msg,
	})
}
