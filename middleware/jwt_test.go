package middleware

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/leivorn/base-kit/config"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
	"github.com/golang-jwt/jwt/v5"
)

func setupJWTTest(t *testing.T) {
	t.Helper()
	setupPermissionTestDB(t)
	saved := config.C.JWT
	config.C.JWT = config.JWTConfig{
		Secret:        "unit-test-secret-0123456789abcdef0123456789",
		Expire:        time.Hour,
		RefreshExpire: time.Hour,
	}
	t.Cleanup(func() { config.C.JWT = saved })
}

func createTestUser(t *testing.T, username string, status int, roles ...adminmodel.Role) adminmodel.User {
	t.Helper()
	user := adminmodel.User{Username: username, Password: "unused", Status: status, Roles: roles}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return user
}

func createTestRole(t *testing.T, code string, status int) adminmodel.Role {
	t.Helper()
	role := adminmodel.Role{Name: code, Code: code, Status: status}
	if err := store.DB.Create(&role).Error; err != nil {
		t.Fatalf("create role %s: %v", code, err)
	}
	return role
}

// protectedApp 挂 JWTAuth，handler 把 Locals 里的 roles 原样返回，便于断言鉴权上下文。
func protectedApp() *fiber.App {
	app := fiber.New()
	app.Use(JWTAuth())
	app.Get("/whoami", func(c *fiber.Ctx) error {
		roles, _ := c.Locals("roles").([]string)
		return c.JSON(fiber.Map{"username": c.Locals("username"), "roles": roles})
	})
	return app
}

func bearerRequest(t *testing.T, app *fiber.App, method, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp
}

func TestParseTokenRejectsWrongOrMissingType(t *testing.T) {
	setupJWTTest(t)

	access, err := GenerateAccessToken(1, "super", []string{"super"})
	if err != nil {
		t.Fatalf("generate access token: %v", err)
	}
	refresh, err := GenerateRefreshToken(1, "super")
	if err != nil {
		t.Fatalf("generate refresh token: %v", err)
	}

	if _, err := ParseToken(access, TokenTypeAccess); err != nil {
		t.Fatalf("access token as access: unexpected error %v", err)
	}
	if _, err := ParseToken(refresh, TokenTypeRefresh); err != nil {
		t.Fatalf("refresh token as refresh: unexpected error %v", err)
	}
	if _, err := ParseToken(access, TokenTypeRefresh); err == nil {
		t.Fatal("access token must not be accepted as refresh token")
	}
	if _, err := ParseToken(refresh, TokenTypeAccess); err == nil {
		t.Fatal("refresh token must not be accepted as access token")
	}

	// 升级前签发的旧 token 没有 typ，一律拒绝
	legacy := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID:   1,
		Username: "super",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	})
	legacyStr, err := legacy.SignedString([]byte(config.C.JWT.Secret))
	if err != nil {
		t.Fatalf("sign legacy token: %v", err)
	}
	if _, err := ParseToken(legacyStr, TokenTypeAccess); err == nil {
		t.Fatal("token without typ must be rejected")
	}
}

func TestJWTAuthRejectsRefreshTokenOnAccessRoute(t *testing.T) {
	setupJWTTest(t)
	user := createTestUser(t, "alice", 1)

	refresh, _ := GenerateRefreshToken(user.ID, user.Username)
	resp := bearerRequest(t, protectedApp(), http.MethodGet, "/whoami", refresh)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh token on access route: expected 401, got %d", resp.StatusCode)
	}
}

func TestJWTAuthRejectsDisabledOrMissingUser(t *testing.T) {
	setupJWTTest(t)
	disabled := createTestUser(t, "disabled", 0)

	token, _ := GenerateAccessToken(disabled.ID, disabled.Username, nil)
	if resp := bearerRequest(t, protectedApp(), http.MethodGet, "/whoami", token); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled user: expected 401, got %d", resp.StatusCode)
	}

	ghost, _ := GenerateAccessToken(9999, "ghost", nil)
	if resp := bearerRequest(t, protectedApp(), http.MethodGet, "/whoami", ghost); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing user: expected 401, got %d", resp.StatusCode)
	}
}

func TestJWTAuthRejectsTokenIssuedBeforePasswordChange(t *testing.T) {
	setupJWTTest(t)
	user := createTestUser(t, "bob", 1)
	token, _ := GenerateAccessToken(user.ID, user.Username, nil)

	// 改密时间晚于签发时间：旧 token 作废
	after := time.Now().Add(2 * time.Second)
	if err := store.DB.Model(&adminmodel.User{}).Where("id = ?", user.ID).Update("password_changed_at", after).Error; err != nil {
		t.Fatalf("set password_changed_at: %v", err)
	}
	InvalidateUserAuthCache(user.ID)
	if resp := bearerRequest(t, protectedApp(), http.MethodGet, "/whoami", token); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token issued before password change: expected 401, got %d", resp.StatusCode)
	}

	// 改密时间早于签发时间：正常放行
	before := time.Now().Add(-2 * time.Second)
	if err := store.DB.Model(&adminmodel.User{}).Where("id = ?", user.ID).Update("password_changed_at", before).Error; err != nil {
		t.Fatalf("set password_changed_at: %v", err)
	}
	InvalidateUserAuthCache(user.ID)
	if resp := bearerRequest(t, protectedApp(), http.MethodGet, "/whoami", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("token issued after password change: expected 200, got %d", resp.StatusCode)
	}
}

// token 里的 roles claim 只是快照，鉴权必须以数据库当前的启用角色为准。
func TestJWTAuthPopulatesRolesFromDBIgnoringClaim(t *testing.T) {
	setupJWTTest(t)
	createTestUser(t, "super", 1) // 占住 id=1，被测用户不能是内置超管
	admin := createTestRole(t, "admin", 1)
	auditor := createTestRole(t, "auditor", 0) // 已禁用，不应出现
	user := createTestUser(t, "carol", 1, admin, auditor)

	forged, _ := GenerateAccessToken(user.ID, user.Username, []string{"super"})

	resp := bearerRequest(t, protectedApp(), http.MethodGet, "/whoami", forged)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body struct {
		Username string   `json:"username"`
		Roles    []string `json:"roles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Username != "carol" || !reflect.DeepEqual(body.Roles, []string{"admin"}) {
		t.Fatalf("expected roles [admin] from DB, got %+v", body)
	}

	// 伪造的 super claim 对权限判定无效
	app := fiber.New()
	app.Use(JWTAuth(), PermissionAuth())
	app.Delete("/admin/system/user/:id", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	if resp := bearerRequest(t, app, http.MethodDelete, "/admin/system/user/2", forged); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged super claim: expected 403, got %d", resp.StatusCode)
	}
}

func TestJWTAuthCacheInvalidation(t *testing.T) {
	setupJWTTest(t)
	user := createTestUser(t, "dave", 1)
	token, _ := GenerateAccessToken(user.ID, user.Username, nil)

	if resp := bearerRequest(t, protectedApp(), http.MethodGet, "/whoami", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("initial: expected 200, got %d", resp.StatusCode)
	}

	if err := store.DB.Model(&adminmodel.User{}).Where("id = ?", user.ID).Update("status", 0).Error; err != nil {
		t.Fatalf("disable user: %v", err)
	}
	// 缓存未失效前仍放行（TTL 内）
	if resp := bearerRequest(t, protectedApp(), http.MethodGet, "/whoami", token); resp.StatusCode != http.StatusOK {
		t.Fatalf("cached: expected 200, got %d", resp.StatusCode)
	}
	InvalidateUserAuthCache(user.ID)
	if resp := bearerRequest(t, protectedApp(), http.MethodGet, "/whoami", token); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after invalidation: expected 401, got %d", resp.StatusCode)
	}
}
