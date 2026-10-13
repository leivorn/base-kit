package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
)

// 解析 dto.Success 返回的 {code, data, ...} 中 data 段为 bool 的便捷工具
func decodeBoolPayload(t *testing.T, resp *http.Response) bool {
	t.Helper()
	var body struct {
		Data bool `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body.Data
}

// 名称校验：新增模式下，遇到已有同名记录应该返回 true。
// 编辑模式下应排除自身，否则编辑时校验把自己当成冲突。
func TestCheckMenuNameExistsRespectsExcludeID(t *testing.T) {
	setupRolePermissionTestDB(t)

	target := adminmodel.Menu{Name: "SystemOperationLog", Type: "menu", Title: "title", Status: 1}
	if err := store.DB.Create(&target).Error; err != nil {
		t.Fatalf("seed menu: %v", err)
	}

	app := fiber.New()
	app.Get("/name-exists", CheckMenuNameExists)

	cases := []struct {
		label    string
		url      string
		expected bool
	}{
		{"create-mode hits existing name", "/name-exists?name=SystemOperationLog", true},
		{"edit-mode excludes self", fmt.Sprintf("/name-exists?name=SystemOperationLog&id=%d", target.ID), false},
		{"edit-mode another id still conflicts", "/name-exists?name=SystemOperationLog&id=9999", true},
		{"empty id ignored", "/name-exists?name=SystemOperationLog&id=", true},
		{"invalid id ignored", "/name-exists?name=SystemOperationLog&id=abc", true},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet, tc.url, nil)
		if err != nil {
			t.Fatalf("%s: new request: %v", tc.label, err)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("%s: request: %v", tc.label, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", tc.label, resp.StatusCode)
		}
		got := decodeBoolPayload(t, resp)
		if got != tc.expected {
			t.Fatalf("%s: expected %v, got %v", tc.label, tc.expected, got)
		}
	}
}

// 路径校验：与名称校验对称。
func TestCheckMenuPathExistsRespectsExcludeID(t *testing.T) {
	setupRolePermissionTestDB(t)

	target := adminmodel.Menu{Name: "SystemOperationLog", Path: "/system/operation-log", Type: "menu", Title: "title", Status: 1}
	if err := store.DB.Create(&target).Error; err != nil {
		t.Fatalf("seed menu: %v", err)
	}

	app := fiber.New()
	app.Get("/path-exists", CheckMenuPathExists)

	cases := []struct {
		label    string
		url      string
		expected bool
	}{
		{"create-mode hits existing path", "/path-exists?path=/system/operation-log", true},
		{"edit-mode excludes self", fmt.Sprintf("/path-exists?path=/system/operation-log&id=%d", target.ID), false},
		{"edit-mode another id still conflicts", "/path-exists?path=/system/operation-log&id=9999", true},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet, tc.url, nil)
		if err != nil {
			t.Fatalf("%s: new request: %v", tc.label, err)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("%s: request: %v", tc.label, err)
		}
		got := decodeBoolPayload(t, resp)
		if got != tc.expected {
			t.Fatalf("%s: expected %v, got %v", tc.label, tc.expected, got)
		}
	}
}
