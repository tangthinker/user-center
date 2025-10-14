package manager

import (
	"context"

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
		authService:    auth.GetPebbleAuth(),
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

	loginResp, err := a.managerService.Login(context.Background(), &req)
	if err != nil {
		return response.Error(ctx, fiber.StatusUnauthorized, "login failed: "+err.Error())
	}

	if loginResp.Token == "" {
		ctx.Status(fiber.StatusUnauthorized)
		return response.Error(ctx, fiber.StatusUnauthorized, "login failed: invalid uid or password")
	}

	return response.Success(ctx, loginResp)

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

	registerResp, err := a.managerService.Register(context.Background(), &req)
	if err != nil {
		return response.Error(ctx, fiber.StatusInternalServerError, "register failed: "+err.Error())
	}
	return response.Success(ctx, registerResp)
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

	modifyPasswordResp, err := a.managerService.ModifyPassword(context.Background(), &req)
	if err != nil {
		return response.Error(ctx, fiber.StatusInternalServerError, "modify password failed: "+err.Error())
	}

	return response.Success(ctx, modifyPasswordResp)
}

func (a *Api) UidUnique(ctx *fiber.Ctx) error {
	var (
		req data.UidUniqueReq
	)

	if ctx.BodyParser(&req) != nil {
		ctx.Status(fiber.StatusBadRequest)
		return nil
	}

	uidUniqueResp, err := a.managerService.UidUnique(context.Background(), &req)
	if err != nil {
		return response.Error(ctx, fiber.StatusInternalServerError, "uid unique failed: "+err.Error())
	}

	return response.Success(ctx, uidUniqueResp)
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
