package admin

import (
	"errors"
	"strconv"

	"github.com/leivorn/base-kit/dto"
	admindto "github.com/leivorn/base-kit/dto/admin"
	"github.com/leivorn/base-kit/middleware"
	adminsvc "github.com/leivorn/base-kit/service/admin"
	"github.com/leivorn/base-kit/store"
	"github.com/leivorn/base-kit/validator"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// GetUserList 获取用户列表
// @Summary 获取用户列表
// @Description 分页查询用户列表，支持按用户名和状态筛选
// @Tags 系统管理 - 用户
// @Produce json
// @Security BearerAuth
// @Param page query int false "页码" default(1)
// @Param pageSize query int false "每页数量，最大 200" default(20)
// @Param username query string false "用户名(模糊搜索)"
// @Param status query int false "状态: 0=禁用 1=启用"
// @Success 200 {object} dto.Response{data=dto.PageData{items=[]admindto.UserItem}}
// @Failure 401 {object} dto.Response
// @Router /admin/system/user/list [get]
func GetUserList(c *fiber.Ctx) error {
	page, pageSize := dto.ParsePage(c)

	params := adminsvc.UserListParams{
		Page:     page,
		PageSize: pageSize,
		Username: c.Query("username"),
	}

	if s := c.Query("status"); s != "" {
		v, _ := strconv.Atoi(s)
		params.Status = &v
	}

	users, total, err := adminsvc.GetUserList(params)
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to get users")
	}

	items := make([]admindto.UserItem, len(users))
	for i, u := range users {
		roles := make([]string, len(u.Roles))
		roleIDs := make([]uint, len(u.Roles))
		for j, r := range u.Roles {
			roles[j] = r.Name
			roleIDs[j] = r.ID
		}
		items[i] = admindto.UserItem{
			ID:       u.ID,
			Username: u.Username,
			RealName: u.RealName,
			Email:    u.Email,
			Phone:    u.Phone,
			Status:   u.Status,
			Roles:    roles,
			RoleIDs:  roleIDs,
			Remark:   u.Remark,
		}
	}
	return dto.PageSuccess(c, items, total)
}

// CreateUser 创建用户
// @Summary 创建用户
// @Description 创建新用户并分配角色
// @Tags 系统管理 - 用户
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body admindto.CreateUserRequest true "用户信息"
// @Success 200 {object} dto.Response{data=dto.IDResponse}
// @Failure 400 {object} dto.Response
// @Failure 403 {object} dto.Response
// @Router /admin/system/user [post]
func CreateUser(c *fiber.Ctx) error {
	var req admindto.CreateUserRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}

	if adminsvc.RolesIncludeSuperCode(req.RoleIDs) && !middleware.OperatorIsSuper(c) {
		return dto.Fail(c, fiber.StatusForbidden, "无权分配超级管理员角色")
	}

	user := adminsvc.NewUser(req.Username, req.Password, req.RealName, req.Email, req.Phone, req.Status, req.Remark)
	if err := adminsvc.CreateUser(user, req.RoleIDs); err != nil {
		var policyErr *adminsvc.PasswordPolicyError
		if errors.As(err, &policyErr) {
			return dto.Fail(c, fiber.StatusBadRequest, policyErr.Error())
		}
		if store.IsUniqueViolation(err) {
			return dto.Fail(c, fiber.StatusBadRequest, "用户名已存在")
		}
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to create user")
	}
	return dto.Success(c, fiber.Map{"id": user.ID})
}

// mayMutateUser 普通管理员不得修改/删除持有 super 角色的用户：
// 否则可以给对方重置密码后登录，或剥掉其 super 角色，等于绕过了"不能分配 super"的防线。
func mayMutateUser(c *fiber.Ctx, targetID uint) bool {
	return !adminsvc.UserHasSuperRole(targetID) || middleware.OperatorIsSuper(c)
}

const msgSuperHolderProtected = "无权修改超级管理员"

func failUserMutation(c *fiber.Ctx, err error, fallback string) error {
	var policyErr *adminsvc.PasswordPolicyError
	switch {
	case errors.As(err, &policyErr):
		return dto.Fail(c, fiber.StatusBadRequest, policyErr.Error())
	case errors.Is(err, adminsvc.ErrSuperAdminProtected):
		return dto.Fail(c, fiber.StatusForbidden, err.Error())
	case errors.Is(err, gorm.ErrRecordNotFound):
		return dto.Fail(c, fiber.StatusNotFound, "User not found")
	case store.IsUniqueViolation(err):
		return dto.Fail(c, fiber.StatusBadRequest, "用户名已存在")
	default:
		return dto.Fail(c, fiber.StatusInternalServerError, fallback)
	}
}

// UpdateUser 更新用户
// @Summary 更新用户
// @Description 更新用户信息和角色分配；重置密码会让该用户已签发的 token 全部失效
// @Tags 系统管理 - 用户
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "用户ID"
// @Param request body admindto.UpdateUserRequest true "用户信息"
// @Success 200 {object} dto.Response
// @Failure 400 {object} dto.Response
// @Failure 403 {object} dto.Response
// @Failure 404 {object} dto.Response
// @Router /admin/system/user/{id} [put]
func UpdateUser(c *fiber.Ctx) error {
	id, _ := strconv.ParseUint(c.Params("id"), 10, 64)

	var req admindto.UpdateUserRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}

	if adminsvc.RolesIncludeSuperCode(req.RoleIDs) && !middleware.OperatorIsSuper(c) {
		return dto.Fail(c, fiber.StatusForbidden, "无权分配超级管理员角色")
	}
	if !mayMutateUser(c, uint(id)) {
		return dto.Fail(c, fiber.StatusForbidden, msgSuperHolderProtected)
	}

	updates := map[string]any{
		"real_name": req.RealName,
		"email":     req.Email,
		"phone":     req.Phone,
		"status":    req.Status,
		"remark":    req.Remark,
	}
	if req.Password != "" {
		updates["password"] = req.Password
	}

	if err := adminsvc.UpdateUser(uint(id), updates, req.RoleIDs); err != nil {
		return failUserMutation(c, err, "Failed to update user")
	}
	// 状态/角色/口令变更即时生效，不等缓存 TTL
	middleware.InvalidateUserAuthCache(uint(id))
	return dto.Success(c, nil)
}

// DeleteUser 删除用户
// @Summary 删除用户
// @Description 物理删除指定用户及其角色关联，删除后同名用户可再次创建
// @Tags 系统管理 - 用户
// @Produce json
// @Security BearerAuth
// @Param id path int true "用户ID"
// @Success 200 {object} dto.Response
// @Failure 403 {object} dto.Response
// @Failure 404 {object} dto.Response
// @Router /admin/system/user/{id} [delete]
func DeleteUser(c *fiber.Ctx) error {
	id, _ := strconv.ParseUint(c.Params("id"), 10, 64)
	if !mayMutateUser(c, uint(id)) {
		return dto.Fail(c, fiber.StatusForbidden, msgSuperHolderProtected)
	}
	if err := adminsvc.DeleteUser(uint(id)); err != nil {
		return failUserMutation(c, err, "Failed to delete user")
	}
	middleware.InvalidateUserAuthCache(uint(id))
	return dto.Success(c, nil)
}
