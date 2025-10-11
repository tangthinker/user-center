package manager

import (
	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/internal/data"
	"github.com/tangthinker/user-center/internal/helper/response"
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
		return response.Error(ctx, fiber.StatusUnauthorized, "login failed: "+err.Error())
	}

	if token == "" {
		ctx.Status(fiber.StatusUnauthorized)
		return response.Error(ctx, fiber.StatusUnauthorized, "login failed: invalid uid or password")
	}

	return response.Success(ctx, fiber.Map{
		"token": token,
	})

}

func (a *Api) Register(ctx *fiber.Ctx) error {
	var (
		req data.RegisterReq
	)

	headers := ctx.GetReqHeaders()
	if len(headers) == 0 {
		ctx.Status(fiber.StatusForbidden)
		return nil
	}
	secret := headers["X-Tangthinker-Secret"]
	if len(secret) == 0 {
		ctx.Status(fiber.StatusForbidden)
		return nil
	}
	if secret[0] != "loveVG" {
		ctx.Status(fiber.StatusForbidden)
		return nil
	}

	if ctx.BodyParser(&req) != nil {
		ctx.Status(fiber.StatusBadRequest)
		return nil
	}

	if err := a.managerService.Register(req.Uid, req.Password); err != nil {
		return response.Error(ctx, fiber.StatusInternalServerError, "register failed: "+err.Error())
	}

	return response.Success(ctx, nil)
}

func (a *Api) ModifyPassword(ctx *fiber.Ctx) error {
	var (
		req data.ModifyPasswordReq
	)

	headers := ctx.GetReqHeaders()
	if len(headers) == 0 {
		ctx.Status(fiber.StatusForbidden)
		return nil
	}
	secret := headers["X-Tangthinker-Secret"]
	if len(secret) == 0 {
		ctx.Status(fiber.StatusForbidden)
		return nil
	}
	if secret[0] != "loveVG" {
		ctx.Status(fiber.StatusForbidden)
		return nil
	}

	if ctx.BodyParser(&req) != nil {
		ctx.Status(fiber.StatusBadRequest)
		return nil
	}

	if err := a.managerService.ModifyPassword(req.Uid, req.OldPassword, req.NewPassword); err != nil {
		return response.Error(ctx, fiber.StatusInternalServerError, "modify password failed: "+err.Error())
	}

	return response.Success(ctx, nil)
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
		return response.Success(ctx, fiber.Map{
			"unique": true,
		})
	}

	return response.Success(ctx, fiber.Map{
		"unique": false,
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
		return response.Error(ctx, fiber.StatusUnauthorized, "verify failed: "+err.Error())
	}

	return response.Success(ctx, fiber.Map{
		"uid": uid,
	})
}
