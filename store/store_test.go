package store

import (
	"github.com/leivorn/base-kit/config"
	adminmodel "github.com/leivorn/base-kit/model/admin"

	"reflect"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func TestDialectorSupportsConfiguredDrivers(t *testing.T) {
	tests := []struct {
		name     string
		driver   string
		dsn      string
		wantType string
	}{
		{name: "empty driver defaults to sqlite", driver: "", dsn: "file::memory:?cache=shared", wantType: "*sqlite.Dialector"},
		{name: "sqlite", driver: "sqlite", dsn: "file::memory:?cache=shared", wantType: "*sqlite.Dialector"},
		{name: "sqlite3 alias", driver: "sqlite3", dsn: "file::memory:?cache=shared", wantType: "*sqlite.Dialector"},
		{name: "mysql", driver: "mysql", dsn: "user:pass@tcp(localhost:3306)/app", wantType: "*mysql.Dialector"},
		{name: "mariadb alias", driver: "mariadb", dsn: "user:pass@tcp(localhost:3306)/app", wantType: "*mysql.Dialector"},
		{name: "postgres", driver: "postgres", dsn: "host=localhost user=postgres dbname=app", wantType: "*postgres.Dialector"},
		{name: "postgresql alias", driver: "postgresql", dsn: "host=localhost user=postgres dbname=app", wantType: "*postgres.Dialector"},
		{name: "pgsql alias", driver: "pgsql", dsn: "host=localhost user=postgres dbname=app", wantType: "*postgres.Dialector"},
		{name: "sqlserver", driver: "sqlserver", dsn: "sqlserver://user:pass@localhost:1433?database=app", wantType: "*sqlserver.Dialector"},
		{name: "mssql alias", driver: "mssql", dsn: "sqlserver://user:pass@localhost:1433?database=app", wantType: "*sqlserver.Dialector"},
		{name: "normalizes case and whitespace", driver: " PostgreSQL ", dsn: "host=localhost user=postgres dbname=app", wantType: "*postgres.Dialector"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dial, err := dialector(tt.driver, tt.dsn)
			if err != nil {
				t.Fatalf("dialector() error = %v", err)
			}
			if got := reflect.TypeOf(dial).String(); got != tt.wantType {
				t.Fatalf("dialector() type = %s, want %s", got, tt.wantType)
			}
		})
	}
}

func TestDialectorRejectsUnsupportedDriver(t *testing.T) {
	_, err := dialector("oracle", "")
	if err == nil {
		t.Fatal("dialector() expected unsupported driver error")
	}
	if !strings.Contains(err.Error(), "supported drivers") {
		t.Fatalf("dialector() error = %q, want supported drivers hint", err.Error())
	}
}

func TestSeedMenusBackfillsAuthCodeForExistingMenus(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.Role{}, &adminmodel.Menu{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	DB = db

	system := adminmodel.Menu{Name: "System", Path: "/system", Type: "catalog", Title: "system.title", Status: 1}
	if err := DB.Create(&system).Error; err != nil {
		t.Fatalf("create system menu: %v", err)
	}
	roleMenu := adminmodel.Menu{Name: "SystemRole", Path: "/system/role", Component: "/system/role/list", Type: "menu", Title: "system.role.title", Status: 1}
	if err := DB.Create(&roleMenu).Error; err != nil {
		t.Fatalf("create role menu: %v", err)
	}

	seedMenus()

	var updated adminmodel.Menu
	if err := DB.Where("name = ?", "SystemRole").First(&updated).Error; err != nil {
		t.Fatalf("load updated role menu: %v", err)
	}
	if updated.AuthCode != "System:Role:List" {
		t.Fatalf("expected SystemRole auth code to be backfilled, got %q", updated.AuthCode)
	}
	if updated.ParentID != system.ID {
		t.Fatalf("expected SystemRole parent id %d, got %d", system.ID, updated.ParentID)
	}
}

// menuMeta 是 Menu 的 Vben meta 装饰字段，整组比较用。
type menuMeta struct {
	ActiveIcon, ActivePath, BadgeType, Badge, BadgeVariants     string
	HideInMenu, HideInBreadcrumb, HideInTab, HideChildrenInMenu bool
}

func loadMenuMeta(t *testing.T, name string) menuMeta {
	t.Helper()
	var m adminmodel.Menu
	if err := DB.Where("name = ?", name).First(&m).Error; err != nil {
		t.Fatalf("load menu %s: %v", name, err)
	}
	return menuMeta{
		ActiveIcon: m.ActiveIcon, ActivePath: m.ActivePath, BadgeType: m.BadgeType, Badge: m.Badge, BadgeVariants: m.BadgeVariants,
		HideInMenu: m.HideInMenu, HideInBreadcrumb: m.HideInBreadcrumb, HideInTab: m.HideInTab, HideChildrenInMenu: m.HideChildrenInMenu,
	}
}

// 下游先建了详情页菜单，后来才在种子里补 HideInMenu / ActivePath 做成隐藏菜单：已存在的菜单也要按种子写入。
func TestSyncSeedMenuSyncsMetaFieldsOfExistingMenu(t *testing.T) {
	openTestDB(t, false, &adminmodel.Menu{})

	plain := adminmodel.Menu{Name: "DemoOrderDetail", Path: "/demo/order/detail/:id", Component: "/demo/order/detail", Type: "menu", Title: "demo.order.detail", Status: 1}
	if _, isNew := SyncSeedMenu(plain, ""); !isNew {
		t.Fatal("first sync should create the menu")
	}

	hidden := plain
	hidden.ActiveIcon = "carbon:document"
	hidden.ActivePath = "/demo/order"
	hidden.HideInMenu = true
	hidden.HideInBreadcrumb = true
	hidden.HideInTab = true
	hidden.HideChildrenInMenu = true
	hidden.BadgeType = "dot"
	hidden.Badge = "new"
	hidden.BadgeVariants = "primary"
	if _, isNew := SyncSeedMenu(hidden, ""); isNew {
		t.Fatal("second sync should update the existing menu, not create one")
	}
	want := menuMeta{
		ActiveIcon: "carbon:document", ActivePath: "/demo/order", BadgeType: "dot", Badge: "new", BadgeVariants: "primary",
		HideInMenu: true, HideInBreadcrumb: true, HideInTab: true, HideChildrenInMenu: true,
	}
	if got := loadMenuMeta(t, plain.Name); got != want {
		t.Fatalf("meta after sync = %+v, want %+v", got, want)
	}

	// 改回默认值：false 和空串同样要写回，不能被当成零值跳过。
	if _, isNew := SyncSeedMenu(plain, ""); isNew {
		t.Fatal("third sync should update the existing menu, not create one")
	}
	if got := loadMenuMeta(t, plain.Name); got != (menuMeta{}) {
		t.Fatalf("meta after reverting = %+v, want all zero", got)
	}
}

func TestSeedUsersRenamesLegacyRootAccountToSuper(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.User{}, &adminmodel.Role{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	DB = db

	superRole := adminmodel.Role{Name: "超级管理员", Code: "super", Status: 1}
	if err := DB.Create(&superRole).Error; err != nil {
		t.Fatalf("create super role: %v", err)
	}
	legacyRoot := adminmodel.User{Username: "vben", Password: "legacy-hash", RealName: "Vben", Status: 1, Roles: []adminmodel.Role{superRole}}
	if err := DB.Create(&legacyRoot).Error; err != nil {
		t.Fatalf("create legacy root user: %v", err)
	}

	seedUsers()

	var root adminmodel.User
	if err := DB.First(&root, legacyRoot.ID).Error; err != nil {
		t.Fatalf("load root user: %v", err)
	}
	if root.Username != "super" {
		t.Fatalf("expected legacy root username to be super, got %q", root.Username)
	}
	if root.RealName != "Super" {
		t.Fatalf("expected legacy root real name to be Super, got %q", root.RealName)
	}

	var count int64
	DB.Model(&adminmodel.User{}).Where("username = ?", "super").Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly one super user, got %d", count)
	}
}

func TestSeedUsersProductionSeedsOnlySuperWithRandomPassword(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.User{}, &adminmodel.Role{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	DB = db
	prevMode := config.C.Server.Mode
	config.C.Server.Mode = "production"
	t.Cleanup(func() { config.C.Server.Mode = prevMode })

	for _, code := range []string{"super", "admin", "user"} {
		if err := DB.Create(&adminmodel.Role{Name: code, Code: code, Status: 1}).Error; err != nil {
			t.Fatalf("create role %s: %v", code, err)
		}
	}

	seedUsers()

	var users []adminmodel.User
	DB.Find(&users)
	if len(users) != 1 || users[0].Username != "super" {
		t.Fatalf("production must seed only super, got %+v", users)
	}
	if bcrypt.CompareHashAndPassword([]byte(users[0].Password), []byte(DefaultSeedPassword)) == nil {
		t.Fatal("production super must not use the default seed password")
	}
}

func TestSeedUsersDoesNotReviveSoftDeletedAccount(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.User{}, &adminmodel.Role{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	DB = db
	prevMode := config.C.Server.Mode
	config.C.Server.Mode = "development"
	t.Cleanup(func() { config.C.Server.Mode = prevMode })

	for _, code := range []string{"super", "admin", "user"} {
		DB.Create(&adminmodel.Role{Name: code, Code: code, Status: 1})
	}
	seedUsers()
	if err := DB.Where("username = ?", "admin").Delete(&adminmodel.User{}).Error; err != nil {
		t.Fatalf("soft delete admin: %v", err)
	}

	seedUsers()

	var alive int64
	DB.Model(&adminmodel.User{}).Where("username = ?", "admin").Count(&alive)
	if alive != 0 {
		t.Fatalf("soft-deleted admin was revived by seeding (%d live rows)", alive)
	}
}

// 生产库启动时删掉残留的演示账号（不论密码改没改）及其角色关联，其他账号和内置超管不动。
func TestSeedUsersProductionPurgesDemoUsers(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.User{}, &adminmodel.Role{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	DB = db
	prevMode := config.C.Server.Mode
	t.Cleanup(func() { config.C.Server.Mode = prevMode })

	for _, code := range []string{"super", "admin", "user"} {
		DB.Create(&adminmodel.Role{Name: code, Code: code, Status: 1})
	}
	// 老库：曾以开发模式启动过，种下了 admin/jack
	config.C.Server.Mode = "development"
	seedUsers()
	var adminUser adminmodel.User
	DB.Where("username = ?", "admin").First(&adminUser)
	strong, _ := bcrypt.GenerateFromPassword([]byte("Np7xQ2wLs9"), bcrypt.MinCost)
	DB.Model(&adminUser).Update("password", string(strong)) // 改过密码的也要删
	var userRole adminmodel.Role
	DB.Where("code = ?", "user").First(&userRole)
	alice := adminmodel.User{Username: "alice", Password: string(strong), Status: 1, Roles: []adminmodel.Role{userRole}}
	DB.Create(&alice)

	config.C.Server.Mode = "production"
	seedUsers()

	var names []string
	DB.Model(&adminmodel.User{}).Unscoped().Order("username").Pluck("username", &names)
	if !reflect.DeepEqual(names, []string{"alice", "super"}) {
		t.Fatalf("生产库应只剩 super 和自建账号，得到 %v", names)
	}
	var orphan int64
	DB.Model(&adminmodel.UserRole{}).Where("user_id NOT IN (?)", DB.Model(&adminmodel.User{}).Select("id")).Count(&orphan)
	if orphan != 0 {
		t.Fatalf("删除演示账号后残留 %d 条 user_roles", orphan)
	}
}

func TestSeedConfigsLoginCaptchaDefaultByMode(t *testing.T) {
	prevMode := config.C.Server.Mode
	t.Cleanup(func() { config.C.Server.Mode = prevMode })
	for mode, want := range map[string]string{"production": "true", "development": "false"} {
		db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
		if err != nil {
			t.Fatalf("open sqlite db: %v", err)
		}
		if err := db.AutoMigrate(&adminmodel.Config{}); err != nil {
			t.Fatalf("migrate test db: %v", err)
		}
		DB = db
		config.C.Server.Mode = mode
		seedConfigs()
		var cfg adminmodel.Config
		DB.Where("config_key = ?", "login_captcha").First(&cfg)
		if cfg.ConfigValue != want {
			t.Errorf("%s 模式新库 login_captcha = %q，期望 %q", mode, cfg.ConfigValue, want)
		}
	}
}

// 早期种子的配置项没有名称，后台编辑配置时名称必填：启动时回填，已有名称不覆盖。
func TestSeedConfigsBackfillsEmptyConfigName(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.Config{}); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	DB = db
	DB.Create(&adminmodel.Config{ConfigKey: "login_captcha", ConfigValue: "false", Status: 1})
	DB.Create(&adminmodel.Config{ConfigName: "自定义名称", ConfigKey: "site_name", ConfigValue: "X", Status: 1})

	seedConfigs()

	names := map[string]string{}
	var cfgs []adminmodel.Config
	DB.Find(&cfgs)
	for _, c := range cfgs {
		names[c.ConfigKey] = c.ConfigName
	}
	if names["login_captcha"] != "登录验证码" {
		t.Errorf("空名称应回填，得到 %q", names["login_captcha"])
	}
	if names["site_name"] != "自定义名称" {
		t.Errorf("已有名称不应被覆盖，得到 %q", names["site_name"])
	}
	for key, name := range names {
		if name == "" {
			t.Errorf("种子配置 %s 没有名称", key)
		}
	}
}
