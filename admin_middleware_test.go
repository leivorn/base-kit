package basekit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/leivorn/base-kit/middleware"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
)

// TestAdminMiddlewaresApplyOncePerRequest 锁定 kit 的 /admin 中间件与下游路由的关系。
//
// SetupAdmin 把 JWTAuth / PermissionAuth / OperationLog 挂在 /admin 前缀上，Routes 里注册的 /admin 路由
// 不挂也会经过它们。模板 router/project.go 的注释曾让下游「参考 protected 分组的中间件挂法」再挂一遍，
// 结果每个写操作记两条操作日志；PreRoutes 的分组上挂了、请求又落到后面的路由，也是两遍。
// PreRoutes 里的路由排在这些中间件前面，自己不挂就没有鉴权。
func TestAdminMiddlewaresApplyOncePerRequest(t *testing.T) {
	ok := func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) }
	app, err := NewApp(Options{
		Config: testConfig(t, "development"),
		PreRoutes: func(app *fiber.App) {
			app.Group("/admin/pre", middleware.JWTAuth(), middleware.PermissionAuth(), middleware.OperationLog())
			app.Post("/admin/bare", ok)
		},
		Routes: func(app *fiber.App) {
			app.Post("/admin/plain/item", ok)
			app.Group("/admin/remount", middleware.JWTAuth(), middleware.PermissionAuth(), middleware.OperationLog()).
				Post("/item", ok)
			app.Post("/admin/pre/item", ok)
			app.Post("/admin/plain/sentinel", ok)
		},
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := store.DB.DB(); err == nil {
			sqlDB.Close()
		}
	})

	if status := postJSON(t, app, "/admin/bare", ""); status != http.StatusOK {
		t.Errorf("PreRoutes 里不挂中间件的路由无 token 访问 = %d，期望 200（它不经过 kit 的鉴权）", status)
	}
	if status := postJSON(t, app, "/admin/plain/item", ""); status != http.StatusUnauthorized {
		t.Errorf("Routes 里的 /admin 路由无 token 访问 = %d，期望 401", status)
	}

	token := loginToken(t, app, "super", "123456")
	paths := []string{"/admin/plain/item", "/admin/remount/item", "/admin/pre/item"}
	for _, path := range paths {
		if status := postJSON(t, app, path, token); status != http.StatusOK {
			t.Fatalf("POST %s = %d，期望 200", path, status)
		}
	}

	// 日志由后台单个 writer 按入队顺序写库：最后一个请求的日志落库了，前面的一定也写完了
	postJSON(t, app, "/admin/plain/sentinel", token)
	waitForOperationLog(t, "/admin/plain/sentinel")

	// 按请求体认日志：每个请求的请求体不同，中间件也拷贝了请求体；path 等列在 v1.0.3 里直接引用
	// Fiber 会复用的缓冲区，后续请求会把它改掉
	for _, path := range paths {
		var logs []adminmodel.OperationLog
		if err := store.DB.Where("body = ?", requestBody(path)).Find(&logs).Error; err != nil {
			t.Fatalf("查询 %s 的操作日志: %v", path, err)
		}
		if len(logs) != 1 {
			t.Errorf("POST %s 记了 %d 条操作日志，期望 1 条", path, len(logs))
			continue
		}
		if logs[0].Username != "super" || logs[0].Status != http.StatusOK {
			t.Errorf("POST %s 的日志 username=%q status=%d，期望 super / 200", path, logs[0].Username, logs[0].Status)
		}
	}
}

// requestBody 给每个路径一个不同的请求体，用来在日志里认出是哪个请求。
func requestBody(path string) string {
	return `{"path":"` + path + `"}`
}

func postJSON(t *testing.T, app *fiber.App, path, token string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(requestBody(path)))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	if token != "" {
		req.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	}
	resp, err := app.Test(req, 10_000)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func loginToken(t *testing.T, app *fiber.App, username, password string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/auth/login",
		strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req, 10_000)
	if err != nil {
		t.Fatalf("登录: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Data struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Data.AccessToken == "" {
		t.Fatalf("登录 = %d，没拿到 accessToken（%v）", resp.StatusCode, err)
	}
	return body.Data.AccessToken
}

func waitForOperationLog(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int64
		if err := store.DB.Model(&adminmodel.OperationLog{}).Where("body = ?", requestBody(path)).Count(&n).Error; err != nil {
			t.Fatalf("查询操作日志: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等了 10 秒，%s 的操作日志还没写进库", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
