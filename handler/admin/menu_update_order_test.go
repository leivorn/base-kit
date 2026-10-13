package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
)

// UpdateMenu 不带 order 字段时不能重置 OrderNo。
//
// 历史 bug：MenuRequest.OrderNo 是 int，前端漏传 order 时反序列化为 0，
// UpdateMenu 用 map[string]any 把 order_no 写成 0，导致编辑过的菜单"跑到最上面"。
// 修复后 OrderNo 是 *int，nil 表示未提交，UpdateMenu 跳过 order_no 更新。
func TestUpdateMenuWithoutOrderKeepsExistingOrderNo(t *testing.T) {
	setupRolePermissionTestDB(t)

	target := adminmodel.Menu{
		Name:    "SystemOperationLog",
		Path:    "/system/operation-log",
		Type:    "menu",
		Title:   "system.operationLog.title",
		Status:  1,
		OrderNo: 6,
	}
	if err := store.DB.Create(&target).Error; err != nil {
		t.Fatalf("seed menu: %v", err)
	}

	// 不传 order 字段（模拟前端旧行为或留空）
	body, err := json.Marshal(fiber.Map{
		"pid":       0,
		"name":      "SystemOperationLog",
		"path":      "/system/operation-log",
		"component": "",
		"type":      "menu",
		"title":     "system.operationLog.title",
		"authCode":  "System:OperationLog:List",
		"status":    1,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	app := fiber.New()
	app.Put("/menu/:id", UpdateMenu)
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("/menu/%d", target.ID), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var got adminmodel.Menu
	if err := store.DB.First(&got, target.ID).Error; err != nil {
		t.Fatalf("reload menu: %v", err)
	}
	if got.OrderNo != 6 {
		t.Fatalf("OrderNo expected to stay 6, got %d", got.OrderNo)
	}
}

// 显式传 order 时必须落库（覆盖未传与显式 0 的两条路径）。
func TestUpdateMenuExplicitOrderIsPersisted(t *testing.T) {
	setupRolePermissionTestDB(t)

	target := adminmodel.Menu{
		Name:    "SystemOperationLog",
		Path:    "/system/operation-log",
		Type:    "menu",
		Title:   "system.operationLog.title",
		Status:  1,
		OrderNo: 6,
	}
	if err := store.DB.Create(&target).Error; err != nil {
		t.Fatalf("seed menu: %v", err)
	}

	cases := []struct {
		name     string
		order    *int
		expected int
	}{
		{"set to 0 (top)", intPtr(0), 0},
		{"set to 12", intPtr(12), 12},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := fiber.Map{
				"pid":       0,
				"name":      "SystemOperationLog",
				"path":      "/system/operation-log",
				"component": "",
				"type":      "menu",
				"title":     "system.operationLog.title",
				"authCode":  "System:OperationLog:List",
				"status":    1,
				"order":     *tc.order,
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}

			app := fiber.New()
			app.Put("/menu/:id", UpdateMenu)
			req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("/menu/%d", target.ID), bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}

			var got adminmodel.Menu
			if err := store.DB.First(&got, target.ID).Error; err != nil {
				t.Fatalf("reload menu: %v", err)
			}
			if got.OrderNo != tc.expected {
				t.Fatalf("OrderNo expected %d, got %d", tc.expected, got.OrderNo)
			}
		})
	}
}

func intPtr(v int) *int { return &v }
