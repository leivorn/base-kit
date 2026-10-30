package middleware

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v2"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type remarksKey struct{}

// useOperationLogExtra 登记一个扩展列取值函数，测试结束后撤掉（登记表是包级的）。
func useOperationLogExtra(t *testing.T, fn OperationLogExtraFunc) {
	t.Helper()
	saved := opLogExtras
	opLogExtras = append(append([]OperationLogExtraFunc(nil), saved...), fn)
	t.Cleanup(func() { opLogExtras = saved })
}

// 扩展列在 handler 之后取值，能拿到 handler 放进 Locals 的数据和响应体；基底字段和非法列名不能被覆盖。
func TestOperationLogExtraColumns(t *testing.T) {
	logs := captureOperationLogs(t)
	useOperationLogExtra(t, func(c *fiber.Ctx, cols map[string]any) {
		if remarks, ok := c.Locals(remarksKey{}).(string); ok {
			cols["remarks"] = remarks
		}
		cols["resp"] = c.Response().Body()
		cols["path"] = "/forged"         // 基底字段，忽略
		cols["bad name; drop"] = "x"     // 非法列名，忽略
		cols["bad\x00text"] = "\xff\x00" // 同上
	})

	app := fiber.New()
	app.Use(OperationLog())
	app.Put("/orders/:id", func(c *fiber.Ctx) error {
		c.Locals(remarksKey{}, "订单迁移：1 → 2")
		return c.JSON(fiber.Map{"code": 0})
	})
	if _, err := app.Test(httptest.NewRequest("PUT", "/orders/1", strings.NewReader(`{}`))); err != nil {
		t.Fatal(err)
	}
	entry := <-logs
	if entry.Path != "/orders/1" {
		t.Errorf("基底字段被扩展列覆盖: path = %q", entry.Path)
	}
	if entry.extra["remarks"] != "订单迁移：1 → 2" {
		t.Errorf("remarks = %v", entry.extra["remarks"])
	}
	if entry.extra["resp"] != `{"code":0}` {
		t.Errorf("resp 应为响应体的字符串拷贝, 得到 %#v", entry.extra["resp"])
	}
	for name := range entry.extra {
		if name != "remarks" && name != "resp" {
			t.Errorf("不该入库的列 %q", name)
		}
	}
}

// 没登记取值函数时不带扩展列，写库仍走原来的结构体路径。
func TestOperationLogWithoutExtraHasNoColumns(t *testing.T) {
	logs := captureOperationLogs(t)
	app := operationLogApp()
	if _, err := app.Test(httptest.NewRequest("POST", "/x", nil)); err != nil {
		t.Fatal(err)
	}
	if entry := <-logs; entry.extra != nil {
		t.Errorf("extra = %v, want nil", entry.extra)
	}
}

type opLogExt struct {
	ID      uint   `gorm:"primarykey"`
	Remarks string `gorm:"type:text"`
	Resp    string `gorm:"type:text"`
}

func (opLogExt) TableName() string { return "sys_operation_logs" }

// writer 把扩展列和基底字段写进同一行；扩展列写不进去（表上没有这一列）时退回只写基底字段，日志不丢。
func TestWriteOperationLogWithExtraColumns(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/oplog.db"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&adminmodel.OperationLog{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&opLogExt{}); err != nil {
		t.Fatal(err)
	}
	saved := store.DB
	store.DB = db
	t.Cleanup(func() {
		store.DB = saved
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})

	base := adminmodel.OperationLog{UserID: 7, Username: "alice", Method: "PUT", Path: "/a", Status: 200, Body: "{}"}
	if err := writeOperationLog(opLogEntry{OperationLog: base, extra: map[string]any{"remarks": "备注", "resp": "ok"}}); err != nil {
		t.Fatalf("write: %v", err)
	}
	var row struct {
		adminmodel.OperationLog
		Remarks string
		Resp    string
	}
	if err := db.Table("sys_operation_logs").Where("path = ?", "/a").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Remarks != "备注" || row.Resp != "ok" || row.Username != "alice" || row.UserID != 7 || row.CreatedAt.IsZero() {
		t.Errorf("同一行里的基底字段或扩展列不对: %+v", row)
	}

	base.Path = "/b"
	if err := writeOperationLog(opLogEntry{OperationLog: base, extra: map[string]any{"no_such_column": "x"}}); err != nil {
		t.Fatalf("扩展列写失败时应退回只写基底字段: %v", err)
	}
	var n int64
	db.Model(&adminmodel.OperationLog{}).Where("path = ?", "/b").Count(&n)
	if n != 1 {
		t.Errorf("退回写入后应有 1 行，得到 %d", n)
	}
}

// TestMySQLOperationLogExtraColumns 在 MySQL 上验证 map 写入（created_at 自己填、扩展列同一行）。
// 没有 BASEKIT_TEST_MYSQL_DSN 时跳过。
func TestMySQLOperationLogExtraColumns(t *testing.T) {
	dsn := os.Getenv("BASEKIT_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 BASEKIT_TEST_MYSQL_DSN，跳过 MySQL 操作日志扩展列测试")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("connect mysql: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.OperationLog{}); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&opLogExt{}); err != nil {
		t.Fatal(err)
	}
	saved := store.DB
	store.DB = db
	t.Cleanup(func() { store.DB = saved })

	path := "/mysql-extra-" + strings.ReplaceAll(t.Name(), "/", "-")
	t.Cleanup(func() { db.Where("path = ?", path).Delete(&adminmodel.OperationLog{}) })
	entry := opLogEntry{
		OperationLog: adminmodel.OperationLog{Username: "alice", Method: "PUT", Path: path, Status: 200},
		extra:        map[string]any{"remarks": strings.Repeat("备注", 600), "resp": `{"code":0}`},
	}
	if err := writeOperationLog(entry); err != nil {
		t.Fatalf("write: %v", err)
	}
	var row opLogExt
	if err := db.Table("sys_operation_logs").Where("path = ?", path).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Remarks != strings.Repeat("备注", 600) || row.Resp != `{"code":0}` {
		t.Errorf("扩展列读回不一致: remarks=%d 字节 resp=%q", len(row.Remarks), row.Resp)
	}
}
