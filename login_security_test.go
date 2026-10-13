package basekit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
	"golang.org/x/crypto/bcrypt"
)

func newTestApp(t *testing.T, mode string) *fiber.App {
	t.Helper()
	app, err := NewApp(Options{Config: testConfig(t, mode)})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := store.DB.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return app
}

func setLoginCaptcha(t *testing.T, on bool) {
	t.Helper()
	value := "false"
	if on {
		value = "true"
	}
	if err := store.DB.Model(&adminmodel.Config{}).Where("config_key = ?", "login_captcha").Update("config_value", value).Error; err != nil {
		t.Fatalf("设置 login_captcha: %v", err)
	}
}

func postLogin(t *testing.T, app *fiber.App, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/auth/login", strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req, 10_000)
	if err != nil {
		t.Fatalf("登录: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type captchaData struct {
	Enabled   bool   `json:"enabled"`
	CaptchaID string `json:"captchaId"`
	Image     string `json:"image"`
	Piece     string `json:"piece"`
	PieceSize int    `json:"pieceSize"`
	Width     int    `json:"width"`
}

func getCaptcha(t *testing.T, app *fiber.App) captchaData {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/admin/auth/captcha", nil), 10_000)
	if err != nil {
		t.Fatalf("获取验证码: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/auth/captcha = %d", resp.StatusCode)
	}
	if cc := resp.Header.Get(fiber.HeaderCacheControl); cc != "no-store" {
		t.Errorf("验证码接口 Cache-Control = %q，期望 no-store", cc)
	}
	var body struct {
		Data captchaData `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析验证码响应: %v", err)
	}
	return body.Data
}

func postJSONBody(t *testing.T, app *fiber.App, path, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req, 10_000)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// 验证码接口无需登录；login_captcha 开启后没有拼图通过凭证一律 400，且不去校验口令。
// 拖对拼图拿凭证的完整路径在 service/admin 的测试里（答案只在服务端，这里拿不到）。
func TestLoginCaptchaGate(t *testing.T) {
	app := newTestApp(t, "development")

	if d := getCaptcha(t, app); d.Enabled || d.CaptchaID != "" {
		t.Fatalf("开发库默认不开验证码，得到 %+v", d)
	}
	loginToken(t, app, "super", "123456")

	setLoginCaptcha(t, true)
	d := getCaptcha(t, app)
	if !d.Enabled || d.CaptchaID == "" || !strings.HasPrefix(d.Image, "data:image/jpeg;base64,") ||
		!strings.HasPrefix(d.Piece, "data:image/png;base64,") || d.PieceSize == 0 || d.Width == 0 {
		t.Fatalf("开启后应返回拼图，得到 enabled=%v id=%q pieceSize=%d width=%d", d.Enabled, d.CaptchaID, d.PieceSize, d.Width)
	}
	if status := postLogin(t, app, `{"username":"super","password":"123456"}`); status != http.StatusBadRequest {
		t.Errorf("不带凭证登录 = %d，期望 400", status)
	}
	if status := postLogin(t, app, `{"username":"super","password":"123456","captchaToken":"forged"}`); status != http.StatusBadRequest {
		t.Errorf("伪造凭证登录 = %d，期望 400", status)
	}
	// 拼图块原地不动（x=0）一定不在缺口上：缺口至少离起点一个身位
	if status := postJSONBody(t, app, "/admin/auth/captcha/verify", `{"captchaId":"`+d.CaptchaID+`","x":0}`); status != http.StatusBadRequest {
		t.Errorf("拼图位置错误 = %d，期望 400", status)
	}
	if status := postJSONBody(t, app, "/admin/auth/captcha/verify", `{"x":10}`); status != http.StatusBadRequest {
		t.Errorf("缺 captchaId = %d，期望 400", status)
	}

	setLoginCaptcha(t, false)
	loginToken(t, app, "super", "123456")
}

// 生产模式：密码正确但不达标的存量账号登录不进去（与密码错误同一个 403），合格的照常登录。
func TestProductionRejectsWeakPasswordLogin(t *testing.T) {
	app := newTestApp(t, "production")
	setLoginCaptcha(t, false) // 生产新库默认开验证码，这里只测口令

	create := func(username, password string) {
		t.Helper()
		hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
		if err := store.DB.Create(&adminmodel.User{Username: username, Password: string(hash), Status: 1}).Error; err != nil {
			t.Fatalf("建用户 %s: %v", username, err)
		}
	}
	create("opsweak", "abcd1234")
	create("opsstrong", "Np7xQ2wLs9")

	if status := postLogin(t, app, `{"username":"opsweak","password":"abcd1234"}`); status != http.StatusForbidden {
		t.Errorf("生产弱口令登录 = %d，期望 403", status)
	}
	loginToken(t, app, "opsstrong", "Np7xQ2wLs9")
}

// reset-password 给超管换一个随机强口令并打印出来，生产模式能直接用它登录。
func TestResetPasswordCommand(t *testing.T) {
	cfg := testConfig(t, "production")
	t.Cleanup(func() {
		if sqlDB, err := store.DB.DB(); err == nil {
			sqlDB.Close()
		}
	})

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	err = resetPassword(Options{Config: cfg}, []string{"super"})
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatalf("resetPassword: %v", err)
	}
	m := regexp.MustCompile(`密码已重置为: (\S+)`).FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("输出里没有新密码:\n%s", out)
	}

	var super adminmodel.User
	store.DB.Where("username = ?", "super").First(&super)
	if bcrypt.CompareHashAndPassword([]byte(super.Password), []byte(m[1])) != nil {
		t.Fatal("库里的哈希与打印的新密码对不上")
	}
	if super.PasswordChangedAt == nil {
		t.Fatal("重置密码要记录改密时间，让旧 token 失效")
	}

	if err := resetPassword(Options{Config: cfg}, []string{"nobody"}); err == nil {
		t.Fatal("不存在的用户应报错")
	}
}
