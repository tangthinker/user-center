// Package httpapi 提供公共面（登录/邀请）与管理面的 HTTP 处理器。
//
// 关键约束（docs/auth-redesign.md §0.1、§3.1、§6.6）：
//   - 库不能自己 Listen：本包只注册路由，监听地址由宿主决定（L1）；
//   - 管理面内置 LocalOnly 中间件（只认 RemoteAddr + Host 白名单），
//     即使宿主把管理路由误挂到公网监听上，外部请求也进不来；
//   - 认证失败一律返回正确的 HTTP 状态码（对照旧实现"失败也返回 200"的问题）；
//   - 对外错误文案统一，**绝不回显 err.Error()**，避免泄漏 SQL/路径细节。
package httpapi

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v2"
	"github.com/tangthinker/user-center/v2/internal/app"
)

// 统一文案表（§6.6）。这是唯一允许出现在响应里的错误措辞来源。
const (
	msgOTPSent         = "如果该邮箱已注册，验证码已发送"
	msgInvalidCode     = "邮箱或验证码不正确"
	msgInviteInvalid   = "链接无效或已过期，请联系管理员重新发送"
	msgUIDTaken        = "该用户名已被使用"
	msgEmailTaken      = "该邮箱已被使用"
	msgEmailInvalid    = "邮箱格式不正确"
	msgSessionInvalid  = "登录状态已失效，请重新登录"
	msgNotFound        = "对象不存在"
	msgForbidden       = "没有权限执行该操作"
	msgRateLimited     = "请求过于频繁，请稍后再试"
	msgInternal        = "服务暂时不可用"
	msgTooManyAttempts = msgRateLimited
)

// envelope 与旧版保持一致的外层结构（code/msg/data），
// 但**同时**设置正确的 HTTP 状态码。
type envelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data,omitempty"`
}

// ok 返回成功响应。
func ok(c *fiber.Ctx, data any) error {
	return c.Status(http.StatusOK).JSON(envelope{Code: 0, Msg: "success", Data: data})
}

// failStatus 返回指定状态码与文案的失败响应。
func failStatus(c *fiber.Ctx, status int, msg string) error {
	return c.Status(status).JSON(envelope{Code: status, Msg: msg})
}

// fail 把用例层错误映射为状态码与统一文案。
//
// reporter 用于把未预期的错误交给宿主日志（L5）；对外仍只返回通用文案。
func fail(c *fiber.Ctx, err error, reporter func(error)) error {
	if err == nil {
		return failStatus(c, http.StatusInternalServerError, msgInternal)
	}

	var rl *app.RateLimitedError
	switch {
	case errors.As(err, &rl):
		if rl.RetryAfter > 0 {
			c.Set(fiber.HeaderRetryAfter, retryAfterSeconds(rl.RetryAfter))
		}
		return failStatus(c, http.StatusTooManyRequests, msgRateLimited)

	case errors.Is(err, app.ErrInvalidCredentials):
		return failStatus(c, http.StatusUnauthorized, msgInvalidCode)

	case errors.Is(err, app.ErrSessionInvalid):
		return failStatus(c, http.StatusUnauthorized, msgSessionInvalid)

	case errors.Is(err, app.ErrInviteInvalid):
		return failStatus(c, http.StatusGone, msgInviteInvalid)

	case errors.Is(err, app.ErrUIDTaken):
		return failStatus(c, http.StatusConflict, msgUIDTaken)

	case errors.Is(err, app.ErrUIDInvalid):
		// uid 规则是本系统唯一允许向用户解释细节的地方
		return failStatus(c, http.StatusBadRequest, trimPrefix(err.Error()))

	case errors.Is(err, app.ErrEmailTaken):
		return failStatus(c, http.StatusConflict, msgEmailTaken)

	case errors.Is(err, app.ErrEmailInvalid):
		return failStatus(c, http.StatusBadRequest, msgEmailInvalid)

	case errors.Is(err, app.ErrNotFound):
		return failStatus(c, http.StatusNotFound, msgNotFound)

	case errors.Is(err, app.ErrForbidden):
		return failStatus(c, http.StatusForbidden, trimPrefix(err.Error()))

	default:
		if reporter != nil {
			reporter(err)
		}
		return failStatus(c, http.StatusInternalServerError, msgInternal)
	}
}

// trimPrefix 去掉内部包装前缀（如 "app: uid invalid: "），只留人类可读部分。
func trimPrefix(s string) string {
	for _, prefix := range []string{
		"app: uid invalid: ",
		"app: forbidden: ",
	} {
		if len(s) > len(prefix) && s[:len(prefix)] == prefix {
			return s[len(prefix):]
		}
	}
	return s
}

func retryAfterSeconds(d interface{ Seconds() float64 }) string {
	secs := int(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return itoa(secs)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
