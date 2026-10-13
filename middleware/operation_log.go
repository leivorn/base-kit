package middleware

import (
	"encoding/json"
	"log"
	"mime/multipart"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/leivorn/base-kit/config"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
)

// sensitiveBodyKeys 是写操作日志前要脱敏的请求字段（密码/密钥等）。
// 之前只按 path 含 "login" 整体 [REDACTED]，漏掉了创建/编辑用户（password）与修改密码
// （oldPassword/newPassword）等路径，会把明文口令写进 operation_logs.body（该表可被持
// System:OperationLog:List 的管理员读取）。改为按字段脱敏：命中键名的值统一替换为 "***"，
// 其余审计字段保留可读。
var sensitiveBodyKeys = []string{"password", "oldPassword", "newPassword", "confirmPassword", "appSecret", "app_secret", "apiV3Key", "api_v3_key", "privateKey", "private_key", "secret"}

// sensitiveBodyRe 匹配请求体 JSON 里敏感字段的值。
var sensitiveBodyRe = regexp.MustCompile(`("(?:` + strings.Join(sensitiveBodyKeys, "|") + `)"\s*:\s*)"(?:[^"\\]|\\.)*"`)

func redactSensitiveBody(body string) string {
	if body == "" {
		return body
	}
	return sensitiveBodyRe.ReplaceAllString(body, `${1}"***"`)
}

// 各列的长度上限，与 adminmodel.OperationLog 的 size 标签一致（TestOperationLogColumnLimitsMatchModel 锁定）。
// MySQL 严格模式下超长值会让整条 INSERT 失败（Error 1406），这条日志就丢了。
const (
	opLogMethodSize    = 10
	opLogUsernameSize  = 64
	opLogPathSize      = 256
	opLogIPSize        = 64
	opLogUserAgentSize = 512
	// 请求体只记前 2KB
	opLogBodyLimit = 2048
)

// logText 把要进操作日志的字符串整理成各数据库都收的样子，所有列入队前都要过一遍：
//   - 按字节截到 limit，但不切开多字节字符。varchar(N) 在 MySQL/PG 里按字符计，N 字节一定放得下。
//   - 非法 UTF-8 换成 U+FFFD。MySQL 的 utf8mb4 列（严格模式）和 PG 会拒绝整条 INSERT（Error 1366），
//     上传文件的二进制请求体、在 2KB 处被切开的中文都因此丢过日志，SQLite 上则一切正常。
//   - 去掉 NUL，PG 的 text 不收 0x00。
//   - 返回副本。Fiber 默认不开 Immutable，c.Path()、c.Get() 返回的字符串指向会被后续请求复用的缓冲区，
//     而日志是后台 writer 异步写库的：不拷贝就会记成别的请求的 method/path（实测 DELETE 被记成 "POSTTE"）。
func logText(s string, limit int) string {
	if len(s) > limit {
		cut := limit
		// UTF-8 字符最长 4 字节，最多往回退 3 个字节就到字符起始位置
		for cut > 0 && cut > limit-utf8.UTFMax+1 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.Clone(s)
}

// operationLogBody 取要记进日志的请求体：脱敏，截到 opLogBodyLimit。
func operationLogBody(c *fiber.Ctx) string {
	var body string
	// 上传文件（multipart/form-data）的原始请求体是二进制，记下来读不了、截断后还可能漏掉排在文件后面的字段，
	// 改记表单摘要。fasthttp 会缓存解析结果，handler 里再调 c.FormFile 不会重复解析。
	if form, err := c.MultipartForm(); err == nil {
		body = multipartSummary(form)
	} else {
		body = redactSensitiveBody(string(c.Body()))
	}
	if len(body) > opLogBodyLimit {
		return logText(body, opLogBodyLimit) + "...[truncated]"
	}
	return logText(body, opLogBodyLimit)
}

type uploadedFileLog struct {
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
}

// multipartSummary 把 multipart 表单整理成 JSON：普通字段照录（敏感字段脱敏），
// 文件只记文件名、大小和类型；同名多值记成数组。
func multipartSummary(form *multipart.Form) string {
	fields := make(map[string][]any, len(form.Value)+len(form.File))
	for key, values := range form.Value {
		for _, v := range values {
			if slices.Contains(sensitiveBodyKeys, key) {
				v = "***"
			} else {
				// 前端常把对象 JSON.stringify 后作为一个字段和文件一起提交，里面的敏感字段同样要脱敏
				v = redactSensitiveBody(v)
			}
			fields[key] = append(fields[key], v)
		}
	}
	for key, files := range form.File {
		for _, fh := range files {
			fields[key] = append(fields[key], uploadedFileLog{
				Filename:    fh.Filename,
				Size:        fh.Size,
				ContentType: fh.Header.Get(fiber.HeaderContentType),
			})
		}
	}

	summary := make(map[string]any, len(fields))
	for key, list := range fields {
		if len(list) == 1 {
			summary[key] = list[0]
		} else {
			summary[key] = list
		}
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // 文件名里的 & < > 保持原样
	if err := enc.Encode(summary); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

var (
	opLogOnce sync.Once
	opLogCh   chan adminmodel.OperationLog
)

// startOpLogWriter 用单个后台 writer 串行写日志。
// 之前是每请求 `go DB.Create` 且丢弃错误：SQLite 上并发插库互相争锁
// （实测 100 并发写 INSERT 峰值 200ms+，还拖慢同期读请求），goroutine 数量也无上限。
// channel 满时丢弃该条并打日志，绝不阻塞请求。
func startOpLogWriter() {
	opLogCh = make(chan adminmodel.OperationLog, 1024)
	go func() {
		for entry := range opLogCh {
			if err := store.DB.Create(&entry).Error; err != nil {
				log.Printf("[operation-log] write failed: %v", err)
			}
		}
	}()
	// 配置在这里读好再传进去：goroutine 里读 config.C 会和下一次 Bootstrap 重写它竞争（测试里多次 NewApp）
	go opLogCleanupLoop(config.C.Server.OpLogRetentionDays)
}

// opLogCleanupLoop 每天清理一次超过保留天数的日志；
// server.op_log_retention_days <= 0 时永久保留（默认行为，与历史一致）。
func opLogCleanupLoop(days int) {
	if days <= 0 {
		return
	}
	for {
		cutoff := time.Now().AddDate(0, 0, -days)
		res := store.DB.Where("created_at < ?", cutoff).Delete(&adminmodel.OperationLog{})
		if res.Error != nil {
			log.Printf("[operation-log] cleanup failed: %v", res.Error)
		} else if res.RowsAffected > 0 {
			log.Printf("[operation-log] cleaned %d entries older than %d days", res.RowsAffected, days)
		}
		time.Sleep(24 * time.Hour)
	}
}

// OperationLog 记录写操作（GET / OPTIONS 和 /swagger 不记），由后台 writer 异步写库。
//
// kit 已把它挂在 /admin 前缀上（见 router.SetupAdmin），/admin 下的路由不用再挂；
// /api 等其他前缀、PreRoutes 里的路由需要时自己挂。同一请求只记一条：外层已经接手时，
// 再次经过直接放行，见 request_once.go。
func OperationLog() fiber.Handler {
	opLogOnce.Do(startOpLogWriter)
	return func(c *fiber.Ctx) error {
		if c.Locals(operationLogStarted) != nil {
			return c.Next()
		}
		c.Locals(operationLogStarted, true)

		if c.Method() == "GET" || c.Method() == "OPTIONS" {
			return c.Next()
		}

		path := c.Path()
		if strings.HasPrefix(path, "/swagger") {
			return c.Next()
		}

		start := time.Now()
		body := operationLogBody(c)

		err := c.Next()

		duration := time.Since(start).Milliseconds()

		// handler 返回 error 时 Fiber 的 ErrorHandler 尚未运行，
		// c.Response() 里还是默认 200，必须从 err 推导真实状态码，
		// 否则所有失败操作都会被记成"成功"（实测 400 全记成 200）
		status := c.Response().StatusCode()
		if err != nil {
			if e, ok := err.(*fiber.Error); ok {
				status = e.Code
			} else {
				status = fiber.StatusInternalServerError
			}
		}

		userID, _ := c.Locals("userId").(uint)
		username, _ := c.Locals("username").(string)
		if username == "" {
			username = "-"
		}

		entry := adminmodel.OperationLog{
			UserID:    userID,
			Username:  logText(username, opLogUsernameSize),
			Method:    logText(c.Method(), opLogMethodSize),
			Path:      logText(path, opLogPathSize),
			Status:    status,
			Duration:  duration,
			IP:        logText(c.IP(), opLogIPSize),
			UserAgent: logText(c.Get(fiber.HeaderUserAgent), opLogUserAgentSize),
			Body:      body,
		}

		select {
		case opLogCh <- entry:
		default:
			log.Printf("[operation-log] buffer full, entry dropped: %s %s", entry.Method, entry.Path)
		}

		return err
	}
}
