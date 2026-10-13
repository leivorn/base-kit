package middleware

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	adminmodel "github.com/leivorn/base-kit/model/admin"

	"github.com/gofiber/fiber/v2"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// captureOperationLogs 让 OperationLog 中间件把条目交给测试，而不是由后台 writer 异步写进 store.DB。
func captureOperationLogs(t *testing.T) <-chan adminmodel.OperationLog {
	t.Helper()
	opLogOnce.Do(func() {}) // 本测试进程里不再启动真实 writer
	saved := opLogCh
	ch := make(chan adminmodel.OperationLog, 16)
	opLogCh = ch
	t.Cleanup(func() { opLogCh = saved })
	return ch
}

// operationLogApp 挂上 OperationLog，任何路径都直接返回 200。
func operationLogApp() *fiber.App {
	app := fiber.New()
	app.Use(OperationLog())
	app.All("/*", func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
	return app
}

// recordOperationLog 发一个请求并取出它产生的日志条目。app.Test 等 handler 链跑完才返回，条目此时已入队。
func recordOperationLog(t *testing.T, app *fiber.App, logs <-chan adminmodel.OperationLog, req *http.Request) adminmodel.OperationLog {
	t.Helper()
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	resp.Body.Close()
	select {
	case entry := <-logs:
		return entry
	default:
		t.Fatalf("%s %s 没有产生操作日志", req.Method, req.URL.Path)
		return adminmodel.OperationLog{}
	}
}

type formFile struct {
	field, filename, contentType, content string
}

func multipartRequest(t *testing.T, target string, fields [][2]string, files []formFile) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	for _, f := range files {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, f.field, f.filename))
		h.Set(fiber.HeaderContentType, f.contentType)
		part, err := w.CreatePart(h)
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := part.Write([]byte(f.content)); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(fiber.MethodPost, target, &buf)
	req.Header.Set(fiber.HeaderContentType, w.FormDataContentType())
	return req
}

// 16 字节，含 0x89 与 NUL：MySQL utf8mb4 列拒收前者，PG 的 text 拒收后者
const pngHeader = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"

func TestOperationLogSummarizesMultipartUpload(t *testing.T) {
	logs := captureOperationLogs(t)

	var uploaded string
	app := fiber.New()
	app.Use(OperationLog())
	app.Post("/upload", func(c *fiber.Ctx) error {
		// 中间件已经解析过表单，handler 照常能取到文件
		fh, err := c.FormFile("avatar")
		if err != nil {
			return err
		}
		uploaded = fh.Filename
		return c.SendStatus(fiber.StatusOK)
	})

	req := multipartRequest(t, "/upload",
		[][2]string{{"name", "张三"}, {"password", "p@ss"}, {"data", `{"secret":"s3cr3t","title":"封面"}`}},
		[]formFile{
			{"avatar", "头像&封面.png", "image/png", pngHeader},
			{"docs", "a.txt", "text/plain", "a"},
			{"docs", "b.txt", "text/plain", "bb"},
		})
	entry := recordOperationLog(t, app, logs, req)

	if entry.Status != fiber.StatusOK || uploaded != "头像&封面.png" {
		t.Fatalf("handler 没能正常处理上传: status=%d uploaded=%q", entry.Status, uploaded)
	}
	want := `{"avatar":{"filename":"头像&封面.png","size":16,"contentType":"image/png"},` +
		`"data":"{\"secret\":\"***\",\"title\":\"封面\"}",` +
		`"docs":[{"filename":"a.txt","size":1,"contentType":"text/plain"},{"filename":"b.txt","size":2,"contentType":"text/plain"}],` +
		`"name":"张三","password":"***"}`
	if entry.Body != want {
		t.Errorf("body =\n%s\nwant\n%s", entry.Body, want)
	}
}

type opLogCase struct {
	name     string
	req      *http.Request
	wantBody string
}

// storageHostileRequests 是之前在 SQLite 上一切正常、在 MySQL 上整条 INSERT 失败而丢日志的请求。
// 请求体只能读一次，每个用例现取。
func storageHostileRequests(t *testing.T) []opLogCase {
	t.Helper()

	// 前缀 11 字节 + 2036 个 a，第 2048 字节正好落在第一个「中」（3 字节）中间
	longCJK := `{"remark":"` + strings.Repeat("a", 2036) + strings.Repeat("中", 10) + `"}`
	cjk := httptest.NewRequest(fiber.MethodPost, "/notice", strings.NewReader(longCJK))
	cjk.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

	binary := httptest.NewRequest(fiber.MethodPut, "/file/raw", strings.NewReader(pngHeader+"\xff"))
	binary.Header.Set(fiber.HeaderContentType, fiber.MIMEOctetStream)

	long := httptest.NewRequest(fiber.MethodPost, "/"+strings.Repeat("p", 300), strings.NewReader(`{}`))
	long.Header.Set(fiber.HeaderUserAgent, strings.Repeat("Mozilla/5.0 ", 60))

	return []opLogCase{
		{
			name:     "multipart upload",
			req:      multipartRequest(t, "/upload", [][2]string{{"name", "张三"}}, []formFile{{"file", "a.png", "image/png", pngHeader}}),
			wantBody: `{"file":{"filename":"a.png","size":16,"contentType":"image/png"},"name":"张三"}`,
		},
		{name: "json cut inside a multibyte char", req: cjk, wantBody: longCJK[:2047] + "...[truncated]"},
		{name: "binary body", req: binary, wantBody: "\uFFFDPNG\r\n\x1a\n\rIHDR\uFFFD"},
		{name: "path and user agent over column size", req: long, wantBody: `{}`},
	}
}

func TestOperationLogEntriesAreStorable(t *testing.T) {
	logs := captureOperationLogs(t)
	app := operationLogApp()

	for _, tc := range storageHostileRequests(t) {
		entry := recordOperationLog(t, app, logs, tc.req)
		if entry.Body != tc.wantBody {
			t.Errorf("%s: body = %q, want %q", tc.name, entry.Body, tc.wantBody)
		}
		for column, v := range map[string]string{
			"method": entry.Method, "path": entry.Path, "ip": entry.IP,
			"user_agent": entry.UserAgent, "username": entry.Username, "body": entry.Body,
		} {
			if !utf8.ValidString(v) || strings.Contains(v, "\x00") {
				t.Errorf("%s: %s 含非法 UTF-8 或 NUL: %q", tc.name, column, v)
			}
		}
		if len(entry.Path) > opLogPathSize || len(entry.UserAgent) > opLogUserAgentSize {
			t.Errorf("%s: 超出列宽 path=%d user_agent=%d", tc.name, len(entry.Path), len(entry.UserAgent))
		}
	}
}

// TestOperationLogEntryOutlivesRequestBuffers 锁定入队前拷贝：条目由后台 writer 异步写库，
// 而 Fiber 默认返回的字符串指向会被后续请求复用的缓冲区。
func TestOperationLogEntryOutlivesRequestBuffers(t *testing.T) {
	logs := captureOperationLogs(t)
	app := operationLogApp()

	first := httptest.NewRequest(fiber.MethodDelete, "/system/user/1", nil)
	first.Header.Set(fiber.HeaderUserAgent, "agent-first")
	entry := recordOperationLog(t, app, logs, first)

	for range 3 {
		next := httptest.NewRequest(fiber.MethodPost, "/system/role/2", strings.NewReader(`{}`))
		next.Header.Set(fiber.HeaderUserAgent, "agent-next")
		recordOperationLog(t, app, logs, next)
	}

	if entry.Method != fiber.MethodDelete || entry.Path != "/system/user/1" || entry.UserAgent != "agent-first" {
		t.Errorf("条目被后续请求改写: method=%q path=%q user_agent=%q", entry.Method, entry.Path, entry.UserAgent)
	}
}

func TestOperationLogColumnLimitsMatchModel(t *testing.T) {
	s, err := schema.Parse(&adminmodel.OperationLog{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	for field, limit := range map[string]int{
		"Method": opLogMethodSize, "Username": opLogUsernameSize, "Path": opLogPathSize,
		"IP": opLogIPSize, "UserAgent": opLogUserAgentSize,
	} {
		if size := s.LookUpField(field).Size; size != limit {
			t.Errorf("OperationLog.%s 列宽是 %d，中间件按 %d 截断，两处要一起改", field, size, limit)
		}
	}
}

// TestMySQLOperationLogEntriesInsert 把 storageHostileRequests 产生的条目真正写进 MySQL 再读回。
// 这几类请求之前在 MySQL 严格模式下整条 INSERT 失败（1366 非法字符 / 1406 超长），writer 只打一行日志。
//
// 没有 BASEKIT_TEST_MYSQL_DSN 时跳过，本机开发不受影响。
func TestMySQLOperationLogEntriesInsert(t *testing.T) {
	dsn := os.Getenv("BASEKIT_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 BASEKIT_TEST_MYSQL_DSN，跳过 MySQL 操作日志写入测试")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("connect mysql: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.OperationLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	logs := captureOperationLogs(t)
	app := operationLogApp()

	for _, tc := range storageHostileRequests(t) {
		entry := recordOperationLog(t, app, logs, tc.req)
		if err := db.Create(&entry).Error; err != nil {
			t.Errorf("%s: 写入 MySQL 失败: %v", tc.name, err)
			continue
		}
		t.Cleanup(func() { db.Delete(&adminmodel.OperationLog{}, entry.ID) })

		var got adminmodel.OperationLog
		if err := db.First(&got, entry.ID).Error; err != nil {
			t.Fatalf("%s: 读回: %v", tc.name, err)
		}
		if got.Body != entry.Body || got.Path != entry.Path || got.UserAgent != entry.UserAgent {
			t.Errorf("%s: 读回的内容与写入不一致", tc.name)
		}
	}
}
