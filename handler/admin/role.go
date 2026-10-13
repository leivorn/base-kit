package admin

import (
	"strconv"
	"strings"
	"time"

	"github.com/leivorn/base-kit/dto"
	admindto "github.com/leivorn/base-kit/dto/admin"
	"github.com/leivorn/base-kit/middleware"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"
	"github.com/leivorn/base-kit/validator"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// GetRoleMenuTree 获取角色授权用的完整菜单树
// @Summary 获取角色授权菜单树
// @Description 返回所有菜单（含按钮）的树形结构，专供"编辑/新增角色"的权限抽屉使用。
// @Description 该接口与系统菜单管理接口解耦：拥有角色管理权限的用户即使没有"菜单管理"权限，
// @Description 也可以正常获取菜单树用于角色授权操作。
// @Tags 系统管理 - 角色
// @Produce json
// @Security BearerAuth
// @Success 200 {object} dto.Response{data=[]object}
// @Failure 401 {object} dto.Response
// @Router /admin/system/role/menu-tree [get]
func GetRoleMenuTree(c *fiber.Ctx) error {
	var menus []adminmodel.Menu
	store.DB.Order("order_no ASC").Find(&menus)
	tree := buildMenuTreeForManage(menus, 0)
	return dto.Success(c, tree)
}

// GetAllRoles 获取全部角色（简单列表，不分页）
// @Summary 获取全部角色
// @Tags 系统管理 - 角色
// @Produce json
// @Security BearerAuth
// @Success 200 {object} dto.Response{data=[]admindto.RoleItem}
// @Router /admin/system/role/all [get]
func GetAllRoles(c *fiber.Ctx) error {
	var roles []adminmodel.Role
	store.DB.Where("status = ?", 1).Order("id").Find(&roles)

	items := make([]admindto.RoleItem, len(roles))
	for i, r := range roles {
		items[i] = admindto.RoleItem{
			ID:   r.ID,
			Name: r.Name,
			Code: r.Code,
		}
	}
	return dto.Success(c, items)
}

// timeBound 解析列表筛选用的时间字符串。前端 RangePicker 经 fieldMappingTime 默认送
// YYYY-MM-DD，也接受 YYYY-MM-DD HH:mm:ss。exclusiveEnd 为 true 时返回开区间上界：
// 日期 → 次日 0 点，日期时间 → +1 秒。解析失败视为未传。
func timeBound(s string, exclusiveEnd bool) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local); err == nil {
		if exclusiveEnd {
			t = t.Add(time.Second)
		}
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		if exclusiveEnd {
			t = t.AddDate(0, 0, 1)
		}
		return t, true
	}
	return time.Time{}, false
}

// GetRoleList 获取角色列表
// @Summary 获取角色列表
// @Description 分页查询角色列表，支持按名称、编码、状态和创建时间筛选
// @Tags 系统管理 - 角色
// @Produce json
// @Security BearerAuth
// @Param page query int false "页码" default(1)
// @Param pageSize query int false "每页数量，最大 200" default(20)
// @Param name query string false "角色名(模糊搜索)"
// @Param code query string false "角色编码(模糊搜索)"
// @Param status query string false "状态: 0=禁用 1=启用"
// @Param startTime query string false "创建时间起（含），YYYY-MM-DD 或 YYYY-MM-DD HH:mm:ss"
// @Param endTime query string false "创建时间止（含），YYYY-MM-DD 或 YYYY-MM-DD HH:mm:ss"
// @Success 200 {object} dto.Response{data=dto.PageData{items=[]admindto.RoleItem}}
// @Failure 401 {object} dto.Response
// @Router /admin/system/role/list [get]
func GetRoleList(c *fiber.Ctx) error {
	page, pageSize := dto.ParsePage(c)
	name := c.Query("name")
	code := c.Query("code")
	status := c.Query("status")

	var roles []adminmodel.Role
	var total int64
	query := store.DB.Model(&adminmodel.Role{})

	if name != "" {
		query = query.Where("name LIKE ?", "%"+name+"%")
	}
	if code != "" {
		query = query.Where("code LIKE ?", "%"+code+"%")
	}
	if status == "0" || status == "1" {
		query = query.Where("status = ?", status)
	}
	if t, ok := timeBound(c.Query("startTime"), false); ok {
		query = query.Where("created_at >= ?", t)
	}
	if t, ok := timeBound(c.Query("endTime"), true); ok {
		query = query.Where("created_at < ?", t)
	}

	if err := query.Count(&total).Error; err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to get roles")
	}
	offset := (page - 1) * pageSize
	if err := query.Preload("Menus").Order("id").Offset(offset).Limit(pageSize).Find(&roles).Error; err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to get roles")
	}

	items := make([]admindto.RoleItem, len(roles))
	for i, r := range roles {
		menuIDs := make([]uint, len(r.Menus))
		for j, m := range r.Menus {
			menuIDs[j] = m.ID
		}
		items[i] = admindto.RoleItem{
			ID:          r.ID,
			Name:        r.Name,
			Code:        r.Code,
			Status:      r.Status,
			Remark:      r.Remark,
			Permissions: menuIDs,
			CreateTime:  r.CreatedAt.Format("2006/01/02 15:04:05"),
		}
	}

	return dto.PageSuccess(c, items, total)
}

// CreateRole 创建角色
// @Summary 创建角色
// @Description 创建新角色并分配菜单权限
// @Tags 系统管理 - 角色
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body admindto.CreateRoleRequest true "角色信息"
// @Success 200 {object} dto.Response{data=dto.IDResponse}
// @Failure 400 {object} dto.Response
// @Router /admin/system/role [post]
func CreateRole(c *fiber.Ctx) error {
	var req admindto.CreateRoleRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}

	// code=super 是权限体系里的最高特权标识（PermissionAuth 见到该角色码直接全量放行），
	// 属系统保留字：只允许由种子数据创建，禁止通过接口新建，否则等于开放"自助提权"入口。
	if req.Code == "super" {
		return dto.Fail(c, fiber.StatusBadRequest, "角色 code \"super\" 为系统保留，不可创建")
	}

	role := adminmodel.Role{
		Name:   req.Name,
		Code:   req.Code,
		Status: req.Status,
		Remark: req.Remark,
	}
	if err := store.DB.Create(&role).Error; err != nil {
		if store.IsUniqueViolation(err) {
			return dto.Fail(c, fiber.StatusBadRequest, "角色名称或编码已存在")
		}
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to create role")
	}

	menuIDs := req.GrantedMenuIDs()
	if len(menuIDs) > 0 {
		var menus []adminmodel.Menu
		store.DB.Where("id IN ?", menuIDs).Find(&menus)
		if err := store.DB.Model(&role).Association("Menus").Replace(menus); err != nil {
			return dto.Fail(c, fiber.StatusInternalServerError, "Failed to grant menus")
		}
	}

	middleware.InvalidatePermissionCache()
	return dto.Success(c, fiber.Map{"id": role.ID})
}

// UpdateRole 更新角色
// @Summary 更新角色
// @Description 更新角色信息和菜单权限
// @Tags 系统管理 - 角色
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "角色ID"
// @Param request body admindto.CreateRoleRequest true "角色信息"
// @Success 200 {object} dto.Response
// @Failure 400 {object} dto.Response
// @Failure 404 {object} dto.Response
// @Router /admin/system/role/{id} [put]
func UpdateRole(c *fiber.Ctx) error {
	id, _ := strconv.ParseUint(c.Params("id"), 10, 64)
	var req admindto.CreateRoleRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}

	// super 角色受保护：不允许通过普通更新接口修改其菜单关联或停用，
	// 防止"超级管理员"角色被误操作清空权限或禁用。
	var existing adminmodel.Role
	if err := store.DB.First(&existing, id).Error; err != nil {
		return dto.Fail(c, fiber.StatusNotFound, "Role not found")
	}
	if existing.Code == "super" {
		return dto.Fail(c, fiber.StatusForbidden, "超级管理员角色受系统保护，不允许修改")
	}

	// 禁止把任意其它角色改名成保留字 code=super（否则可绕过上面"现有 super 不可改"的保护，
	// 把一个普通角色升格为最高特权角色，再分配给自己完成提权）。
	if req.Code == "super" {
		return dto.Fail(c, fiber.StatusBadRequest, "角色 code \"super\" 为系统保留，不可使用")
	}

	updates := map[string]any{
		"name":   req.Name,
		"code":   req.Code,
		"status": req.Status,
		"remark": req.Remark,
	}
	if err := store.DB.Model(&adminmodel.Role{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		if store.IsUniqueViolation(err) {
			return dto.Fail(c, fiber.StatusBadRequest, "角色名称或编码已存在")
		}
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to update role")
	}

	// 仅当请求显式带上 menuIds/permissions 字段时才更新菜单关联。
	// 字段缺失（指针 nil）时保持原有关联不变，避免"仅改状态/备注"等场景误清空权限。
	if req.HasGrantedMenuIDs() {
		var menus []adminmodel.Menu
		if menuIDs := req.GrantedMenuIDs(); len(menuIDs) > 0 {
			store.DB.Where("id IN ?", menuIDs).Find(&menus)
		}
		if err := store.DB.Model(&existing).Association("Menus").Replace(menus); err != nil {
			return dto.Fail(c, fiber.StatusInternalServerError, "Failed to update role menus")
		}
	}

	middleware.InvalidatePermissionCache()
	// 只有 code/状态变了才影响持有者的鉴权上下文，且只清这些用户，避免一改角色就让所有人缓存全失效
	if existing.Code != req.Code || existing.Status != req.Status {
		invalidateRoleHolders(existing.ID)
	}
	return dto.Success(c, nil)
}

// invalidateRoleHolders 精确失效持有该角色的用户的鉴权缓存。
func invalidateRoleHolders(roleID uint) {
	var userIDs []uint
	store.DB.Model(&adminmodel.UserRole{}).Where("role_id = ?", roleID).Pluck("user_id", &userIDs)
	for _, id := range userIDs {
		middleware.InvalidateUserAuthCache(id)
	}
}

// DeleteRole 删除角色
// @Summary 删除角色
// @Description 物理删除指定角色及其菜单、用户关联
// @Tags 系统管理 - 角色
// @Produce json
// @Security BearerAuth
// @Param id path int true "角色ID"
// @Success 200 {object} dto.Response
// @Failure 404 {object} dto.Response
// @Router /admin/system/role/{id} [delete]
func DeleteRole(c *fiber.Ctx) error {
	id, _ := strconv.ParseUint(c.Params("id"), 10, 64)
	var role adminmodel.Role
	if err := store.DB.First(&role, id).Error; err != nil {
		return dto.Fail(c, fiber.StatusNotFound, "Role not found")
	}
	if role.Code == "super" {
		return dto.Fail(c, fiber.StatusForbidden, "超级管理员角色受系统保护，不允许删除")
	}

	// 持有者要在关联行删除前取到，删完再精确失效他们的鉴权缓存
	var holders []uint
	store.DB.Model(&adminmodel.UserRole{}).Where("role_id = ?", role.ID).Pluck("user_id", &holders)

	// 物理删除：name/code 带唯一索引，软删会让同名角色再也建不出来
	err := store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", role.ID).Delete(&adminmodel.RoleMenu{}).Error; err != nil {
			return err
		}
		if err := tx.Where("role_id = ?", role.ID).Delete(&adminmodel.UserRole{}).Error; err != nil {
			return err
		}
		return tx.Unscoped().Delete(&adminmodel.Role{}, role.ID).Error
	})
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to delete role")
	}

	middleware.InvalidatePermissionCache()
	for _, id := range holders {
		middleware.InvalidateUserAuthCache(id)
	}
	return dto.Success(c, nil)
}
