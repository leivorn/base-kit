package admin

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/leivorn/base-kit/config"
	"github.com/leivorn/base-kit/dto"
	admindto "github.com/leivorn/base-kit/dto/admin"
	"github.com/leivorn/base-kit/middleware"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	adminsvc "github.com/leivorn/base-kit/service/admin"
	"github.com/leivorn/base-kit/store"
	"github.com/leivorn/base-kit/validator"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// GetCaptcha 获取登录拼图验证码
// @Summary 获取登录拼图验证码
// @Description 系统配置 login_captcha 开启时返回一张拼图（带缺口的背景图 + 拼图块，2 分钟有效，只能提交一次）；关闭时只返回 enabled=false
// @Tags 认证
// @Produce json
// @Success 200 {object} dto.Response{data=admindto.CaptchaResponse}
// @Failure 500 {object} dto.Response
// @Router /admin/auth/captcha [get]
func GetCaptcha(c *fiber.Ctx) error {
	// 每次都要新图：不让浏览器或中间代理缓存
	c.Set(fiber.HeaderCacheControl, "no-store")
	if !adminsvc.CaptchaEnabled() {
		return dto.Success(c, admindto.CaptchaResponse{Enabled: false})
	}
	p, err := adminsvc.GenerateCaptcha()
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to generate captcha")
	}
	return dto.Success(c, admindto.CaptchaResponse{
		Enabled:   true,
		CaptchaID: p.ID,
		Image:     p.Image,
		Piece:     p.Piece,
		PieceY:    p.PieceY,
		PieceSize: adminsvc.PuzzlePieceSize,
		Width:     adminsvc.PuzzleWidth,
		Height:    adminsvc.PuzzleHeight,
	})
}

// VerifyCaptcha 校验拼图位置
// @Summary 校验拼图位置
// @Description 提交拼图块拖到的横坐标，通过时返回一次性登录凭证（2 分钟有效）。拼图提交一次即作废；失败计入来源 IP 的登录失败次数，超限返回 429
// @Tags 认证
// @Accept json
// @Produce json
// @Param request body admindto.CaptchaVerifyRequest true "拼图位置"
// @Success 200 {object} dto.Response{data=admindto.CaptchaVerifyResponse}
// @Failure 400 {object} dto.Response
// @Failure 429 {object} dto.Response
// @Router /admin/auth/captcha/verify [post]
func VerifyCaptcha(c *fiber.Ctx) error {
	var req admindto.CaptchaVerifyRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}
	ip := loginClientIP(c)
	if wait := adminsvc.CaptchaLockedFor(ip); wait > 0 {
		return dto.Fail(c, fiber.StatusTooManyRequests,
			fmt.Sprintf("Too many failed attempts, try again in %d minutes.", int(wait.Minutes())+1))
	}
	token, ok := adminsvc.VerifyCaptchaSlide(req.CaptchaID, req.X)
	if !ok {
		adminsvc.RecordCaptchaFailure(ip)
		return dto.Fail(c, fiber.StatusBadRequest, "验证失败，请重试")
	}
	return dto.Success(c, admindto.CaptchaVerifyResponse{Token: token})
}

// Login 用户登录
// @Summary 用户登录
// @Description 使用用户名和密码登录，返回 accessToken；系统配置 login_captcha 开启时还须带上拼图验证通过后拿到的 captchaToken
// @Tags 认证
// @Accept json
// @Produce json
// @Param request body admindto.LoginRequest true "登录参数"
// @Success 200 {object} dto.Response{data=admindto.LoginResponse}
// @Failure 400 {object} dto.Response
// @Failure 403 {object} dto.Response
// @Failure 429 {object} dto.Response
// @Router /admin/auth/login [post]
func Login(c *fiber.Ctx) error {
	var req admindto.LoginRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}

	ip := loginClientIP(c)
	if wait := adminsvc.LoginLockedFor(req.Username, ip); wait > 0 {
		return dto.Fail(c, fiber.StatusTooManyRequests,
			fmt.Sprintf("Too many failed login attempts, try again in %d minutes.", int(wait.Minutes())+1))
	}

	// 拼图凭证先于口令校验：没有有效凭证就不去碰口令（拖错拼图已在 VerifyCaptcha 计过失败次数）
	if adminsvc.CaptchaEnabled() && !adminsvc.ConsumeCaptchaToken(req.CaptchaToken) {
		return dto.Fail(c, fiber.StatusBadRequest, "请先完成安全验证")
	}

	user, err := adminsvc.Authenticate(req.Username, req.Password)
	if err != nil {
		adminsvc.RecordLoginFailure(req.Username, ip)
		return dto.Fail(c, fiber.StatusForbidden, "Username or password is incorrect.")
	}
	adminsvc.RecordLoginSuccess(req.Username, ip)

	roles := adminsvc.GetRoleNames(user)
	accessToken, err := middleware.GenerateAccessToken(user.ID, user.Username, roles)
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to generate token")
	}

	refreshToken, err := middleware.GenerateRefreshToken(user.ID, user.Username)
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to generate refresh token")
	}

	c.Cookie(&fiber.Cookie{
		Name:     "jwt",
		Value:    refreshToken,
		HTTPOnly: true,
		SameSite: "None",
		Secure:   true,
		MaxAge:   int(config.C.JWT.RefreshExpire / time.Second),
	})

	return dto.Success(c, fiber.Map{
		"id":          user.ID,
		"username":    user.Username,
		"realName":    user.RealName,
		"avatar":      user.Avatar,
		"roles":       roles,
		"homePath":    resolveAccessibleHomePath(user),
		"accessToken": accessToken,
	})
}

// Logout 退出登录
// @Summary 退出登录
// @Description 清除 refresh token cookie
// @Tags 认证
// @Produce json
// @Success 200 {object} dto.Response
// @Router /admin/auth/logout [post]
func Logout(c *fiber.Ctx) error {
	clearRefreshCookie(c)
	return dto.Success(c, "")
}

// clearRefreshCookie 用与 Login 完全相同的属性覆盖删除。跨站部署时浏览器只接受
// SameSite=None; Secure 的 Set-Cookie，缺了属性的删除指令会被直接丢弃，旧 cookie 一直留着。
func clearRefreshCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     "jwt",
		Value:    "",
		HTTPOnly: true,
		SameSite: "None",
		Secure:   true,
		MaxAge:   -1,
	})
}

// RefreshToken 刷新令牌
// @Summary 刷新 Access Token
// @Description 使用 cookie 中的 refresh token 获取新的 access token
// @Tags 认证
// @Produce plain
// @Success 200 {string} string "新的 access token"
// @Failure 403 {object} dto.Response
// @Failure 500 {object} dto.Response
// @Router /admin/auth/refresh [post]
func RefreshToken(c *fiber.Ctx) error {
	refreshToken := c.Cookies("jwt")
	if refreshToken == "" {
		return dto.Fail(c, fiber.StatusForbidden, "Forbidden Exception")
	}

	// 只认 refresh 类型：access token 塞进 cookie 不能用来续签
	claims, err := middleware.ParseToken(refreshToken, middleware.TokenTypeRefresh)
	if err != nil {
		clearRefreshCookie(c)
		return dto.Fail(c, fiber.StatusForbidden, "Forbidden Exception")
	}

	user, err := adminsvc.GetUserByID(claims.UserID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		clearRefreshCookie(c)
		return dto.Fail(c, fiber.StatusForbidden, "Forbidden Exception")
	}
	if err != nil {
		// 数据库抖动不是鉴权失败：不动 cookie，让前端稍后重试
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to load user")
	}

	// 禁用（status=0）或已删除的管理员不得再用 refresh cookie 续签 access token，
	// 否则后台"禁用账号"在 access token 过期前（最长可达数天）形同虚设。
	// 改密之前签发的 refresh token 同样作废，否则"改密即下线"会被续签绕过。
	if user.Status != 1 || middleware.TokenRevokedByPasswordChange(claims.IssuedAt, user.PasswordChangedAt) {
		clearRefreshCookie(c)
		return dto.Fail(c, fiber.StatusForbidden, "Forbidden Exception")
	}

	roles := adminsvc.GetRoleNames(user)
	newToken, err := middleware.GenerateAccessToken(user.ID, user.Username, roles)
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to generate token")
	}

	return c.SendString(newToken)
}

// ChangePassword 修改当前用户密码
// @Summary 修改密码
// @Description 用户修改自己的密码，需验证旧密码；成功后此前签发的 token 全部失效，需重新登录
// @Tags 认证
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body admindto.ChangePasswordRequest true "密码信息"
// @Success 200 {object} dto.Response
// @Failure 400 {object} dto.Response
// @Failure 403 {object} dto.Response
// @Router /admin/auth/change-password [post]
func ChangePassword(c *fiber.Ctx) error {
	var req admindto.ChangePasswordRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}

	userID := c.Locals("userId").(uint)
	user, err := adminsvc.GetUserByID(userID)
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "用户不存在")
	}

	if err := adminsvc.VerifyPassword(user, req.OldPassword); err != nil {
		return dto.Fail(c, fiber.StatusForbidden, "旧密码不正确")
	}
	if err := adminsvc.CheckPasswordStrength(req.NewPassword, user.Username); err != nil {
		return dto.Fail(c, fiber.StatusBadRequest, err.Error())
	}

	if err := adminsvc.ChangePassword(userID, req.NewPassword); err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "修改密码失败")
	}
	middleware.InvalidateUserAuthCache(userID)

	return dto.Success(c, nil)
}

// GetAccessCodes 获取权限码
// @Summary 获取当前用户权限码
// @Description 返回当前登录用户拥有的所有权限码列表
// @Tags 认证
// @Produce json
// @Security BearerAuth
// @Success 200 {object} dto.Response{data=[]string}
// @Failure 401 {object} dto.Response
// @Router /admin/auth/codes [get]
func GetAccessCodes(c *fiber.Ctx) error {
	username := c.Locals("username").(string)
	user, err := adminsvc.GetUserByUsername(username)
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "User not found")
	}
	codes := adminsvc.GetAccessCodes(user)
	if codes == nil {
		codes = []string{}
	}
	return dto.Success(c, codes)
}

// GetUserInfo 获取当前用户信息
// @Summary 获取当前用户信息
// @Description 返回当前登录用户的基本信息
// @Tags 用户
// @Produce json
// @Security BearerAuth
// @Success 200 {object} dto.Response{data=admindto.UserInfoResponse}
// @Failure 401 {object} dto.Response
// @Router /admin/user/info [get]
func GetUserInfo(c *fiber.Ctx) error {
	userID := c.Locals("userId").(uint)
	user, err := adminsvc.GetUserByID(userID)
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "User not found")
	}
	roles := adminsvc.GetRoleNames(user)
	return dto.Success(c, fiber.Map{
		"id":       user.ID,
		"username": user.Username,
		"realName": user.RealName,
		"avatar":   user.Avatar,
		"roles":    roles,
		"homePath": resolveAccessibleHomePath(user),
	})
}

func resolveAccessibleHomePath(user *adminmodel.User) string {
	if adminsvc.IsSuperUser(user) {
		return user.HomePath
	}

	roleIDs := adminsvc.ActiveRoleIDs(user)
	if len(roleIDs) == 0 {
		return user.HomePath
	}

	if user.HomePath != "" {
		var count int64
		store.DB.Model(&adminmodel.Menu{}).
			Joins("JOIN role_menus ON role_menus.menu_id = sys_menus.id").
			Joins("JOIN sys_roles ON sys_roles.id = role_menus.role_id").
			Where("role_menus.role_id IN ? AND sys_roles.status = ?", roleIDs, 1).
			Where("sys_menus.path = ? AND sys_menus.type IN ? AND sys_menus.status = ?", user.HomePath, []string{"menu", "embedded", "link"}, 1).
			Count(&count)
		if count > 0 {
			return user.HomePath
		}
	}

	var menu adminmodel.Menu
	if err := store.DB.
		Joins("JOIN role_menus ON role_menus.menu_id = sys_menus.id").
		Joins("JOIN sys_roles ON sys_roles.id = role_menus.role_id").
		Where("role_menus.role_id IN ? AND sys_roles.status = ?", roleIDs, 1).
		Where("sys_menus.path <> '' AND sys_menus.type IN ? AND sys_menus.status = ?", []string{"menu", "embedded", "link"}, 1).
		Order("sys_menus.order_no ASC, sys_menus.id ASC").
		First(&menu).Error; err == nil {
		return menu.Path
	}

	return user.HomePath
}

// loginClientIP 取登录请求的真实来源。生产在 nginx 之后，直连地址恒为 127.0.0.1，
// 只有直连方是本机反代时才信 X-Real-IP（nginx 用 $remote_addr 覆盖写入，客户端伪造不了）；
// 直连服务端口的请求一律用直连地址，防止伪造请求头绕过按 IP 的登录闸。
func loginClientIP(c *fiber.Ctx) string {
	direct := c.IP()
	if ip := net.ParseIP(direct); ip != nil && ip.IsLoopback() {
		if real := net.ParseIP(strings.TrimSpace(c.Get("X-Real-IP"))); real != nil {
			return real.String()
		}
	}
	return direct
}
