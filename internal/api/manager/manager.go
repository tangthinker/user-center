package manager

import (
	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/internal/data"
	"github.com/tangthinker/user-center/internal/service/auth"
	"github.com/tangthinker/user-center/internal/service/manager"
)

type Api struct {
	managerService manager.Manager
	authService    auth.Auth
}

func NewApi() *Api {
	return &Api{
		managerService: manager.NewCommonManager(),
		authService:    auth.NewCommonAuth(),
	}
}

func (a *Api) Login(ctx *fiber.Ctx) error {
	var (
		req data.LoginReq
	)

	if ctx.BodyParser(&req) != nil {
		ctx.Status(fiber.StatusBadRequest)
		return nil
	}

	token, err := a.managerService.Login(req.Uid, req.Password)
	if err != nil {
		ctx.Status(fiber.StatusInternalServerError)
		return ctx.JSON(fiber.Map{
			"code": 1,
			"msg":  "login failed: " + err.Error(),
		})
	}

	if token == "" {
		ctx.Status(fiber.StatusUnauthorized)
		return ctx.JSON(fiber.Map{
			"code": 1,
			"msg":  "login failed: invalid uid or password",
		})
	}

	return ctx.JSON(fiber.Map{
		"code": 0,
		"msg":  "success",
		"data": fiber.Map{
			"token": token,
		},
	})

}

func (a *Api) Register(ctx *fiber.Ctx) error {
	var (
		req data.RegisterReq
	)

	if ctx.BodyParser(&req) != nil {
		ctx.Status(fiber.StatusBadRequest)
		return nil
	}

	if err := a.managerService.Register(req.Uid, req.Password); err != nil {
		ctx.Status(fiber.StatusInternalServerError)
		return ctx.JSON(fiber.Map{
			"code": 1,
			"msg":  "register failed: " + err.Error(),
		})
	}

	return ctx.JSON(fiber.Map{
		"code": 0,
		"msg":  "success",
	})
}

func (a *Api) ModifyPassword(ctx *fiber.Ctx) error {
	var (
		req data.ModifyPasswordReq
	)

	if ctx.BodyParser(&req) != nil {
		ctx.Status(fiber.StatusBadRequest)
		return nil
	}

	if err := a.managerService.ModifyPassword(req.Uid, req.OldPassword, req.NewPassword); err != nil {
		ctx.Status(fiber.StatusInternalServerError)
		return ctx.JSON(fiber.Map{
			"code": 1,
			"msg":  "modify password failed: " + err.Error(),
		})
	}

	return ctx.JSON(fiber.Map{
		"code": 0,
		"msg":  "success",
	})
}

func (a *Api) UidUnique(ctx *fiber.Ctx) error {
	var (
		req data.UidUniqueReq
	)

	if ctx.BodyParser(&req) != nil {
		ctx.Status(fiber.StatusBadRequest)
		return nil
	}

	if a.managerService.UidUnique(req.Uid) {
		return ctx.JSON(fiber.Map{
			"code": 0,
			"msg":  "success",
			"data": fiber.Map{
				"unique": true,
			},
		})
	}

	return ctx.JSON(fiber.Map{
		"code": 0,
		"msg":  "success",
		"data": fiber.Map{
			"unique": false,
		},
	})
}

func (a *Api) Verify(ctx *fiber.Ctx) error {
	var (
		req data.VerifyReq
	)

	if ctx.BodyParser(&req) != nil {
		ctx.Status(fiber.StatusBadRequest)
		return nil
	}

	uid, err := a.authService.Verify(req.Token)
	if err != nil {
		ctx.Status(fiber.StatusUnauthorized)
		return ctx.JSON(fiber.Map{
			"code": 1,
			"msg":  "verify failed: " + err.Error(),
		})
	}

	return ctx.JSON(fiber.Map{
		"code": 0,
		"msg":  "success",
		"data": fiber.Map{
			"uid": uid,
		},
	})
}
