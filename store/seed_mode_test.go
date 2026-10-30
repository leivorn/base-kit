package store

import (
	"path/filepath"
	"testing"

	"github.com/leivorn/base-kit/config"
	adminmodel "github.com/leivorn/base-kit/model/admin"

	"gorm.io/gorm"
)

// initWith 用临时 sqlite 跑一次 Init，dsn 为空时新建库。返回库文件路径，方便同一个库再跑一次。
func initWith(t *testing.T, dsn, mode, seed string, opts Options) string {
	t.Helper()
	saved := config.C
	// seedMode 是包级的，别让本测试的模式漏到后面直接调种子函数的测试里
	t.Cleanup(func() { config.C = saved; seedMode = SeedFull })
	if dsn == "" {
		dsn = filepath.Join(t.TempDir(), "seed.db")
	}
	config.C.Database.Driver = "sqlite"
	config.C.Database.DSN = dsn
	config.C.Database.Seed = seed
	config.C.Server.Mode = mode
	if err := Init(opts); err != nil {
		t.Fatalf("Init(seed=%q): %v", seed, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := DB.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return dsn
}

func count(t *testing.T, model any, where ...any) int64 {
	t.Helper()
	q := DB.Model(model).Unscoped()
	if len(where) > 0 {
		q = q.Where(where[0], where[1:]...)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		t.Fatalf("count %T: %v", model, err)
	}
	return n
}

func TestParseSeedMode(t *testing.T) {
	for in, want := range map[string]SeedMode{"": SeedFull, "full": SeedFull, " Insert ": SeedInsert, "OFF": SeedOff} {
		got, err := ParseSeedMode(in)
		if err != nil || got != want {
			t.Errorf("ParseSeedMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseSeedMode("none"); err == nil {
		t.Error("不认识的取值必须报错，不能悄悄按 full 跑")
	}
}

func TestInitRejectsUnknownSeedMode(t *testing.T) {
	saved := config.C
	t.Cleanup(func() { config.C = saved })
	config.C.Database.Driver = "sqlite"
	config.C.Database.DSN = filepath.Join(t.TempDir(), "x.db")
	config.C.Database.Seed = "nope"
	if err := Init(Options{}); err == nil {
		t.Fatal("database.seed 写错时 Init 必须失败")
	}
}

// off：新库上只建表，一行种子都不写；Options.Seed 照样被调用，但 kit 的种子助手什么都不写。
func TestSeedOffWritesNothing(t *testing.T) {
	called := false
	initWith(t, "", "development", "off", Options{Seed: func(*gorm.DB) {
		called = true
		SyncSeedMenus([]MenuDef{{Menu: adminmodel.Menu{Name: "Biz", Path: "/biz", Type: "catalog", Title: "biz", Status: 1}}})
		RefreshRoleMenus()
	}})
	if !called {
		t.Fatal("off 模式也要调用 Options.Seed（模板垫片在里面拿 DB）")
	}
	if CurrentSeedMode() != SeedOff {
		t.Fatalf("CurrentSeedMode = %q", CurrentSeedMode())
	}
	for _, m := range []any{&adminmodel.User{}, &adminmodel.Role{}, &adminmodel.Menu{}, &adminmodel.Dept{}, &adminmodel.Config{}, &adminmodel.RoleMenu{}, &adminmodel.UserRole{}} {
		if n := count(t, m); n != 0 {
			t.Errorf("off 模式写了 %T: %d 行", m, n)
		}
	}
}

// sharedDB 先按 full 建好一个库，再模拟「和别的系统共用」：手改过的菜单、受限的 admin 角色、
// 软删的角色、生产库里叫 admin 的账号、被删掉的种子菜单和配置项。
func sharedDB(t *testing.T) (dsn string, adminMenus int64) {
	t.Helper()
	dsn = initWith(t, "", "development", "", Options{})

	DB.Model(&adminmodel.Menu{}).Where("name = ?", "SystemUser").Update("path", "/custom/user")
	var adminRole adminmodel.Role
	DB.Where("code = ?", "admin").First(&adminRole)
	DB.Where("role_id = ? AND menu_id NOT IN (?)", adminRole.ID,
		DB.Model(&adminmodel.Menu{}).Select("id").Where("name = ?", "SystemUser")).Delete(&adminmodel.RoleMenu{})
	adminMenus = count(t, &adminmodel.RoleMenu{}, "role_id = ?", adminRole.ID)

	legacy := adminmodel.Role{Name: "旧角色", Code: "legacy", Status: 1}
	DB.Create(&legacy)
	DB.Delete(&legacy) // 软删

	var dept adminmodel.Menu
	DB.Where("name = ?", "SystemDept").First(&dept)
	DB.Where("parent_id = ?", dept.ID).Unscoped().Delete(&adminmodel.Menu{})
	DB.Unscoped().Delete(&dept)
	DB.Unscoped().Where("config_key = ?", "site_logo").Delete(&adminmodel.Config{})
	return dsn, adminMenus
}

// off 用在共用库上：再启动一次（生产模式）不能改动任何已有数据，也不补缺。
func TestSeedOffLeavesSharedDataAlone(t *testing.T) {
	dsn, adminMenus := sharedDB(t)
	initWith(t, dsn, "production", "off", Options{})
	assertSharedDataUntouched(t, adminMenus)
	if n := count(t, &adminmodel.Menu{}, "name = ?", "SystemDept"); n != 0 {
		t.Error("off 模式不应补建缺失的菜单")
	}
	if n := count(t, &adminmodel.Config{}, "config_key = ?", "site_logo"); n != 0 {
		t.Error("off 模式不应补建缺失的配置项")
	}
}

// insert：缺的补上，已有的一概不动。
func TestSeedInsertOnlyFillsGaps(t *testing.T) {
	dsn, adminMenus := sharedDB(t)
	initWith(t, dsn, "production", "insert", Options{})
	assertSharedDataUntouched(t, adminMenus)
	if n := count(t, &adminmodel.Menu{}, "name = ?", "SystemDept"); n != 1 {
		t.Error("insert 模式应补建缺失的菜单")
	}
	if n := count(t, &adminmodel.Menu{}, "name = ?", "SystemDeptCreate"); n != 1 {
		t.Error("insert 模式应补建缺失菜单的按钮")
	}
	if n := count(t, &adminmodel.Config{}, "config_key = ?", "site_logo"); n != 1 {
		t.Error("insert 模式应补建缺失的配置项")
	}
}

func assertSharedDataUntouched(t *testing.T, adminMenus int64) {
	t.Helper()
	var menu adminmodel.Menu
	DB.Where("name = ?", "SystemUser").First(&menu)
	if menu.Path != "/custom/user" {
		t.Errorf("已有菜单被按种子改写: path = %q", menu.Path)
	}
	var adminRole adminmodel.Role
	DB.Where("code = ?", "admin").First(&adminRole)
	if n := count(t, &adminmodel.RoleMenu{}, "role_id = ?", adminRole.ID); n != adminMenus {
		t.Errorf("admin 角色被重新授权: %d 个菜单，原来 %d 个", n, adminMenus)
	}
	if n := count(t, &adminmodel.Role{}, "code = ?", "legacy"); n != 1 {
		t.Error("软删的角色被物理删除了")
	}
	if n := count(t, &adminmodel.User{}, "username = ?", "admin"); n != 1 {
		t.Error("生产模式下叫 admin 的账号被删了")
	}
}

// 扩展结构和基底模型同表时，两边的列都要迁移（此前同一次 AutoMigrate 里只迁移后一个结构）。
type opLogExtension struct {
	ID      uint   `gorm:"primarykey"`
	Remarks string `gorm:"type:text"`
}

func (opLogExtension) TableName() string { return "sys_operation_logs" }

func TestInitMigratesExtensionOfBaseTable(t *testing.T) {
	initWith(t, "", "development", "", Options{Models: []any{&opLogExtension{}, &userExtension{}}})
	m := DB.Migrator()
	for _, c := range []struct {
		model any
		col   string
	}{
		{&adminmodel.OperationLog{}, "created_at"},
		{&adminmodel.OperationLog{}, "body"},
		{&opLogExtension{}, "remarks"},
		{&adminmodel.User{}, "username"},
		{&adminmodel.User{}, "password_changed_at"},
		{&userExtension{}, "dept"},
	} {
		if !m.HasColumn(c.model, c.col) {
			t.Errorf("%T 缺列 %s", c.model, c.col)
		}
	}
	// 种子照常：说明 sys_users 的基底列都在
	if n := count(t, &adminmodel.User{}, "username = ?", "super"); n != 1 {
		t.Error("同表扩展登记后种子没跑通")
	}
}
