package store

import (
	"errors"
	"testing"

	"github.com/leivorn/base-kit/config"
	adminmodel "github.com/leivorn/base-kit/model/admin"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T, translate bool, models ...any) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{TranslateError: translate})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	DB = db
}

func TestIsUniqueViolation(t *testing.T) {
	for _, translate := range []bool{true, false} {
		openTestDB(t, translate, &adminmodel.Role{})
		if err := DB.Create(&adminmodel.Role{Name: "A", Code: "a", Status: 1}).Error; err != nil {
			t.Fatalf("first insert: %v", err)
		}
		err := DB.Create(&adminmodel.Role{Name: "A", Code: "a", Status: 1}).Error
		if err == nil {
			t.Fatal("duplicate insert must fail")
		}
		if !IsUniqueViolation(err) {
			t.Fatalf("translate=%v: sqlite duplicate must be detected, got %v", translate, err)
		}
	}

	cases := map[string]bool{
		"Error 1062 (23000): Duplicate entry 'a' for key 'sys_roles.idx_code'":                     true,
		"ERROR: duplicate key value violates unique constraint \"idx_code\" (SQLSTATE 23505)":      true,
		"mssql: Violation of UNIQUE KEY constraint. Cannot insert duplicate key in object 'dbo.x'": true,
		"record not found": false,
	}
	for msg, want := range cases {
		if got := IsUniqueViolation(errors.New(msg)); got != want {
			t.Fatalf("%q: got %v, want %v", msg, got, want)
		}
	}
	if IsUniqueViolation(nil) {
		t.Fatal("nil error must not be a violation")
	}
}

func TestPurgeLegacySoftDeletedFreesUniqueKey(t *testing.T) {
	openTestDB(t, true, &adminmodel.User{}, &adminmodel.Role{}, &adminmodel.Menu{}, &adminmodel.Config{})

	menu := adminmodel.Menu{Name: "M", Type: "menu", Title: "m", Status: 1}
	if err := DB.Create(&menu).Error; err != nil {
		t.Fatalf("create menu: %v", err)
	}
	role := adminmodel.Role{Name: "Editor", Code: "editor", Status: 1, Menus: []adminmodel.Menu{menu}}
	if err := DB.Create(&role).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}
	user := adminmodel.User{Username: "eve", Password: "x", Status: 1, Roles: []adminmodel.Role{role}}
	if err := DB.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	// 旧版本的软删除
	if err := DB.Delete(&role).Error; err != nil {
		t.Fatalf("soft delete role: %v", err)
	}
	if err := DB.Create(&adminmodel.Role{Name: "Editor", Code: "editor", Status: 1}).Error; err == nil {
		t.Fatal("precondition: soft-deleted row should still block the unique key")
	}

	purgeLegacySoftDeleted()

	if err := DB.Create(&adminmodel.Role{Name: "Editor", Code: "editor", Status: 1}).Error; err != nil {
		t.Fatalf("recreate after purge must succeed, got %v", err)
	}
	var n int64
	DB.Model(&adminmodel.RoleMenu{}).Where("role_id = ?", role.ID).Count(&n)
	if n != 0 {
		t.Fatalf("role_menus of purged role must be cleared, got %d", n)
	}
	DB.Model(&adminmodel.UserRole{}).Where("role_id = ?", role.ID).Count(&n)
	if n != 0 {
		t.Fatalf("user_roles of purged role must be cleared, got %d", n)
	}
}

func TestSeedMenusRemovesLegacyDemoMenus(t *testing.T) {
	openTestDB(t, true, &adminmodel.Role{}, &adminmodel.Menu{})

	analytics := adminmodel.Menu{Name: "Analytics", Path: "/analytics", Component: "/dashboard/analytics/index", Type: "menu", Title: "page.dashboard.analytics", Status: 1}
	about := adminmodel.Menu{Name: "About", Path: "/about", Component: "_core/about/index", Type: "menu", Title: "demos.vben.about", Status: 1}
	// 下游自己建的同名菜单：component 不同，绝不能被当成演示菜单删掉
	downstream := adminmodel.Menu{Name: "Analytics", Path: "/report/analytics", Component: "/report/analytics/index", Type: "menu", Title: "report.analytics", Status: 1}
	for _, m := range []*adminmodel.Menu{&analytics, &about, &downstream} {
		if err := DB.Create(m).Error; err != nil {
			t.Fatalf("create %s: %v", m.Name, err)
		}
	}
	role := adminmodel.Role{Name: "Admin", Code: "admin", Status: 1, Menus: []adminmodel.Menu{analytics, about, downstream}}
	if err := DB.Create(&role).Error; err != nil {
		t.Fatalf("create role: %v", err)
	}

	seedMenus()

	var n int64
	DB.Model(&adminmodel.Menu{}).Unscoped().Where("id IN ?", []uint{analytics.ID, about.ID}).Count(&n)
	if n != 0 {
		t.Fatalf("legacy menus must be removed, %d remain", n)
	}
	DB.Model(&adminmodel.RoleMenu{}).Where("menu_id IN ?", []uint{analytics.ID, about.ID}).Count(&n)
	if n != 0 {
		t.Fatalf("role_menus of legacy menus must be cleared, got %d", n)
	}
	DB.Model(&adminmodel.Menu{}).Where("id = ?", downstream.ID).Count(&n)
	if n != 1 {
		t.Fatal("downstream menu with the same name must survive")
	}
	DB.Model(&adminmodel.RoleMenu{}).Where("menu_id = ?", downstream.ID).Count(&n)
	if n != 1 {
		t.Fatal("downstream menu grants must survive")
	}

	var dashboard, workspace adminmodel.Menu
	if err := DB.Where("name = ?", "Dashboard").First(&dashboard).Error; err != nil {
		t.Fatalf("Dashboard catalog must be seeded: %v", err)
	}
	if err := DB.Where("name = ?", "Workspace").First(&workspace).Error; err != nil {
		t.Fatalf("Workspace menu must be seeded: %v", err)
	}
	if workspace.ParentID != dashboard.ID || workspace.Path != "/workspace" {
		t.Fatalf("Workspace must sit under Dashboard at /workspace, got parent=%d path=%s", workspace.ParentID, workspace.Path)
	}
}

func TestSeedUsersProductionSeedsOnlySuper(t *testing.T) {
	openTestDB(t, true, &adminmodel.User{}, &adminmodel.Role{})
	seedRoles()

	saved := config.C.Server.Mode
	t.Cleanup(func() { config.C.Server.Mode = saved })

	config.C.Server.Mode = "production"
	seedUsers()
	var users []adminmodel.User
	DB.Order("id").Find(&users)
	if len(users) != 1 || users[0].Username != "super" {
		t.Fatalf("production must seed only super, got %v", usernames(users))
	}

	config.C.Server.Mode = "development"
	seedUsers()
	DB.Order("id").Find(&users)
	if len(users) != 3 {
		t.Fatalf("development must seed all demo users, got %v", usernames(users))
	}
	for _, u := range users {
		if u.HomePath == "/analytics" {
			t.Fatalf("no seeded user may point at the removed /analytics page: %s", u.Username)
		}
	}
}

func usernames(users []adminmodel.User) []string {
	out := make([]string, len(users))
	for i, u := range users {
		out[i] = u.Username
	}
	return out
}
