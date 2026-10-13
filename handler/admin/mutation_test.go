package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/leivorn/base-kit/middleware"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"
	"github.com/leivorn/base-kit/validator"

	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// setupHandlerTestDB 建全量表的测试库，并像生产一样开启 TranslateError。
func setupHandlerTestDB(t *testing.T) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.User{}, &adminmodel.Role{}, &adminmodel.Menu{}, &adminmodel.Dept{}, &adminmodel.Config{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}

	store.DB = db
	validator.Init()
	middleware.InvalidatePermissionCache()
	middleware.InvalidateUserAuthCache(0)
}

// asOperator 把 handler 包一层，模拟 JWTAuth 装配好的请求上下文。
func asOperator(userID uint, roles []string, h fiber.Handler) fiber.Handler {
	return func(c *fiber.Ctx) error {
		c.Locals("userId", userID)
		c.Locals("roles", roles)
		return h(c)
	}
}

func jsonRequest(t *testing.T, app *fiber.App, method, path string, body any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	return resp
}

func mustCreate(t *testing.T, v any) {
	t.Helper()
	if err := store.DB.Create(v).Error; err != nil {
		t.Fatalf("create %T: %v", v, err)
	}
}

func countWhere(t *testing.T, model any, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := store.DB.Model(model).Unscoped().Where(query, args...).Count(&n).Error; err != nil {
		t.Fatalf("count %T: %v", model, err)
	}
	return n
}

// --- super 角色持有者保护 ---

func seedSuperHolder(t *testing.T) (adminmodel.Role, adminmodel.User) {
	t.Helper()
	// id=1 占位，保证被测用户不是内置超管
	mustCreate(t, &adminmodel.User{Username: "super", Password: "unused", Status: 1})
	superRole := adminmodel.Role{Name: "超级管理员", Code: "super", Status: 1}
	mustCreate(t, &superRole)
	holder := adminmodel.User{Username: "root2", Password: "original-hash", Status: 1, Roles: []adminmodel.Role{superRole}}
	mustCreate(t, &holder)
	return superRole, holder
}

func TestUpdateUserRejectsSuperRoleHolderByNonSuper(t *testing.T) {
	setupHandlerTestDB(t)
	superRole, holder := seedSuperHolder(t)

	app := fiber.New()
	app.Put("/user/:id", asOperator(5, []string{"admin"}, UpdateUser))
	resp := jsonRequest(t, app, http.MethodPut, fmt.Sprintf("/user/%d", holder.ID), fiber.Map{
		"realName": "Hijacked", "password": "newpass123", "status": 1, "roleIds": []uint{},
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}

	var reloaded adminmodel.User
	if err := store.DB.Preload("Roles").First(&reloaded, holder.ID).Error; err != nil {
		t.Fatalf("reload holder: %v", err)
	}
	if reloaded.Password != "original-hash" {
		t.Fatalf("password must stay untouched, got %q", reloaded.Password)
	}
	if len(reloaded.Roles) != 1 || reloaded.Roles[0].ID != superRole.ID {
		t.Fatalf("super role must stay attached, got %#v", reloaded.Roles)
	}
}

func TestDeleteUserRejectsSuperRoleHolderByNonSuper(t *testing.T) {
	setupHandlerTestDB(t)
	_, holder := seedSuperHolder(t)

	app := fiber.New()
	app.Delete("/user/:id", asOperator(5, []string{"admin"}, DeleteUser))
	resp := jsonRequest(t, app, http.MethodDelete, fmt.Sprintf("/user/%d", holder.ID), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	if countWhere(t, &adminmodel.User{}, "id = ?", holder.ID) != 1 {
		t.Fatal("holder must not be deleted")
	}
}

func TestUpdateUserAllowsSuperRoleHolderBySuperOperator(t *testing.T) {
	setupHandlerTestDB(t)
	_, holder := seedSuperHolder(t)

	app := fiber.New()
	app.Put("/user/:id", asOperator(1, nil, UpdateUser))
	resp := jsonRequest(t, app, http.MethodPut, fmt.Sprintf("/user/%d", holder.ID), fiber.Map{
		"realName": "Renamed", "status": 1,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("built-in super must be allowed, got %d", resp.StatusCode)
	}
}

// --- 物理删除 ---

func TestDeleteRoleHardDeletesAndAllowsRecreate(t *testing.T) {
	setupHandlerTestDB(t)

	menu := adminmodel.Menu{Name: "M", Type: "menu", Title: "m", Status: 1}
	mustCreate(t, &menu)
	role := adminmodel.Role{Name: "Editor", Code: "editor", Status: 1, Menus: []adminmodel.Menu{menu}}
	mustCreate(t, &role)
	mustCreate(t, &adminmodel.User{Username: "super", Password: "unused", Status: 1})
	user := adminmodel.User{Username: "eve", Password: "unused", Status: 1, Roles: []adminmodel.Role{role}}
	mustCreate(t, &user)

	app := fiber.New()
	app.Delete("/role/:id", DeleteRole)
	app.Post("/role", CreateRole)

	if resp := jsonRequest(t, app, http.MethodDelete, fmt.Sprintf("/role/%d", role.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete role: expected 200, got %d", resp.StatusCode)
	}
	if countWhere(t, &adminmodel.Role{}, "code = ?", "editor") != 0 {
		t.Fatal("role must be physically deleted")
	}
	if countWhere(t, &adminmodel.RoleMenu{}, "role_id = ?", role.ID) != 0 {
		t.Fatal("role_menus must be cleared")
	}
	if countWhere(t, &adminmodel.UserRole{}, "role_id = ?", role.ID) != 0 {
		t.Fatal("user_roles must be cleared")
	}

	resp := jsonRequest(t, app, http.MethodPost, "/role", fiber.Map{"name": "Editor", "code": "editor", "status": 1})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recreate role with same code: expected 200, got %d", resp.StatusCode)
	}
}

func TestDeleteRoleReturns404WhenMissing(t *testing.T) {
	setupHandlerTestDB(t)
	app := fiber.New()
	app.Delete("/role/:id", DeleteRole)
	if resp := jsonRequest(t, app, http.MethodDelete, "/role/999", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCreateRoleDuplicateCodeReturns400(t *testing.T) {
	setupHandlerTestDB(t)
	mustCreate(t, &adminmodel.Role{Name: "Editor", Code: "editor", Status: 1})

	app := fiber.New()
	app.Post("/role", CreateRole)
	resp := jsonRequest(t, app, http.MethodPost, "/role", fiber.Map{"name": "Editor2", "code": "editor", "status": 1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for duplicate code, got %d", resp.StatusCode)
	}
}

func TestDeleteUserHardDeletesAndAllowsSameUsernameAgain(t *testing.T) {
	setupHandlerTestDB(t)
	mustCreate(t, &adminmodel.User{Username: "super", Password: "unused", Status: 1})
	role := adminmodel.Role{Name: "User", Code: "user", Status: 1}
	mustCreate(t, &role)
	user := adminmodel.User{Username: "temp", Password: "unused", Status: 1, Roles: []adminmodel.Role{role}}
	mustCreate(t, &user)

	app := fiber.New()
	app.Delete("/user/:id", asOperator(1, nil, DeleteUser))
	app.Post("/user", asOperator(1, nil, CreateUser))

	if resp := jsonRequest(t, app, http.MethodDelete, fmt.Sprintf("/user/%d", user.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete user: expected 200, got %d", resp.StatusCode)
	}
	if countWhere(t, &adminmodel.User{}, "username = ?", "temp") != 0 {
		t.Fatal("user must be physically deleted")
	}
	if countWhere(t, &adminmodel.UserRole{}, "user_id = ?", user.ID) != 0 {
		t.Fatal("user_roles must be cleared")
	}

	resp := jsonRequest(t, app, http.MethodPost, "/user", fiber.Map{
		"username": "temp", "password": "Np7xQ2wLs9", "realName": "Temp", "status": 1,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recreate user with same username: expected 200, got %d", resp.StatusCode)
	}

	if resp := jsonRequest(t, app, http.MethodDelete, "/user/999", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing user: expected 404, got %d", resp.StatusCode)
	}
}

func TestConfigDuplicateKeyAndHardDelete(t *testing.T) {
	setupHandlerTestDB(t)
	cfg := adminmodel.Config{ConfigName: "站点名称", ConfigKey: "site_name", ConfigValue: "x", Status: 1}
	mustCreate(t, &cfg)

	app := fiber.New()
	app.Post("/config", CreateConfig)
	app.Put("/config/:id", UpdateConfig)
	app.Delete("/config/:id", DeleteConfig)

	dup := fiber.Map{"configName": "站点名称", "configKey": "site_name", "status": 1}
	if resp := jsonRequest(t, app, http.MethodPost, "/config", dup); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate key: expected 400, got %d", resp.StatusCode)
	}
	if resp := jsonRequest(t, app, http.MethodPut, "/config/999", dup); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update missing: expected 404, got %d", resp.StatusCode)
	}
	if resp := jsonRequest(t, app, http.MethodDelete, fmt.Sprintf("/config/%d", cfg.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", resp.StatusCode)
	}
	if countWhere(t, &adminmodel.Config{}, "config_key = ?", "site_name") != 0 {
		t.Fatal("config must be physically deleted")
	}
	if resp := jsonRequest(t, app, http.MethodPost, "/config", dup); resp.StatusCode != http.StatusOK {
		t.Fatalf("recreate after delete: expected 200, got %d", resp.StatusCode)
	}
	if resp := jsonRequest(t, app, http.MethodDelete, "/config/999", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing: expected 404, got %d", resp.StatusCode)
	}
}

// --- 菜单/部门树 ---

func menuBody(name string, pid uint) fiber.Map {
	return fiber.Map{"pid": pid, "name": name, "type": "menu", "title": name, "status": 1}
}

func TestDeleteMenuRemovesDescendantsAndRoleMenus(t *testing.T) {
	setupHandlerTestDB(t)

	catalog := adminmodel.Menu{Name: "System", Type: "catalog", Title: "system", Status: 1}
	mustCreate(t, &catalog)
	page := adminmodel.Menu{Name: "SystemUser", ParentID: catalog.ID, Type: "menu", Title: "user", AuthCode: "System:User:List", Status: 1}
	mustCreate(t, &page)
	button := adminmodel.Menu{Name: "SystemUserCreate", ParentID: page.ID, Type: "button", Title: "create", AuthCode: "System:User:Create", Status: 1}
	mustCreate(t, &button)
	role := adminmodel.Role{Name: "Admin", Code: "admin", Status: 1, Menus: []adminmodel.Menu{page, button}}
	mustCreate(t, &role)

	app := fiber.New()
	app.Delete("/menu/:id", DeleteMenu)
	if resp := jsonRequest(t, app, http.MethodDelete, fmt.Sprintf("/menu/%d", catalog.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var remaining int64
	store.DB.Model(&adminmodel.Menu{}).Where("id IN ?", []uint{catalog.ID, page.ID, button.ID}).Count(&remaining)
	if remaining != 0 {
		t.Fatalf("catalog, page and button must all be deleted, %d remain", remaining)
	}
	if countWhere(t, &adminmodel.RoleMenu{}, "menu_id IN ?", []uint{page.ID, button.ID}) != 0 {
		t.Fatal("role_menus of deleted menus must be cleared")
	}

	if resp := jsonRequest(t, app, http.MethodDelete, "/menu/999", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing: expected 404, got %d", resp.StatusCode)
	}
}

func TestMenuParentValidation(t *testing.T) {
	setupHandlerTestDB(t)

	root := adminmodel.Menu{Name: "Root", Type: "catalog", Title: "root", Status: 1}
	mustCreate(t, &root)
	child := adminmodel.Menu{Name: "Child", ParentID: root.ID, Type: "menu", Title: "child", Status: 1}
	mustCreate(t, &child)

	app := fiber.New()
	app.Post("/menu", CreateMenu)
	app.Put("/menu/:id", UpdateMenu)

	if resp := jsonRequest(t, app, http.MethodPut, fmt.Sprintf("/menu/%d", root.ID), menuBody("Root", root.ID)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("pid=self: expected 400, got %d", resp.StatusCode)
	}
	if resp := jsonRequest(t, app, http.MethodPut, fmt.Sprintf("/menu/%d", root.ID), menuBody("Root", child.ID)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("pid=descendant: expected 400, got %d", resp.StatusCode)
	}
	if resp := jsonRequest(t, app, http.MethodPut, fmt.Sprintf("/menu/%d", root.ID), menuBody("Root", 999)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("pid=missing: expected 400, got %d", resp.StatusCode)
	}
	if resp := jsonRequest(t, app, http.MethodPut, "/menu/999", menuBody("Ghost", 0)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update missing: expected 404, got %d", resp.StatusCode)
	}
	if resp := jsonRequest(t, app, http.MethodPost, "/menu", menuBody("Orphan", 999)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create with missing parent: expected 400, got %d", resp.StatusCode)
	}

	var reloaded adminmodel.Menu
	store.DB.First(&reloaded, root.ID)
	if reloaded.ParentID != 0 {
		t.Fatalf("root parent must stay 0, got %d", reloaded.ParentID)
	}

	// 合法的父级变更仍然成功
	if resp := jsonRequest(t, app, http.MethodPut, fmt.Sprintf("/menu/%d", child.ID), menuBody("Child", 0)); resp.StatusCode != http.StatusOK {
		t.Fatalf("valid reparent: expected 200, got %d", resp.StatusCode)
	}
}

func deptBody(name string, pid uint) fiber.Map {
	return fiber.Map{"pid": pid, "name": name, "status": 1}
}

func TestDeptTreeDeleteAndCycleGuard(t *testing.T) {
	setupHandlerTestDB(t)

	root := adminmodel.Dept{Name: "总公司", Status: 1}
	mustCreate(t, &root)
	mid := adminmodel.Dept{Name: "技术部", ParentID: root.ID, Status: 1}
	mustCreate(t, &mid)
	leaf := adminmodel.Dept{Name: "前端组", ParentID: mid.ID, Status: 1}
	mustCreate(t, &leaf)

	app := fiber.New()
	app.Put("/dept/:id", UpdateDept)
	app.Delete("/dept/:id", DeleteDept)

	if resp := jsonRequest(t, app, http.MethodPut, fmt.Sprintf("/dept/%d", root.ID), deptBody("总公司", leaf.ID)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("pid=descendant: expected 400, got %d", resp.StatusCode)
	}
	if resp := jsonRequest(t, app, http.MethodDelete, fmt.Sprintf("/dept/%d", root.ID), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete root: expected 200, got %d", resp.StatusCode)
	}
	var remaining int64
	store.DB.Model(&adminmodel.Dept{}).Count(&remaining)
	if remaining != 0 {
		t.Fatalf("all depts must be deleted, %d remain", remaining)
	}
}

// --- 角色列表筛选 ---

func TestGetRoleListFiltersByCreateTimeRange(t *testing.T) {
	setupHandlerTestDB(t)

	day := func(d int) time.Time { return time.Date(2026, 1, d, 10, 0, 0, 0, time.Local) }
	for i, d := range []int{1, 2, 3} {
		mustCreate(t, &adminmodel.Role{Name: fmt.Sprintf("R%d", i), Code: fmt.Sprintf("r%d", i), Status: 1,
			BaseModel: modelBase(day(d))})
	}

	app := fiber.New()
	app.Get("/role/list", GetRoleList)

	decode := func(resp *http.Response) []string {
		t.Helper()
		var parsed struct {
			Data struct {
				Items []struct {
					Code string `json:"code"`
				} `json:"items"`
				Total int64 `json:"total"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		codes := make([]string, len(parsed.Data.Items))
		for i, it := range parsed.Data.Items {
			codes[i] = it.Code
		}
		return codes
	}

	resp := jsonRequest(t, app, http.MethodGet, "/role/list?startTime=2026-01-02&endTime=2026-01-02", nil)
	if codes := decode(resp); len(codes) != 1 || codes[0] != "r1" {
		t.Fatalf("date-only range: expected [r1], got %v", codes)
	}
	resp = jsonRequest(t, app, http.MethodGet, "/role/list?startTime=2026-01-02%2000:00:00&endTime=2026-01-03%2023:59:59", nil)
	if codes := decode(resp); len(codes) != 2 {
		t.Fatalf("datetime range: expected 2 roles, got %v", codes)
	}
	resp = jsonRequest(t, app, http.MethodGet, "/role/list?pageSize=100000&startTime=garbage", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("huge pageSize + bad time: expected 200, got %d", resp.StatusCode)
	}
	if codes := decode(resp); len(codes) != 3 {
		t.Fatalf("unfiltered: expected 3 roles, got %v", codes)
	}
}
