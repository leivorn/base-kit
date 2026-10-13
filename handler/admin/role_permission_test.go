package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"
	"github.com/leivorn/base-kit/validator"

	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

func setupRolePermissionTestDB(t *testing.T) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.User{}, &adminmodel.Role{}, &adminmodel.Menu{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}

	store.DB = db
	validator.Init()
}

func TestUpdateRoleAcceptsPermissionsPayload(t *testing.T) {
	setupRolePermissionTestDB(t)

	initialMenu := adminmodel.Menu{Name: "InitialMenu", Type: "menu", Title: "initial", Status: 1}
	targetMenu := adminmodel.Menu{Name: "TargetMenu", Type: "menu", Title: "target", Status: 1}
	if err := store.DB.Create(&initialMenu).Error; err != nil {
		t.Fatalf("create initial menu: %v", err)
	}
	if err := store.DB.Create(&targetMenu).Error; err != nil {
		t.Fatalf("create target menu: %v", err)
	}

	role := adminmodel.Role{Name: "Auditor", Code: "auditor", Status: 1}
	if err := store.DB.Create(&role).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := store.DB.Model(&role).Association("Menus").Replace([]adminmodel.Menu{initialMenu}); err != nil {
		t.Fatalf("seed role menus: %v", err)
	}

	body, err := json.Marshal(fiber.Map{
		"name":        "Auditor",
		"code":        "auditor",
		"status":      1,
		"permissions": []uint{targetMenu.ID},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	app := fiber.New()
	app.Put("/role/:id", UpdateRole)
	req, err := http.NewRequest(http.MethodPut, "/role/1", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("update role request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var updated adminmodel.Role
	if err := store.DB.Preload("Menus").First(&updated, role.ID).Error; err != nil {
		t.Fatalf("load updated role: %v", err)
	}
	if len(updated.Menus) != 1 || updated.Menus[0].ID != targetMenu.ID {
		t.Fatalf("expected role menus [%d], got %#v", targetMenu.ID, updated.Menus)
	}
}

// 角色管理专用菜单树接口必须返回完整树（含 button），且不依赖菜单管理权限。
func TestGetRoleMenuTreeReturnsFullTreeWithButtons(t *testing.T) {
	setupRolePermissionTestDB(t)

	parent := adminmodel.Menu{Name: "System", Type: "catalog", Title: "system.title", Status: 1, OrderNo: 1}
	if err := store.DB.Create(&parent).Error; err != nil {
		t.Fatalf("create catalog: %v", err)
	}
	listMenu := adminmodel.Menu{Name: "SystemRole", ParentID: parent.ID, Type: "menu", Title: "system.role.title", AuthCode: "System:Role:List", Status: 1, OrderNo: 1}
	if err := store.DB.Create(&listMenu).Error; err != nil {
		t.Fatalf("create role menu: %v", err)
	}
	editBtn := adminmodel.Menu{Name: "SystemRoleEdit", ParentID: listMenu.ID, Type: "button", Title: "common.edit", AuthCode: "System:Role:Edit", Status: 1, OrderNo: 2}
	if err := store.DB.Create(&editBtn).Error; err != nil {
		t.Fatalf("create edit button: %v", err)
	}

	app := fiber.New()
	app.Get("/role/menu-tree", GetRoleMenuTree)
	req, err := http.NewRequest(http.MethodGet, "/role/menu-tree", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("get tree request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var parsed struct {
		Data []struct {
			Name     string `json:"name"`
			AuthCode string `json:"authCode"`
			Children []struct {
				Name     string `json:"name"`
				AuthCode string `json:"authCode"`
				Type     string `json:"type"`
				Children []struct {
					Name     string `json:"name"`
					AuthCode string `json:"authCode"`
					Type     string `json:"type"`
				} `json:"children"`
			} `json:"children"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(parsed.Data) != 1 || parsed.Data[0].Name != "System" {
		t.Fatalf("expected System catalog at root, got %#v", parsed.Data)
	}
	if len(parsed.Data[0].Children) != 1 || parsed.Data[0].Children[0].Name != "SystemRole" {
		t.Fatalf("expected SystemRole under catalog, got %#v", parsed.Data[0].Children)
	}
	btns := parsed.Data[0].Children[0].Children
	if len(btns) != 1 || btns[0].Name != "SystemRoleEdit" || btns[0].Type != "button" {
		t.Fatalf("expected SystemRoleEdit button under SystemRole, got %#v", btns)
	}
}

// 状态切换等场景下，前端不应传 permissions 字段；后端必须保留原有菜单关联，
// 避免把整个角色的菜单权限清空。
func TestUpdateRoleWithoutPermissionsFieldKeepsExistingMenus(t *testing.T) {
	setupRolePermissionTestDB(t)

	keepMenu := adminmodel.Menu{Name: "KeepMenu", Type: "menu", Title: "keep", Status: 1}
	if err := store.DB.Create(&keepMenu).Error; err != nil {
		t.Fatalf("create keep menu: %v", err)
	}

	role := adminmodel.Role{Name: "Editor", Code: "editor", Status: 1}
	if err := store.DB.Create(&role).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := store.DB.Model(&role).Association("Menus").Replace([]adminmodel.Menu{keepMenu}); err != nil {
		t.Fatalf("seed role menus: %v", err)
	}

	// 仅切换状态/备注，不携带 permissions 字段
	body, err := json.Marshal(fiber.Map{
		"name":   "Editor",
		"code":   "editor",
		"status": 0,
		"remark": "disabled temporarily",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	app := fiber.New()
	app.Put("/role/:id", UpdateRole)
	req, err := http.NewRequest(http.MethodPut, "/role/1", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("update role request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var updated adminmodel.Role
	if err := store.DB.Preload("Menus").First(&updated, role.ID).Error; err != nil {
		t.Fatalf("load updated role: %v", err)
	}
	if len(updated.Menus) != 1 || updated.Menus[0].ID != keepMenu.ID {
		t.Fatalf("expected role menus to be preserved [%d], got %#v", keepMenu.ID, updated.Menus)
	}
	if updated.Status != 0 {
		t.Fatalf("expected status to be updated to 0, got %d", updated.Status)
	}
}

// 显式提交空数组 [] 时仍然按用户意图清空，确保"清空所有权限"的能力没有被误改。
func TestUpdateRoleExplicitEmptyPermissionsClearsMenus(t *testing.T) {
	setupRolePermissionTestDB(t)

	menu := adminmodel.Menu{Name: "AnyMenu", Type: "menu", Title: "any", Status: 1}
	if err := store.DB.Create(&menu).Error; err != nil {
		t.Fatalf("create menu: %v", err)
	}

	role := adminmodel.Role{Name: "Editor", Code: "editor", Status: 1}
	if err := store.DB.Create(&role).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := store.DB.Model(&role).Association("Menus").Replace([]adminmodel.Menu{menu}); err != nil {
		t.Fatalf("seed role menus: %v", err)
	}

	body, err := json.Marshal(fiber.Map{
		"name":        "Editor",
		"code":        "editor",
		"status":      1,
		"permissions": []uint{},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	app := fiber.New()
	app.Put("/role/:id", UpdateRole)
	req, err := http.NewRequest(http.MethodPut, "/role/1", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("update role request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var updated adminmodel.Role
	if err := store.DB.Preload("Menus").First(&updated, role.ID).Error; err != nil {
		t.Fatalf("load updated role: %v", err)
	}
	if len(updated.Menus) != 0 {
		t.Fatalf("expected role menus cleared, got %#v", updated.Menus)
	}
}

// super 角色受系统保护，禁止通过 PUT 接口被任意修改。
func TestUpdateRoleRejectsSuperRoleModification(t *testing.T) {
	setupRolePermissionTestDB(t)

	role := adminmodel.Role{Name: "Super", Code: "super", Status: 1}
	if err := store.DB.Create(&role).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}

	body, err := json.Marshal(fiber.Map{
		"name":        "Super",
		"code":        "super",
		"status":      0,
		"permissions": []uint{},
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	app := fiber.New()
	app.Put("/role/:id", UpdateRole)
	req, err := http.NewRequest(http.MethodPut, "/role/1", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("update role request: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d", resp.StatusCode)
	}

	var unchanged adminmodel.Role
	if err := store.DB.First(&unchanged, role.ID).Error; err != nil {
		t.Fatalf("load role: %v", err)
	}
	if unchanged.Status != 1 {
		t.Fatalf("expected super role status to remain 1, got %d", unchanged.Status)
	}
}

func TestGetAllMenusIncludesAncestorsForGrantedChildMenu(t *testing.T) {
	setupRolePermissionTestDB(t)

	parent := adminmodel.Menu{Name: "System", Path: "/system", Type: "catalog", Title: "system.title", Status: 1}
	if err := store.DB.Create(&parent).Error; err != nil {
		t.Fatalf("create parent menu: %v", err)
	}
	child := adminmodel.Menu{Name: "SystemUser", ParentID: parent.ID, Path: "/system/user", Component: "/system/user/list", Type: "menu", Title: "system.user.title", Status: 1}
	if err := store.DB.Create(&child).Error; err != nil {
		t.Fatalf("create child menu: %v", err)
	}

	role := adminmodel.Role{Name: "Limited", Code: "limited", Status: 1}
	if err := store.DB.Create(&role).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := store.DB.Model(&role).Association("Menus").Replace([]adminmodel.Menu{child}); err != nil {
		t.Fatalf("seed role menus: %v", err)
	}

	super := adminmodel.User{Username: "super", Password: "unused", Status: 1}
	if err := store.DB.Create(&super).Error; err != nil {
		t.Fatalf("create super placeholder: %v", err)
	}
	user := adminmodel.User{Username: "limited", Password: "unused", Status: 1, Roles: []adminmodel.Role{role}}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	app := fiber.New()
	app.Get("/menu/all", func(c *fiber.Ctx) error {
		c.Locals("username", "limited")
		return GetAllMenus(c)
	})
	req, err := http.NewRequest(http.MethodGet, "/menu/all", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("get menus request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var parsed struct {
		Code int `json:"code"`
		Data []struct {
			Name     string `json:"name"`
			Children []struct {
				Name string `json:"name"`
			} `json:"children"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(parsed.Data) != 1 || parsed.Data[0].Name != "System" {
		t.Fatalf("expected System parent in menu tree, got %#v", parsed.Data)
	}
	if len(parsed.Data[0].Children) != 1 || parsed.Data[0].Children[0].Name != "SystemUser" {
		t.Fatalf("expected SystemUser child in menu tree, got %#v", parsed.Data[0].Children)
	}
}

func TestGetAllMenusHidesCatalogWhenNoGrantedPageChild(t *testing.T) {
	setupRolePermissionTestDB(t)

	catalog := adminmodel.Menu{Name: "System", Path: "/system", Type: "catalog", Title: "system.title", Status: 1}
	if err := store.DB.Create(&catalog).Error; err != nil {
		t.Fatalf("create catalog menu: %v", err)
	}
	child := adminmodel.Menu{Name: "SystemUser", ParentID: catalog.ID, Path: "/system/user", Component: "/system/user/list", Type: "menu", Title: "system.user.title", Status: 1}
	if err := store.DB.Create(&child).Error; err != nil {
		t.Fatalf("create child menu: %v", err)
	}

	role := adminmodel.Role{Name: "CatalogOnly", Code: "catalog_only", Status: 1}
	if err := store.DB.Create(&role).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := store.DB.Model(&role).Association("Menus").Replace([]adminmodel.Menu{catalog}); err != nil {
		t.Fatalf("seed role menus: %v", err)
	}

	super := adminmodel.User{Username: "super", Password: "unused", Status: 1}
	if err := store.DB.Create(&super).Error; err != nil {
		t.Fatalf("create super placeholder: %v", err)
	}
	user := adminmodel.User{Username: "limited", Password: "unused", Status: 1, Roles: []adminmodel.Role{role}}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	app := fiber.New()
	app.Get("/menu/all", func(c *fiber.Ctx) error {
		c.Locals("username", "limited")
		return GetAllMenus(c)
	})
	req, err := http.NewRequest(http.MethodGet, "/menu/all", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("get menus request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var parsed struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(parsed.Data) != 0 {
		t.Fatalf("expected no visible menus, got %#v", parsed.Data)
	}
}

func TestGetUserInfoFallsBackToFirstAccessibleMenuWhenHomePathIsNotGranted(t *testing.T) {
	setupRolePermissionTestDB(t)

	parent := adminmodel.Menu{Name: "System", Path: "/system", Type: "catalog", Title: "system.title", Status: 1}
	if err := store.DB.Create(&parent).Error; err != nil {
		t.Fatalf("create parent menu: %v", err)
	}
	child := adminmodel.Menu{Name: "SystemUser", ParentID: parent.ID, Path: "/system/user", Component: "/system/user/list", Type: "menu", Title: "system.user.title", Status: 1}
	if err := store.DB.Create(&child).Error; err != nil {
		t.Fatalf("create child menu: %v", err)
	}

	role := adminmodel.Role{Name: "Limited", Code: "limited", Status: 1}
	if err := store.DB.Create(&role).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := store.DB.Model(&role).Association("Menus").Replace([]adminmodel.Menu{child}); err != nil {
		t.Fatalf("seed role menus: %v", err)
	}

	super := adminmodel.User{Username: "super", Password: "unused", Status: 1}
	if err := store.DB.Create(&super).Error; err != nil {
		t.Fatalf("create super placeholder: %v", err)
	}
	user := adminmodel.User{Username: "limited", Password: "unused", Status: 1, HomePath: "/analytics", Roles: []adminmodel.Role{role}}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	app := fiber.New()
	app.Get("/user/info", func(c *fiber.Ctx) error {
		c.Locals("userId", user.ID)
		return GetUserInfo(c)
	})
	req, err := http.NewRequest(http.MethodGet, "/user/info", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("get user info request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var parsed struct {
		Data struct {
			HomePath string `json:"homePath"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if parsed.Data.HomePath != "/system/user" {
		t.Fatalf("expected fallback home path /system/user, got %q", parsed.Data.HomePath)
	}
}
