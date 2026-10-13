package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
)

// TestCreateRoleRejectsSuperCode 锁定 super 越权修复：禁止通过接口新建 code=super 的角色
// （CasbinAuth 见到该角色码直接全量放行，是最高特权标识，只允许种子创建）。
func TestCreateRoleRejectsSuperCode(t *testing.T) {
	setupRolePermissionTestDB(t)

	app := fiber.New()
	app.Post("/role", CreateRole)

	body, _ := json.Marshal(fiber.Map{"name": "冒充超管", "code": "super", "status": 1})
	req, _ := http.NewRequest(http.MethodPost, "/role", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("create role request: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting reserved code super, got %d", resp.StatusCode)
	}

	var count int64
	store.DB.Model(&adminmodel.Role{}).Where("code = ?", "super").Count(&count)
	if count != 0 {
		t.Fatalf("super-coded role must not be created, found %d", count)
	}
}

// TestCreateUserRejectsSuperRoleAssignmentByNonSuper 锁定 super 越权修复：普通管理员
// （非内置超管 id=1、且不持 super 角色）不得把已存在的 super 角色分配给任何用户。
func TestCreateUserRejectsSuperRoleAssignmentByNonSuper(t *testing.T) {
	setupRolePermissionTestDB(t)

	superRole := adminmodel.Role{Name: "超级管理员", Code: "super", Status: 1}
	if err := store.DB.Create(&superRole).Error; err != nil {
		t.Fatalf("seed super role: %v", err)
	}

	app := fiber.New()
	app.Post("/user", func(c *fiber.Ctx) error {
		c.Locals("userId", uint(5))          // 非内置超管
		c.Locals("roles", []string{"admin"}) // 不含 super
		return CreateUser(c)
	})

	body, _ := json.Marshal(fiber.Map{
		"username": "eve", "password": "Np7xQ2wLs9", "realName": "Eve",
		"status": 1, "roleIds": []uint{superRole.ID},
	})
	req, _ := http.NewRequest(http.MethodPost, "/user", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("create user request: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 blocking super-role assignment, got %d", resp.StatusCode)
	}

	var count int64
	store.DB.Model(&adminmodel.User{}).Where("username = ?", "eve").Count(&count)
	if count != 0 {
		t.Fatalf("user must not be created when assignment blocked, found %d", count)
	}
}

// TestCreateUserAllowsSuperRoleAssignmentBySuper 反向用例：内置超管（id=1）仍可正常分配 super 角色，
// 确认防线只挡住越权者、不误伤合法超管。
func TestCreateUserAllowsSuperRoleAssignmentBySuper(t *testing.T) {
	setupRolePermissionTestDB(t)

	superRole := adminmodel.Role{Name: "超级管理员", Code: "super", Status: 1}
	if err := store.DB.Create(&superRole).Error; err != nil {
		t.Fatalf("seed super role: %v", err)
	}

	app := fiber.New()
	app.Post("/user", func(c *fiber.Ctx) error {
		c.Locals("userId", uint(1)) // 内置超管
		return CreateUser(c)
	})

	body, _ := json.Marshal(fiber.Map{
		"username": "trusted", "password": "Np7xQ2wLs9", "realName": "Trusted",
		"status": 1, "roleIds": []uint{superRole.ID},
	})
	req, _ := http.NewRequest(http.MethodPost, "/user", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("create user request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("built-in super operator should be allowed to assign super role, got %d", resp.StatusCode)
	}
}
