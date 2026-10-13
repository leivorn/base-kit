package admin

import (
	"sort"
	"strconv"
	"strings"

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

// GetAllMenus 获取用户菜单
// @Summary 获取当前用户的菜单树
// @Description 根据当前用户角色返回可访问的菜单树(用于前端动态路由)
// @Tags 菜单
// @Produce json
// @Security BearerAuth
// @Success 200 {object} dto.Response{data=[]object}
// @Failure 401 {object} dto.Response
// @Router /admin/menu/all [get]
func GetAllMenus(c *fiber.Ctx) error {
	username := c.Locals("username").(string)
	user, err := adminsvc.GetUserByUsername(username)
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "User not found")
	}

	var menus []adminmodel.Menu

	if adminsvc.IsSuperUser(user) {
		store.DB.
			Where("type != ? AND status = ?", "button", 1).
			Order("order_no ASC").
			Find(&menus)
	} else {
		// 只按启用角色取菜单，与权限判定口径一致（禁用角色的菜单不再出现在侧栏）
		roleIDs := adminsvc.ActiveRoleIDs(user)
		if len(roleIDs) > 0 {
			store.DB.
				Joins("JOIN role_menus ON role_menus.menu_id = sys_menus.id").
				Where("role_menus.role_id IN ? AND sys_menus.type IN ? AND sys_menus.status = ?", roleIDs, []string{"menu", "embedded", "link"}, 1).
				Order("sys_menus.order_no ASC").
				Distinct().
				Find(&menus)
			menus = includeMenuAncestors(menus)
		}
	}

	tree := buildMenuTree(menus, 0)
	if tree == nil {
		tree = []fiber.Map{}
	}
	return dto.Success(c, tree)
}

// GetMenuList 获取菜单管理列表
// @Summary 获取菜单管理列表
// @Description 返回所有菜单的树形结构(用于后台菜单管理页面)
// @Tags 系统管理 - 菜单
// @Produce json
// @Security BearerAuth
// @Success 200 {object} dto.Response{data=[]object}
// @Failure 401 {object} dto.Response
// @Router /admin/system/menu/list [get]
func GetMenuList(c *fiber.Ctx) error {
	var menus []adminmodel.Menu
	store.DB.Order("order_no ASC").Find(&menus)
	tree := buildMenuTreeForManage(menus, 0)
	return dto.Success(c, tree)
}

// CreateMenu 创建菜单
// @Summary 创建菜单
// @Description 创建新的菜单/目录/按钮
// @Tags 系统管理 - 菜单
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body admindto.MenuRequest true "菜单信息"
// @Success 200 {object} dto.Response{data=dto.IDResponse}
// @Failure 400 {object} dto.Response
// @Router /admin/system/menu [post]
func CreateMenu(c *fiber.Ctx) error {
	var req admindto.MenuRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}
	if !safeMenuURL(req.Link) || !safeMenuURL(req.IframeSrc) {
		return dto.Fail(c, fiber.StatusBadRequest, msgUnsafeMenuURL)
	}
	req.Link, req.IframeSrc = strings.TrimSpace(req.Link), strings.TrimSpace(req.IframeSrc)

	if ok, err := parentExists(store.DB, &adminmodel.Menu{}, req.ParentID); err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to create menu")
	} else if !ok {
		return dto.Fail(c, fiber.StatusBadRequest, "父级菜单不存在")
	}

	menu := menuFromRequest(req)
	if err := store.DB.Create(&menu).Error; err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to create menu")
	}
	middleware.InvalidatePermissionCache()
	return dto.Success(c, fiber.Map{"id": menu.ID})
}

// UpdateMenu 更新菜单
// @Summary 更新菜单
// @Description 更新菜单信息；父级不能设为自身或其下级
// @Tags 系统管理 - 菜单
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "菜单ID"
// @Param request body admindto.MenuRequest true "菜单信息"
// @Success 200 {object} dto.Response
// @Failure 400 {object} dto.Response
// @Failure 404 {object} dto.Response
// @Router /admin/system/menu/{id} [put]
func UpdateMenu(c *fiber.Ctx) error {
	id, _ := strconv.ParseUint(c.Params("id"), 10, 64)
	var req admindto.MenuRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}
	if !safeMenuURL(req.Link) || !safeMenuURL(req.IframeSrc) {
		return dto.Fail(c, fiber.StatusBadRequest, msgUnsafeMenuURL)
	}
	req.Link, req.IframeSrc = strings.TrimSpace(req.Link), strings.TrimSpace(req.IframeSrc)

	var existing adminmodel.Menu
	if err := store.DB.First(&existing, id).Error; err != nil {
		return dto.Fail(c, fiber.StatusNotFound, "Menu not found")
	}
	// 父级指向自身或自身的后代会让整棵子树从树形结构里"消失"，但权限码仍然有效且无法再回收
	if cyclic, err := isSelfOrDescendant(store.DB, &adminmodel.Menu{}, existing.ID, req.ParentID); err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to update menu")
	} else if cyclic {
		return dto.Fail(c, fiber.StatusBadRequest, "父级不能是自身或其下级")
	}
	if ok, err := parentExists(store.DB, &adminmodel.Menu{}, req.ParentID); err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to update menu")
	} else if !ok {
		return dto.Fail(c, fiber.StatusBadRequest, "父级菜单不存在")
	}

	// 用 map[string]any 进行更新：
	// 1) 显式列出所有列名，把 bool 字段的 false 也写入（结构体 Updates 会忽略零值）
	// 2) 装饰类 meta 字段（hideInMenu 等）取消勾选时需要能落库
	// 3) order_no 用指针判断：仅在请求显式提交了 order 字段时才更新，
	//    防止前端漏带导致排序被重置为 0
	updates := map[string]any{
		"parent_id":             req.ParentID,
		"name":                  req.Name,
		"path":                  req.Path,
		"component":             req.Component,
		"redirect":              req.Redirect,
		"type":                  req.Type,
		"icon":                  req.Icon,
		"title":                 req.Title,
		"auth_code":             req.AuthCode,
		"status":                req.Status,
		"keep_alive":            req.KeepAlive,
		"affix_tab":             req.AffixTab,
		"iframe_src":            req.IframeSrc,
		"link":                  req.Link,
		"active_icon":           req.ActiveIcon,
		"active_path":           req.ActivePath,
		"hide_in_menu":          req.HideInMenu,
		"hide_in_breadcrumb":    req.HideInBreadcrumb,
		"hide_in_tab":           req.HideInTab,
		"hide_children_in_menu": req.HideChildrenInMenu,
		"badge_type":            req.BadgeType,
		"badge":                 req.Badge,
		"badge_variants":        req.BadgeVariants,
	}
	if req.OrderNo != nil {
		updates["order_no"] = *req.OrderNo
	}
	if err := store.DB.Model(&adminmodel.Menu{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to update menu")
	}
	middleware.InvalidatePermissionCache()
	return dto.Success(c, nil)
}

// menuFromRequest 把 MenuRequest 转换为 Menu model，供 Create 使用。
// Update 走 map[string]any，否则 bool 字段的 false 会被 GORM 当作零值忽略，
// 用户取消勾选 hideInMenu 等装饰字段时无法落库。
func menuFromRequest(req admindto.MenuRequest) adminmodel.Menu {
	order := 0
	if req.OrderNo != nil {
		order = *req.OrderNo
	}
	return adminmodel.Menu{
		ParentID:           req.ParentID,
		Name:               req.Name,
		Path:               req.Path,
		Component:          req.Component,
		Redirect:           req.Redirect,
		Type:               req.Type,
		Icon:               req.Icon,
		Title:              req.Title,
		AuthCode:           req.AuthCode,
		OrderNo:            order,
		Status:             req.Status,
		KeepAlive:          req.KeepAlive,
		AffixTab:           req.AffixTab,
		IframeSrc:          req.IframeSrc,
		Link:               req.Link,
		ActiveIcon:         req.ActiveIcon,
		ActivePath:         req.ActivePath,
		HideInMenu:         req.HideInMenu,
		HideInBreadcrumb:   req.HideInBreadcrumb,
		HideInTab:          req.HideInTab,
		HideChildrenInMenu: req.HideChildrenInMenu,
		BadgeType:          req.BadgeType,
		Badge:              req.Badge,
		BadgeVariants:      req.BadgeVariants,
	}
}

// DeleteMenu 删除菜单
// @Summary 删除菜单
// @Description 删除指定菜单及其全部下级（含按钮），并清理角色关联，权限码即时失效
// @Tags 系统管理 - 菜单
// @Produce json
// @Security BearerAuth
// @Param id path int true "菜单ID"
// @Success 200 {object} dto.Response
// @Failure 404 {object} dto.Response
// @Router /admin/system/menu/{id} [delete]
func DeleteMenu(c *fiber.Ctx) error {
	id, _ := strconv.ParseUint(c.Params("id"), 10, 64)
	var menu adminmodel.Menu
	if err := store.DB.First(&menu, id).Error; err != nil {
		return dto.Fail(c, fiber.StatusNotFound, "Menu not found")
	}

	// 只删一层会留下孤儿按钮：树上看不见，auth_code 却仍然有效且无法回收。
	// 后代在事务内收集，避免收集与删除之间新插入的子节点漏删。
	err := store.DB.Transaction(func(tx *gorm.DB) error {
		descendants, err := collectDescendantIDs(tx, &adminmodel.Menu{}, menu.ID)
		if err != nil {
			return err
		}
		ids := append(descendants, menu.ID)
		if err := tx.Where("menu_id IN ?", ids).Delete(&adminmodel.RoleMenu{}).Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", ids).Delete(&adminmodel.Menu{}).Error
	})
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to delete menu")
	}
	middleware.InvalidatePermissionCache()
	return dto.Success(c, nil)
}

// CheckMenuNameExists 检查菜单名称是否存在
// @Summary 检查菜单名称是否存在
// @Description 编辑模式下传 id 排除当前记录自身，避免编辑时校验把自己也当作冲突。
// @Tags 系统管理 - 菜单
// @Produce json
// @Security BearerAuth
// @Param name query string true "菜单名称"
// @Param id query int false "当前编辑的菜单ID（编辑模式必传，新增时省略）"
// @Success 200 {object} dto.Response{data=bool}
// @Router /admin/system/menu/name-exists [get]
func CheckMenuNameExists(c *fiber.Ctx) error {
	name := c.Query("name")
	query := store.DB.Model(&adminmodel.Menu{}).Where("name = ?", name)
	// 编辑模式：排除自身，避免误报"已存在"。id 非法或为 0 时忽略。
	if rawID := c.Query("id"); rawID != "" {
		if id, err := strconv.ParseUint(rawID, 10, 64); err == nil && id > 0 {
			query = query.Where("id <> ?", id)
		}
	}
	var count int64
	query.Count(&count)
	return dto.Success(c, count > 0)
}

// CheckMenuPathExists 检查菜单路径是否存在
// @Summary 检查菜单路径是否存在
// @Description 编辑模式下传 id 排除当前记录自身，避免编辑时校验把自己也当作冲突。
// @Tags 系统管理 - 菜单
// @Produce json
// @Security BearerAuth
// @Param path query string true "路由路径"
// @Param id query int false "当前编辑的菜单ID（编辑模式必传，新增时省略）"
// @Success 200 {object} dto.Response{data=bool}
// @Router /admin/system/menu/path-exists [get]
func CheckMenuPathExists(c *fiber.Ctx) error {
	path := c.Query("path")
	query := store.DB.Model(&adminmodel.Menu{}).Where("path = ?", path)
	if rawID := c.Query("id"); rawID != "" {
		if id, err := strconv.ParseUint(rawID, 10, 64); err == nil && id > 0 {
			query = query.Where("id <> ?", id)
		}
	}
	var count int64
	query.Count(&count)
	return dto.Success(c, count > 0)
}

// menuMeta 构造前端 Vben Admin 期望的 meta 对象。
// 所有装饰类字段都收敛在这里，避免 list/manage 两条返回路径分叉。
// 空字符串/默认 false 字段也带上，便于前端在编辑时直接 setValues 填回。
func menuMeta(m adminmodel.Menu) fiber.Map {
	meta := fiber.Map{
		"title":              m.Title,
		"icon":               m.Icon,
		"order":              m.OrderNo,
		"affixTab":           m.AffixTab,
		"keepAlive":          m.KeepAlive,
		"hideInMenu":         m.HideInMenu,
		"hideInBreadcrumb":   m.HideInBreadcrumb,
		"hideInTab":          m.HideInTab,
		"hideChildrenInMenu": m.HideChildrenInMenu,
	}
	if m.ActiveIcon != "" {
		meta["activeIcon"] = m.ActiveIcon
	}
	if m.ActivePath != "" {
		meta["activePath"] = m.ActivePath
	}
	if m.BadgeType != "" {
		meta["badgeType"] = m.BadgeType
	}
	if m.Badge != "" {
		meta["badge"] = m.Badge
	}
	if m.BadgeVariants != "" {
		meta["badgeVariants"] = m.BadgeVariants
	}
	if m.IframeSrc != "" {
		meta["iframeSrc"] = m.IframeSrc
	}
	if m.Link != "" {
		meta["link"] = m.Link
	}
	return meta
}

func buildMenuTree(menus []adminmodel.Menu, parentID uint) []fiber.Map {
	var tree []fiber.Map
	for _, m := range menus {
		if m.ParentID == parentID {
			node := fiber.Map{
				"name": m.Name,
				"path": m.Path,
				"meta": runtimeMenuMeta(m),
			}
			if m.Component != "" {
				node["component"] = m.Component
			}
			if m.Redirect != "" {
				node["redirect"] = m.Redirect
			}
			children := buildMenuTree(menus, m.ID)
			if len(children) > 0 {
				node["children"] = children
			}
			tree = append(tree, node)
		}
	}
	return tree
}

func includeMenuAncestors(menus []adminmodel.Menu) []adminmodel.Menu {
	if len(menus) == 0 {
		return menus
	}

	var allMenus []adminmodel.Menu
	store.DB.Where("type != ? AND status = ?", "button", 1).Find(&allMenus)

	allByID := make(map[uint]adminmodel.Menu, len(allMenus))
	for _, menu := range allMenus {
		allByID[menu.ID] = menu
	}

	selectedByID := make(map[uint]adminmodel.Menu, len(menus))
	for _, menu := range menus {
		selectedByID[menu.ID] = menu
	}

	for _, menu := range menus {
		visited := map[uint]bool{}
		for parentID := menu.ParentID; parentID != 0 && !visited[parentID]; {
			visited[parentID] = true
			parent, ok := allByID[parentID]
			if !ok {
				break
			}
			if _, exists := selectedByID[parent.ID]; !exists {
				selectedByID[parent.ID] = parent
			}
			parentID = parent.ParentID
		}
	}

	result := make([]adminmodel.Menu, 0, len(selectedByID))
	for _, menu := range selectedByID {
		result = append(result, menu)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].OrderNo == result[j].OrderNo {
			return result[i].ID < result[j].ID
		}
		return result[i].OrderNo < result[j].OrderNo
	})
	return result
}

func buildMenuTreeForManage(menus []adminmodel.Menu, parentID uint) []fiber.Map {
	var tree []fiber.Map
	for _, m := range menus {
		if m.ParentID == parentID {
			node := fiber.Map{
				"id":     m.ID,
				"pid":    m.ParentID,
				"name":   m.Name,
				"path":   m.Path,
				"status": m.Status,
				"type":   m.Type,
				"meta":   menuMeta(m),
			}
			if m.Component != "" {
				node["component"] = m.Component
			}
			if m.AuthCode != "" {
				node["authCode"] = m.AuthCode
			}
			children := buildMenuTreeForManage(menus, m.ID)
			if len(children) > 0 {
				node["children"] = children
			}
			tree = append(tree, node)
		}
	}
	return tree
}
