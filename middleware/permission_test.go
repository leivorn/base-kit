package middleware

import (
	"net/http"
	"testing"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

func setupPermissionTestDB(t *testing.T) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.User{}, &adminmodel.Role{}, &adminmodel.Menu{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}

	store.DB = db
	// 换库后旧缓存必须作废
	InvalidatePermissionCache()
	InvalidateUserAuthCache(0)
}

// snapshotRouteTables 保存包级路由表，用例结束后恢复，避免 Register* 污染其他用例。
func snapshotRouteTables(t *testing.T) {
	t.Helper()
	savedPerms := append([]compiledRoute(nil), routePermissions...)
	savedAuthed := append([]compiledRoute(nil), authenticatedRoutes...)
	t.Cleanup(func() {
		routePermissions = savedPerms
		authenticatedRoutes = savedAuthed
	})
}

func grantMenuCode(t *testing.T, roleCode string, authCode string) {
	t.Helper()

	role := adminmodel.Role{Name: roleCode, Code: roleCode, Status: 1}
	if err := store.DB.Create(&role).Error; err != nil {
		t.Fatalf("create role %s: %v", roleCode, err)
	}
	menu := adminmodel.Menu{Name: authCode, Type: "menu", Title: authCode, AuthCode: authCode, Status: 1}
	if err := store.DB.Create(&menu).Error; err != nil {
		t.Fatalf("create menu %s: %v", authCode, err)
	}
	if err := store.DB.Model(&role).Association("Menus").Replace([]adminmodel.Menu{menu}); err != nil {
		t.Fatalf("grant menu code %s: %v", authCode, err)
	}
}

func testAppWithRoles(roles []string, guard fiber.Handler) *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("roles", roles)
		return c.Next()
	})
	app.Use(guard)
	ok := func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) }
	app.Get("/admin/system/user/list", ok)
	app.Delete("/admin/system/user/:id", ok)
	app.Get("/admin/shop/order/list", ok)
	app.Get("/admin/shop/ping", ok)
	app.Get("/admin/unregistered", ok)
	return app
}

func doRequest(t *testing.T, app *fiber.App, method, path string) int {
	t.Helper()
	req, err := http.NewRequest(method, path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	return resp.StatusCode
}

func TestPermissionAuthAllowsUserIDOneAsSuperAdmin(t *testing.T) {
	setupPermissionTestDB(t)

	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("userId", uint(1))
		return c.Next()
	})
	app.Use(PermissionAuth())
	app.Delete("/admin/system/user/:id", func(c *fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})

	if got := doRequest(t, app, http.MethodDelete, "/admin/system/user/2"); got != http.StatusOK {
		t.Fatalf("expected status 200, got %d", got)
	}
}

func TestPermissionAuthAllowsCustomRoleByGrantedMenuAuthCode(t *testing.T) {
	setupPermissionTestDB(t)
	grantMenuCode(t, "auditor", "System:User:List")

	app := testAppWithRoles([]string{"auditor"}, PermissionAuth())
	if got := doRequest(t, app, http.MethodGet, "/admin/system/user/list"); got != http.StatusOK {
		t.Fatalf("expected status 200, got %d", got)
	}
}

func TestPermissionAuthDeniesAdminRouteWithoutMatchingMenuAuthCode(t *testing.T) {
	setupPermissionTestDB(t)
	grantMenuCode(t, "admin", "System:User:List")

	app := testAppWithRoles([]string{"admin"}, PermissionAuth())
	if got := doRequest(t, app, http.MethodDelete, "/admin/system/user/2"); got != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d", got)
	}
}

// CasbinAuth 是给下游保留的别名，行为必须与 PermissionAuth 完全一致。
func TestCasbinAuthAliasBehavesLikePermissionAuth(t *testing.T) {
	setupPermissionTestDB(t)
	grantMenuCode(t, "auditor", "System:User:List")

	app := testAppWithRoles([]string{"auditor"}, CasbinAuth())
	if got := doRequest(t, app, http.MethodGet, "/admin/system/user/list"); got != http.StatusOK {
		t.Fatalf("alias: expected status 200, got %d", got)
	}
	if got := doRequest(t, app, http.MethodDelete, "/admin/system/user/2"); got != http.StatusForbidden {
		t.Fatalf("alias: expected status 403, got %d", got)
	}
}

// 未登记权限码的受保护路由默认拒绝，防止下游漏登记导致越权放行。
func TestPermissionAuthDeniesUnregisteredRoute(t *testing.T) {
	setupPermissionTestDB(t)
	grantMenuCode(t, "admin", "System:User:List")

	app := testAppWithRoles([]string{"admin"}, PermissionAuth())
	if got := doRequest(t, app, http.MethodGet, "/admin/unregistered"); got != http.StatusForbidden {
		t.Fatalf("expected status 403 for unregistered route, got %d", got)
	}
}

// 下游通过 RegisterRoutePermissions 登记自己的路由后，权限码判定与基底路由一致。
func TestRegisterRoutePermissionsGuardsDownstreamRoute(t *testing.T) {
	setupPermissionTestDB(t)
	snapshotRouteTables(t)
	grantMenuCode(t, "shopkeeper", "Shop:Order:List")
	grantMenuCode(t, "viewer", "System:User:List")

	RegisterRoutePermissions(RoutePermission{Method: "GET", Path: "/admin/shop/order/list", Code: "Shop:Order:List"})

	granted := testAppWithRoles([]string{"shopkeeper"}, PermissionAuth())
	if got := doRequest(t, granted, http.MethodGet, "/admin/shop/order/list"); got != http.StatusOK {
		t.Fatalf("granted role: expected 200, got %d", got)
	}
	denied := testAppWithRoles([]string{"viewer"}, PermissionAuth())
	if got := doRequest(t, denied, http.MethodGet, "/admin/shop/order/list"); got != http.StatusForbidden {
		t.Fatalf("role without code: expected 403, got %d", got)
	}
}

func TestRegisterAuthenticatedRoutesAllowsAnyLoggedInRole(t *testing.T) {
	setupPermissionTestDB(t)
	snapshotRouteTables(t)

	RegisterAuthenticatedRoutes(RoutePermission{Method: "GET", Path: "/admin/shop/ping"})

	app := testAppWithRoles([]string{"user"}, PermissionAuth())
	if got := doRequest(t, app, http.MethodGet, "/admin/shop/ping"); got != http.StatusOK {
		t.Fatalf("authenticated route: expected 200, got %d", got)
	}
}
