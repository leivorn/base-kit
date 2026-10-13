package basekit

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leivorn/base-kit/config"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
)

// testConfig 造一份能过 ValidateProduction 的配置，库落在 t.TempDir() 里。
func testConfig(t *testing.T, mode string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.Mode = mode
	cfg.Database.Driver = "sqlite"
	cfg.Database.DSN = filepath.Join(t.TempDir(), "kit.db")
	cfg.JWT.Secret = "basekit-test-secret-0123456789abcdefghij"
	cfg.JWT.Expire = time.Hour
	cfg.JWT.RefreshExpire = time.Hour
	return cfg
}

// 5xx 的原文（DB 错误、panic 文本）只能进日志：生产模式对外一句通用的，开发模式保留原文；
// 4xx 两种模式都保持 Fiber 自己的文案。
func TestErrorHandlerMasksInternalErrorsInProduction(t *testing.T) {
	const leak = "pg: password=secret"

	for _, tc := range []struct {
		mode     string
		wantLeak bool
	}{
		{"production", false},
		{"development", true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			app, err := NewApp(Options{
				Config: testConfig(t, tc.mode),
				PreRoutes: func(app *fiber.App) {
					app.Get("/boom", func(c *fiber.Ctx) error { return errors.New(leak) })
					app.Get("/panic", func(c *fiber.Ctx) error { panic(leak) })
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

			for _, path := range []string{"/boom", "/panic"} {
				resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil), 10_000)
				if err != nil {
					t.Fatalf("GET %s: %v", path, err)
				}
				raw, _ := io.ReadAll(resp.Body)
				body := string(raw)
				if resp.StatusCode != http.StatusInternalServerError {
					t.Errorf("GET %s = %d，期望 500: %s", path, resp.StatusCode, body)
				}
				if got := strings.Contains(body, leak); got != tc.wantLeak {
					t.Errorf("GET %s 响应体含原文 = %v，期望 %v: %s", path, got, tc.wantLeak, body)
				}
				if !tc.wantLeak && !strings.Contains(body, "Internal Server Error") {
					t.Errorf("GET %s 生产模式应返回通用文案: %s", path, body)
				}
			}

			// 4xx 不受影响：Fiber 的 "Cannot GET /nope" 两种模式都原样返回
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/nope", nil), 10_000)
			if err != nil {
				t.Fatalf("GET /nope: %v", err)
			}
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(raw), "Cannot GET /nope") {
				t.Errorf("GET /nope = %d: %s，期望 404 且保留 Fiber 文案", resp.StatusCode, raw)
			}
		})
	}
}
