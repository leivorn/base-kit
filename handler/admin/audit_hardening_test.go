package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/leivorn/base-kit/model"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
)

func TestSafeMenuURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		safe bool
	}{
		{"", true},
		{"https://example.com/docs", true},
		{"HTTP://example.com", true},
		{"  https://example.com  ", true},
		{"/docs/index.html", true},
		{"//cdn.example.com/page", true},
		{"javascript:alert(1)", false},
		{"  JaVaScRiPt:alert(1)", false},
		{"java\tscript:alert(1)", false},
		{"https://example.com/\nx", false},
		{"data:text/html,<script>alert(1)</script>", false},
		{"vbscript:msgbox(1)", false},
		{"https:///no-host", false},
		{"example.com/no-scheme", false},
	} {
		if got := safeMenuURL(tc.url); got != tc.safe {
			t.Errorf("safeMenuURL(%q) = %v，期望 %v", tc.url, got, tc.safe)
		}
	}
}

// 有菜单编辑权限的人不能写入 javascript: 等地址（超管点开菜单时会在后台域名下执行，读走 token）。
func TestCreateAndUpdateMenuRejectUnsafeURL(t *testing.T) {
	setupRolePermissionTestDB(t)
	app := fiber.New()
	app.Post("/menu", CreateMenu)
	app.Put("/menu/:id", UpdateMenu)

	send := func(method, path string, body fiber.Map) int {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp.StatusCode
	}
	menu := func(name, typ, field, url string) fiber.Map {
		return fiber.Map{"name": name, "title": name, "type": typ, "status": 1, "path": "/" + name, field: url}
	}

	if code := send(http.MethodPost, "/menu", menu("Evil", "link", "link", "javascript:alert(document.cookie)")); code != http.StatusBadRequest {
		t.Errorf("外链 javascript: = %d，期望 400", code)
	}
	if code := send(http.MethodPost, "/menu", menu("EvilFrame", "embedded", "iframeSrc", "java\tscript:alert(1)")); code != http.StatusBadRequest {
		t.Errorf("内嵌 java\\tscript: = %d，期望 400", code)
	}
	if code := send(http.MethodPost, "/menu", menu("Docs", "link", "link", " https://example.com/docs ")); code != http.StatusOK {
		t.Fatalf("合法外链 = %d，期望 200", code)
	}
	var docs adminmodel.Menu
	store.DB.Where("name = ?", "Docs").First(&docs)
	if docs.Link != "https://example.com/docs" {
		t.Errorf("外链应去掉首尾空白再落库，得到 %q", docs.Link)
	}

	path := fmt.Sprintf("/menu/%d", docs.ID)
	if code := send(http.MethodPut, path, menu("Docs", "embedded", "iframeSrc", "data:text/html,<script>alert(1)</script>")); code != http.StatusBadRequest {
		t.Errorf("更新为 data: 地址 = %d，期望 400", code)
	}
	if code := send(http.MethodPut, path, menu("Docs", "embedded", "iframeSrc", "/internal/page")); code != http.StatusOK {
		t.Errorf("更新为站内路径 = %d，期望 200", code)
	}
}

// 校验上线前已经存进库里的危险地址，不下发给前端渲染；菜单管理列表仍原样展示，便于管理员改掉。
func TestRuntimeMenuTreeDropsUnsafeStoredURL(t *testing.T) {
	menus := []adminmodel.Menu{
		{BaseModel: model.BaseModel{ID: 1}, Name: "Evil", Type: "link", Path: "/evil", Link: "javascript:alert(1)", Status: 1},
		{BaseModel: model.BaseModel{ID: 2}, Name: "Docs", Type: "embedded", Path: "/docs", IframeSrc: "https://example.com", Status: 1},
	}
	tree := buildMenuTree(menus, 0)
	if len(tree) != 2 {
		t.Fatalf("应保留两个菜单节点，得到 %d", len(tree))
	}
	for _, node := range tree {
		meta := node["meta"].(fiber.Map)
		switch node["name"] {
		case "Evil":
			if _, ok := meta["link"]; ok {
				t.Error("危险外链不应下发给前端")
			}
		case "Docs":
			if meta["iframeSrc"] != "https://example.com" {
				t.Errorf("合法内嵌地址应照常下发，得到 %v", meta["iframeSrc"])
			}
		}
	}
	manage := buildMenuTreeForManage(menus, 0)
	for _, node := range manage {
		if node["name"] == "Evil" && node["meta"].(fiber.Map)["link"] != "javascript:alert(1)" {
			t.Error("菜单管理列表应原样展示，管理员才能看到并修改")
		}
	}
}

// 审计记录只有超管能删：持有删除权限码的普通管理员也不行。
func TestOperationLogDeletionIsSuperOnly(t *testing.T) {
	setupRolePermissionTestDB(t)
	if err := store.DB.AutoMigrate(&adminmodel.OperationLog{}); err != nil {
		t.Fatalf("migrate operation logs: %v", err)
	}
	for i := range 3 {
		store.DB.Create(&adminmodel.OperationLog{Username: "admin", Method: "POST", Path: fmt.Sprintf("/x/%d", i)})
	}

	as := func(userID uint, roles ...string) *fiber.App {
		app := fiber.New()
		set := func(c *fiber.Ctx) error {
			c.Locals("userId", userID)
			c.Locals("roles", roles)
			return c.Next()
		}
		app.Delete("/log/clear", set, ClearOperationLog)
		app.Delete("/log/:id", set, DeleteOperationLog)
		return app
	}
	del := func(app *fiber.App, path string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodDelete, path, nil)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("DELETE %s: %v", path, err)
		}
		return resp.StatusCode
	}
	count := func() int64 {
		var n int64
		store.DB.Model(&adminmodel.OperationLog{}).Count(&n)
		return n
	}

	admin := as(5, "admin")
	if code := del(admin, "/log/1"); code != http.StatusForbidden {
		t.Errorf("普通管理员删单条 = %d，期望 403", code)
	}
	if code := del(admin, "/log/clear"); code != http.StatusForbidden {
		t.Errorf("普通管理员清空 = %d，期望 403", code)
	}
	if count() != 3 {
		t.Fatalf("普通管理员不应删掉任何日志，剩 %d 条", count())
	}

	if code := del(as(7, "super"), "/log/1"); code != http.StatusOK {
		t.Errorf("持 super 角色删单条 = %d，期望 200", code)
	}
	if code := del(as(1), "/log/clear"); code != http.StatusOK {
		t.Errorf("内置超管清空 = %d，期望 200", code)
	}
	if count() != 0 {
		t.Errorf("超管清空后应无日志，剩 %d 条", count())
	}
}
