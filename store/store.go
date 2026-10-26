package store

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/leivorn/base-kit/config"
	adminmodel "github.com/leivorn/base-kit/model/admin"

	// 纯 Go 的 SQLite 驱动：deploy.sh 用 CGO_ENABLED=0 交叉编译，
	// CGO 版驱动 (gorm.io/driver/sqlite) 编译出的二进制在生产连库直接 fatal
	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlserver"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

func dialector(driver, dsn string) (gorm.Dialector, error) {
	switch normalizeDriver(driver) {
	case "sqlite", "sqlite3", "":
		return sqlite.Open(sqliteDSNWithDefaults(dsn)), nil
	case "mysql", "mariadb":
		return mysql.Open(dsn), nil
	case "postgres", "postgresql", "pgsql":
		return postgres.Open(dsn), nil
	case "sqlserver", "mssql":
		return sqlserver.Open(dsn), nil
	default:
		return nil, fmt.Errorf("unsupported database driver %q, supported drivers: sqlite, mysql, postgres, sqlserver", driver)
	}
}

func normalizeDriver(driver string) string {
	return strings.ToLower(strings.TrimSpace(driver))
}

// sqliteDSNWithDefaults 给 SQLite DSN 补默认 PRAGMA：
// WAL 让读写不再互相阻塞（默认 delete 模式下写风暴会把读请求拖到几十 ms），
// busy_timeout 把 "database is locked" 变成短暂等待，synchronous=NORMAL 是
// WAL 模式官方推荐搭配。用户已显式写 _pragma 时不做任何改写。
func sqliteDSNWithDefaults(dsn string) string {
	if strings.Contains(dsn, "_pragma") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}

// IsUniqueViolation 判断错误是否为唯一索引冲突，handler 据此返回 400 而不是 500。
// 生产连接开了 TranslateError，四种驱动都会翻译成 gorm.ErrDuplicatedKey；
// 子串匹配是给未开翻译的连接（如测试里直接 gorm.Open）兜底。
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	for _, needle := range []string{
		"UNIQUE constraint failed", // sqlite
		"Error 1062",               // mysql
		"Duplicate entry",          // mysql
		"23505",                    // postgres
		"duplicate key value violates unique constraint", // postgres
		"Cannot insert duplicate key",                    // sqlserver
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// Options 是下游对数据层的扩展点：额外模型并入 AutoMigrate，Seed 在基底种子之后执行。
type Options struct {
	// Models 会追加到基底模型之后一起 AutoMigrate
	Models []any
	// Seed 在基底 seed() 之后调用，用来种下游自己的菜单和业务数据。
	// 菜单用 SyncSeedMenus / SyncSeedMenu 追加，追加后调用 RefreshRoleMenus。
	Seed func(db *gorm.DB)
}

// Init 连接数据库、迁移表结构并执行种子数据。
// 与模板时代的版本不同，这里返回 error 而不是 log.Fatal——库不该替调用方决定进程存亡。
func Init(opts Options) error {
	cfg := config.C.Database

	dial, err := dialector(cfg.Driver, cfg.DSN)
	if err != nil {
		return fmt.Errorf("database config error: %w", err)
	}

	DB, err = gorm.Open(dial, &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Warn),
		TranslateError: true, // 唯一冲突等驱动错误统一翻译成 gorm.ErrDuplicatedKey 等
	})
	if err != nil {
		return fmt.Errorf("failed to connect database: %w", err)
	}

	// 连接池：ConnMaxLifetime 必须小于 MySQL 的 wait_timeout（默认 8h，云上常被调低），
	// 否则闲置连接被服务端掐断后会偶发 "invalid connection"。
	// MaxIdle 与 MaxOpen 取相同值，避免高峰期连接反复重建（go-sql-driver 官方建议）。
	// 对 SQLite 同样无害：新连接会重新应用 DSN 里的 PRAGMA。
	if sqlDB, err := DB.DB(); err == nil {
		sqlDB.SetMaxOpenConns(25)
		sqlDB.SetMaxIdleConns(25)
		sqlDB.SetConnMaxLifetime(3 * time.Minute)
	} else {
		log.Printf("failed to configure connection pool: %v", err)
	}

	// AutoMigrate: 无表自动建表，新字段自动加列（不删列不改类型，和 FreeSql 行为一致）
	models := []any{
		&adminmodel.User{},
		&adminmodel.Role{},
		&adminmodel.Menu{},
		&adminmodel.Dept{},
		&adminmodel.Config{},
		&adminmodel.OperationLog{},
	}
	models = append(models, opts.Models...)
	if err = DB.AutoMigrate(models...); err != nil {
		return fmt.Errorf("failed to migrate database: %w", err)
	}

	purgeLegacySoftDeleted()
	seed()
	if opts.Seed != nil {
		opts.Seed(DB)
	}
	return nil
}

// purgeLegacySoftDeleted 物理清理角色/用户/配置表里历史遗留的软删除行。
// 这三张表现在都是物理删除（见 DeleteRole / DeleteUser / DeleteConfig）：它们的
// name/code/username/config_key 带唯一索引，软删行会一直占着键值，导致删过的角色
// 再也建不出同 code 的。老库升级时把残留行连同关联表一起清掉。
func purgeLegacySoftDeleted() {
	type target struct {
		model  any
		label  string
		before func(ids []uint)
	}
	targets := []target{
		{model: &adminmodel.Role{}, label: "sys_roles", before: func(ids []uint) {
			DB.Where("role_id IN ?", ids).Delete(&adminmodel.RoleMenu{})
			DB.Where("role_id IN ?", ids).Delete(&adminmodel.UserRole{})
		}},
		{model: &adminmodel.User{}, label: "sys_users", before: func(ids []uint) {
			DB.Where("user_id IN ?", ids).Delete(&adminmodel.UserRole{})
		}},
		{model: &adminmodel.Config{}, label: "sys_configs"},
	}
	for _, t := range targets {
		if !DB.Migrator().HasColumn(t.model, "DeletedAt") {
			continue
		}
		var ids []uint
		if err := DB.Model(t.model).Unscoped().Where("deleted_at IS NOT NULL").Pluck("id", &ids).Error; err != nil {
			log.Printf("  [migrate] scan soft-deleted %s failed: %v", t.label, err)
			continue
		}
		if len(ids) == 0 {
			continue
		}
		if t.before != nil {
			t.before(ids)
		}
		if err := DB.Unscoped().Where("id IN ?", ids).Delete(t.model).Error; err != nil {
			log.Printf("  [migrate] purge soft-deleted %s failed: %v", t.label, err)
			continue
		}
		log.Printf("  [migrate] purged %d soft-deleted rows from %s", len(ids), t.label)
	}
}

// ---------------------------------------------------------------------------
// 增量种子：每条数据按唯一键判断，已存在则跳过，不存在则插入
// 新增模块只需在这里加定义，重启即可自动补齐，无需删库
// ---------------------------------------------------------------------------

func seed() {
	seedRoles()
	seedMenus()
	seedUsers()
	seedDepts()
	seedConfigs()
	log.Println("seed check completed")
}

// --- Roles ---

func seedRoles() {
	roles := []adminmodel.Role{
		{Name: "超级管理员", Code: "super", Status: 1, Remark: "拥有所有权限"},
		{Name: "管理员", Code: "admin", Status: 1, Remark: "普通管理权限"},
		{Name: "普通用户", Code: "user", Status: 1, Remark: "基础查看权限"},
	}
	for _, r := range roles {
		var exists adminmodel.Role
		// Unscoped：管理员删掉的角色不随重启复活（与 seedUsers 同理）
		if DB.Unscoped().Where("code = ?", r.Code).First(&exists).Error == nil {
			continue
		}
		if err := DB.Create(&r).Error; err != nil {
			log.Printf("  [seed] create role %s failed: %v", r.Code, err)
			continue
		}
		log.Printf("  [seed] role created: %s", r.Code)
	}
}

// --- Menus ---

// MenuDef 描述一条种子菜单及其按钮，SyncSeedMenus 按它建表。
type MenuDef struct {
	adminmodel.Menu
	ParentName string
	Buttons    []adminmodel.Menu
}

// legacySeedMenus 是基底早期版本种下、现已移除的 vben 演示菜单，升级时连同角色关联一起清掉。
// 按 name + component 精确匹配：下游完全可能自己建一个叫 Analytics 的业务页，不能只看名字。
var legacySeedMenus = []adminmodel.Menu{
	{Name: "Analytics", Component: "/dashboard/analytics/index"},
	{Name: "About", Component: "_core/about/index"},
}

// SyncSeedMenus 幂等地同步一组菜单（含按钮），有新建时自动刷新 super/admin 角色关联。
// 下游 Seed 里加菜单用它，不用自己拼 SyncSeedMenu + RefreshRoleMenus。
func SyncSeedMenus(defs []MenuDef) (created bool) {
	for _, d := range defs {
		menu, isNew := SyncSeedMenu(d.Menu, d.ParentName)
		if isNew {
			log.Printf("  [seed] menu created: %s", d.Name)
			created = true
		}
		for _, btn := range d.Buttons {
			btn.ParentID = menu.ID
			if _, isNew := SyncSeedMenu(btn, ""); isNew {
				log.Printf("  [seed]   button created: %s", btn.Name)
				created = true
			}
		}
	}
	if created {
		RefreshRoleMenus()
	}
	return created
}

func seedMenus() {
	RemoveLegacySeedMenus(legacySeedMenus)

	defs := []MenuDef{
		{Menu: adminmodel.Menu{Name: "Dashboard", Path: "/dashboard", Type: "catalog", Icon: "lucide:layout-dashboard", Title: "page.dashboard.title", OrderNo: -1, Status: 1}},
		{Menu: adminmodel.Menu{Name: "Workspace", Path: "/workspace", Component: "/dashboard/workspace/index", Type: "menu", Icon: "carbon:workspace", Title: "page.dashboard.workspace", OrderNo: 1, Status: 1, AffixTab: true}, ParentName: "Dashboard"},
		{Menu: adminmodel.Menu{Name: "System", Path: "/system", Type: "catalog", Icon: "carbon:settings", Title: "system.title", OrderNo: 9997, Status: 1}},
		{
			Menu:       adminmodel.Menu{Name: "SystemUser", Path: "/system/user", Component: "/system/user/list", Type: "menu", Icon: "mdi:account-outline", Title: "system.user.title", OrderNo: 1, Status: 1, AuthCode: "System:User:List"},
			ParentName: "System",
			Buttons: []adminmodel.Menu{
				{Name: "SystemUserCreate", Type: "button", Title: "common.create", AuthCode: "System:User:Create", Status: 1},
				{Name: "SystemUserEdit", Type: "button", Title: "common.edit", AuthCode: "System:User:Edit", Status: 1},
				{Name: "SystemUserDelete", Type: "button", Title: "common.delete", AuthCode: "System:User:Delete", Status: 1},
			},
		},
		{
			Menu:       adminmodel.Menu{Name: "SystemRole", Path: "/system/role", Component: "/system/role/list", Type: "menu", Icon: "mdi:account-group", Title: "system.role.title", OrderNo: 2, Status: 1, AuthCode: "System:Role:List"},
			ParentName: "System",
			Buttons: []adminmodel.Menu{
				{Name: "SystemRoleCreate", Type: "button", Title: "common.create", AuthCode: "System:Role:Create", Status: 1},
				{Name: "SystemRoleEdit", Type: "button", Title: "common.edit", AuthCode: "System:Role:Edit", Status: 1},
				{Name: "SystemRoleDelete", Type: "button", Title: "common.delete", AuthCode: "System:Role:Delete", Status: 1},
			},
		},
		{
			Menu:       adminmodel.Menu{Name: "SystemMenu", Path: "/system/menu", Component: "/system/menu/list", Type: "menu", Icon: "carbon:menu", Title: "system.menu.title", OrderNo: 3, Status: 1, AuthCode: "System:Menu:List"},
			ParentName: "System",
			Buttons: []adminmodel.Menu{
				{Name: "SystemMenuCreate", Type: "button", Title: "common.create", AuthCode: "System:Menu:Create", Status: 1},
				{Name: "SystemMenuEdit", Type: "button", Title: "common.edit", AuthCode: "System:Menu:Edit", Status: 1},
				{Name: "SystemMenuDelete", Type: "button", Title: "common.delete", AuthCode: "System:Menu:Delete", Status: 1},
			},
		},
		{
			Menu:       adminmodel.Menu{Name: "SystemDept", Path: "/system/dept", Component: "/system/dept/list", Type: "menu", Icon: "carbon:container-services", Title: "system.dept.title", OrderNo: 4, Status: 1, AuthCode: "System:Dept:List"},
			ParentName: "System",
			Buttons: []adminmodel.Menu{
				{Name: "SystemDeptCreate", Type: "button", Title: "common.create", AuthCode: "System:Dept:Create", Status: 1},
				{Name: "SystemDeptEdit", Type: "button", Title: "common.edit", AuthCode: "System:Dept:Edit", Status: 1},
				{Name: "SystemDeptDelete", Type: "button", Title: "common.delete", AuthCode: "System:Dept:Delete", Status: 1},
			},
		},
		{
			Menu:       adminmodel.Menu{Name: "SystemConfig", Path: "/system/config", Component: "/system/config/list", Type: "menu", Icon: "carbon:settings-adjust", Title: "system.config.title", OrderNo: 5, Status: 1, AuthCode: "System:Config:List"},
			ParentName: "System",
			Buttons: []adminmodel.Menu{
				{Name: "SystemConfigCreate", Type: "button", Title: "common.create", AuthCode: "System:Config:Create", Status: 1},
				{Name: "SystemConfigEdit", Type: "button", Title: "common.edit", AuthCode: "System:Config:Edit", Status: 1},
				{Name: "SystemConfigDelete", Type: "button", Title: "common.delete", AuthCode: "System:Config:Delete", Status: 1},
			},
		},
		{
			Menu:       adminmodel.Menu{Name: "SystemOperationLog", Path: "/system/operation-log", Component: "/system/operation-log/list", Type: "menu", Icon: "carbon:activity", Title: "system.operationLog.title", OrderNo: 6, Status: 1, AuthCode: "System:OperationLog:List"},
			ParentName: "System",
			Buttons: []adminmodel.Menu{
				{Name: "SystemOperationLogDelete", Type: "button", Title: "common.delete", AuthCode: "System:OperationLog:Delete", Status: 1},
			},
		},
	}

	SyncSeedMenus(defs)
}

// removeLegacySeedMenus 按 name + component 物理删除已废弃的种子菜单及其 role_menus 关联。
// 这些菜单没有子节点；不走 syncSeedMenu，因为它只会新增/更新、不会删除。
// RemoveLegacySeedMenus 按 name + component 精确删除已废弃的种子菜单及其角色关联。
// 只匹配 name 会误删下游自己建的同名菜单。
func RemoveLegacySeedMenus(defs []adminmodel.Menu) {
	var ids []uint
	var names []string
	for _, d := range defs {
		var found []uint
		if err := DB.Model(&adminmodel.Menu{}).Unscoped().
			Where("name = ? AND component = ?", d.Name, d.Component).
			Pluck("id", &found).Error; err != nil {
			log.Printf("  [seed] scan legacy menu %s failed: %v", d.Name, err)
			continue
		}
		if len(found) > 0 {
			ids = append(ids, found...)
			names = append(names, d.Name)
		}
	}
	if len(ids) == 0 {
		return
	}
	DB.Where("menu_id IN ?", ids).Delete(&adminmodel.RoleMenu{})
	if err := DB.Unscoped().Where("id IN ?", ids).Delete(&adminmodel.Menu{}).Error; err != nil {
		log.Printf("  [seed] remove legacy menus failed: %v", err)
		return
	}
	log.Printf("  [seed] legacy menus removed: %s", strings.Join(names, ", "))
}

// RefreshRoleMenus 把当前全部菜单重新授予 super 和 admin 角色。
// 下游 Seed 里新增菜单后要调一次，否则新菜单对这两个角色不可见。
func RefreshRoleMenus() {
	var superRole, adminRole adminmodel.Role
	if DB.Where("code = ?", "super").First(&superRole).Error != nil {
		return
	}

	var allMenus []adminmodel.Menu
	DB.Find(&allMenus)
	DB.Model(&superRole).Association("Menus").Replace(allMenus)
	log.Println("  [seed] super role menus refreshed")

	if DB.Where("code = ?", "admin").First(&adminRole).Error == nil {
		var adminMenus []adminmodel.Menu
		DB.Find(&adminMenus)
		DB.Model(&adminRole).Association("Menus").Replace(adminMenus)
		log.Println("  [seed] admin role menus refreshed")
	}
}

// SyncSeedMenu 按 name 幂等地新增或更新一条菜单，返回菜单本身与「是否新建」。
//
// 菜单已存在时以种子定义为准：下面 updates 里的列（含 hide_in_menu、active_path 等 Vben meta 字段）
// 每次启动都按种子写回，在「菜单管理」里手改过的这些字段会被覆盖，要长期保留就改种子定义。
// status 不在其中，「菜单管理」里的启用/停用不会被重启恢复。
func SyncSeedMenu(menu adminmodel.Menu, parentName string) (adminmodel.Menu, bool) {
	if parentName != "" {
		var parent adminmodel.Menu
		if DB.Where("name = ?", parentName).First(&parent).Error == nil {
			menu.ParentID = parent.ID
		}
	}

	var exists adminmodel.Menu
	if DB.Where("name = ?", menu.Name).First(&exists).Error != nil {
		DB.Create(&menu)
		return menu, true
	}

	// 用 map 而不是结构体 Updates：bool 改回 false、字符串清空也要能落库。
	updates := map[string]any{
		"parent_id":             menu.ParentID,
		"path":                  menu.Path,
		"component":             menu.Component,
		"redirect":              menu.Redirect,
		"type":                  menu.Type,
		"icon":                  menu.Icon,
		"title":                 menu.Title,
		"auth_code":             menu.AuthCode,
		"order_no":              menu.OrderNo,
		"keep_alive":            menu.KeepAlive,
		"affix_tab":             menu.AffixTab,
		"iframe_src":            menu.IframeSrc,
		"link":                  menu.Link,
		"active_icon":           menu.ActiveIcon,
		"active_path":           menu.ActivePath,
		"hide_in_menu":          menu.HideInMenu,
		"hide_in_breadcrumb":    menu.HideInBreadcrumb,
		"hide_in_tab":           menu.HideInTab,
		"hide_children_in_menu": menu.HideChildrenInMenu,
		"badge_type":            menu.BadgeType,
		"badge":                 menu.Badge,
		"badge_variants":        menu.BadgeVariants,
	}
	DB.Model(&exists).Updates(updates)
	DB.First(&exists, exists.ID)
	return exists, false
}

// --- Users ---

// DefaultSeedPassword 是开发/演示环境的种子密码。生产模式既不用它建号，
// 也拒绝用它登录（见 service/admin.Authenticate），防止存量库沿用默认密码。
const DefaultSeedPassword = "123456"

// RandomPassword 生成随机口令（20 位字母数字，去掉易混字符），用于生产首建超管和 reset-password 命令。
// 保证同时含字母和数字：生产登录会拦截不合口令策略的密码，纯字母的随机口令会把超管锁在门外。
func RandomPassword() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	buf := make([]byte, 20)
	for {
		if _, err := rand.Read(buf); err != nil {
			log.Fatalf("generate random password: %v", err)
		}
		var hasLetter, hasDigit bool
		for i, b := range buf {
			buf[i] = alphabet[int(b)%len(alphabet)]
			if buf[i] >= '2' && buf[i] <= '9' {
				hasDigit = true
			} else {
				hasLetter = true
			}
		}
		if hasLetter && hasDigit {
			return string(buf)
		}
	}
}

func seedUsers() {
	renameLegacyRootUser()

	userDefs := []struct {
		Username string
		RealName string
		RoleCode string
		HomePath string
	}{
		{"super", "Super", "super", ""},
		{"admin", "Admin", "admin", "/workspace"},
		{"jack", "Jack", "user", "/workspace"},
	}
	// 生产库只种内置超管：admin/jack 是演示账号，密码人尽皆知，不该出现在线上；
	// 老库里残留的（曾用开发模式启动过、或早期版本种下的）也一并删掉
	if config.IsProduction() {
		userDefs = userDefs[:1]
		purgeDemoUsers()
	}

	production := config.IsProduction()
	for _, u := range userDefs {
		var exists adminmodel.User
		// Unscoped：被软删的账号也算存在，不能因为管理员删了它就在下次启动时按默认密码复活
		if DB.Unscoped().Where("username = ?", u.Username).First(&exists).Error == nil {
			continue
		}
		password := DefaultSeedPassword
		if production {
			// 生产库首建超管用随机密码，只在启动日志里出现一次
			password = RandomPassword()
		}
		hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		var role adminmodel.Role
		if DB.Where("code = ?", u.RoleCode).First(&role).Error != nil {
			log.Printf("  [seed] skip user %s: role %s not found", u.Username, u.RoleCode)
			continue
		}
		user := adminmodel.User{
			Username: u.Username,
			Password: string(hash),
			RealName: u.RealName,
			Status:   1,
			HomePath: u.HomePath,
			Roles:    []adminmodel.Role{role},
		}
		if err := DB.Create(&user).Error; err != nil {
			log.Printf("  [seed] create user %s failed: %v", u.Username, err)
			continue
		}
		if production {
			log.Printf("  [seed] user created: %s, initial password: %s (shown once, change it after first login)", u.Username, password)
		} else {
			log.Printf("  [seed] user created: %s", u.Username)
		}
	}

	// 早期种子把 jack 的首页指向已删除的 /analytics，统一迁到 /workspace
	DB.Model(&adminmodel.User{}).Where("home_path = ?", "/analytics").Update("home_path", "/workspace")

	warnDefaultSuperPassword()
}

// demoUsernames 是开发模式种下的演示账号，生产库启动时清除。
var demoUsernames = []string{"admin", "jack"}

// purgeDemoUsers 物理删除演示账号及其角色关联，不论密码是否改过：线上只保留内置超管，
// 真正需要的账号由超管在后台另建（用户名别再叫 admin/jack，否则每次启动都会被删）。id=1 的内置超管永不删除。
func purgeDemoUsers() {
	var users []adminmodel.User
	if err := DB.Unscoped().Select("id", "username").Where("username IN ? AND id <> ?", demoUsernames, 1).Find(&users).Error; err != nil {
		log.Printf("  [seed] scan demo users failed: %v", err)
		return
	}
	for _, u := range users {
		err := DB.Transaction(func(tx *gorm.DB) error {
			// 先删关联行再删主行：user_roles 对 sys_users 有外键
			if err := tx.Where("user_id = ?", u.ID).Delete(&adminmodel.UserRole{}).Error; err != nil {
				return err
			}
			return tx.Unscoped().Delete(&adminmodel.User{}, u.ID).Error
		})
		if err != nil {
			log.Printf("  [seed] delete demo user %s failed: %v", u.Username, err)
			continue
		}
		log.Printf("  [seed] production: demo user %q (id=%d) deleted", u.Username, u.ID)
	}
}

// warnDefaultSuperPassword 每次启动检查内置超管是否还在用种子密码，命中则打 WARN。
// 一次 bcrypt 比对的成本可以忽略；这是生产库最常见的失守点。
func warnDefaultSuperPassword() {
	var super adminmodel.User
	if DB.Select("id", "password").Where("username = ?", "super").First(&super).Error != nil {
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(super.Password), []byte(DefaultSeedPassword)) == nil {
		log.Printf("  [seed] WARN: user 'super' still uses the default password %s; production mode refuses this password at login, change it now", DefaultSeedPassword)
	}
}

func renameLegacyRootUser() {
	var root adminmodel.User
	if DB.First(&root, 1).Error != nil || root.Username != "vben" {
		return
	}

	var existing adminmodel.User
	if DB.Where("username = ? AND id != ?", "super", root.ID).First(&existing).Error == nil {
		log.Println("  [seed] skip legacy root rename: username super already exists")
		return
	}

	DB.Model(&root).Updates(map[string]any{
		"username":  "super",
		"real_name": "Super",
	})
	log.Println("  [seed] root user renamed: vben -> super")
}

// --- Departments ---

func seedDepts() {
	type deptDef struct {
		Name       string
		OrderNo    int
		ParentName string
	}
	defs := []deptDef{
		{"总公司", 1, ""},
		{"技术部", 1, "总公司"},
		{"市场部", 2, "总公司"},
		{"财务部", 3, "总公司"},
	}
	for _, d := range defs {
		var exists adminmodel.Dept
		if DB.Where("name = ?", d.Name).First(&exists).Error == nil {
			continue
		}
		dept := adminmodel.Dept{Name: d.Name, OrderNo: d.OrderNo, Status: 1}
		if d.ParentName != "" {
			var parent adminmodel.Dept
			if DB.Where("name = ?", d.ParentName).First(&parent).Error == nil {
				dept.ParentID = parent.ID
			}
		}
		DB.Create(&dept)
		log.Printf("  [seed] dept created: %s", d.Name)
	}
}

// --- Configs ---

func seedConfigs() {
	defs := []adminmodel.Config{
		{ConfigName: "站点名称", ConfigKey: "site_name", ConfigValue: "Admin 后台管理系统", ConfigGroup: "basic", Status: 1, Remark: "站点名称"},
		{ConfigName: "站点 Logo", ConfigKey: "site_logo", ConfigValue: "/logo.png", ConfigGroup: "basic", Status: 1, Remark: "站点 Logo"},
		{ConfigName: "上传大小上限", ConfigKey: "upload_max_size", ConfigValue: "10", ConfigGroup: "upload", Status: 1, Remark: "上传文件最大大小(MB)"},
		// 生产新库默认开验证码；开发库默认关，免得本地调试每次都要输。已有库保留原值，按需在后台改
		{ConfigName: "登录验证码", ConfigKey: "login_captcha", ConfigValue: loginCaptchaDefault(), ConfigGroup: "security", Status: 1, Remark: "后台登录是否需要图形验证码（true/false）"},
		{ConfigName: "密码最小长度", ConfigKey: "password_min_length", ConfigValue: "8", ConfigGroup: "security", Status: 1, Remark: "设置密码时的最小长度（不低于 8，只约束新密码）"},
	}
	for _, cfg := range defs {
		var exists adminmodel.Config
		if DB.Where("config_key = ?", cfg.ConfigKey).First(&exists).Error == nil {
			// 早期种子没填名称，而后台编辑配置要求名称必填，不补上就改不了这些配置
			if exists.ConfigName == "" {
				DB.Model(&exists).Update("config_name", cfg.ConfigName)
			}
			continue
		}
		DB.Create(&cfg)
		log.Printf("  [seed] config created: %s", cfg.ConfigKey)
	}
}

func loginCaptchaDefault() string {
	if config.IsProduction() {
		return "true"
	}
	return "false"
}
