package store

import (
	"testing"

	adminmodel "github.com/leivorn/base-kit/model/admin"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// 下游给 sys_users 加列的官方姿势：只声明表名、主键和新列，登记到 store.Options.Models。
// AutoMigrate 只增不删，kit 先建表、这个结构再迁移同一张表时只补上新列。
//
// 关键：不要嵌入 adminmodel.User。嵌入会把 User 的 Roles many2many 也带过来，
// GORM 按新结构体名派生外键，往共享的 user_roles 表里加一列 <新结构名>_id，
// 通过嵌入结构写入时 user_id 为 NULL——kit 的 handler 按 user_id 查角色，
// 角色关联就静默失效了。加 `gorm:"-"` 屏蔽 Roles 也挡不住那一列。
type userExtension struct {
	ID     uint   `gorm:"primarykey"`
	Dept   string `gorm:"size:64;index"`
	Level  int    `gorm:"default:0"`
	Mobile string `gorm:"column:mobile;size:20"`
}

func (userExtension) TableName() string { return "sys_users" }

// TestUserExtensionAddsColumns 锁定「给基底表加列」这条扩展路径：
// 新列加得上、基础列不变、共享的 user_roles 不被污染、kit 的角色关联照常工作。
// 这是 kit 承诺给下游的扩展方式，坏了下游就只能 fork kit。
func TestUserExtensionAddsColumns(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/ext.db"), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	DB = db

	// 顺序与 store.Init 一致：kit 模型先迁移，下游扩展随后
	if err := db.AutoMigrate(&adminmodel.User{}, &adminmodel.Role{}); err != nil {
		t.Fatalf("迁移基底模型: %v", err)
	}
	if err := db.AutoMigrate(&userExtension{}); err != nil {
		t.Fatalf("迁移扩展模型: %v", err)
	}

	m := db.Migrator()
	for _, col := range []string{"dept", "level", "mobile"} {
		if !m.HasColumn(&userExtension{}, col) {
			t.Errorf("扩展列 %s 未添加", col)
		}
	}
	// 基础列必须原样保留，否则 kit 的 handler 读不到数据
	for _, col := range []string{"username", "password", "status", "password_changed_at", "home_path"} {
		if !m.HasColumn(&adminmodel.User{}, col) {
			t.Errorf("基础列 %s 丢失", col)
		}
	}

	// 共享的 user_roles 只该有 user_id / role_id 两列。
	// 多出 <结构名>_id 说明扩展模型把关联也带进来了，那会让角色关联静默失效。
	var joinCols []string
	if err := db.Raw("SELECT name FROM PRAGMA_table_info('user_roles')").Scan(&joinCols).Error; err != nil {
		t.Fatalf("读 user_roles 列: %v", err)
	}
	if len(joinCols) != 2 {
		t.Errorf("user_roles 列 = %v，期望仅 user_id / role_id；扩展模型不应携带关联", joinCols)
	}

	// kit 的写入路径：角色照常挂上，join 行的 user_id 有值
	role := adminmodel.Role{Name: "编辑", Code: "editor", Status: 1}
	if err := db.Create(&role).Error; err != nil {
		t.Fatalf("建角色: %v", err)
	}
	user := adminmodel.User{Username: "alice", Password: "x", Status: 1, Roles: []adminmodel.Role{role}}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("建用户: %v", err)
	}
	var loaded adminmodel.User
	if err := db.Preload("Roles").First(&loaded, user.ID).Error; err != nil {
		t.Fatalf("预加载角色: %v", err)
	}
	if len(loaded.Roles) != 1 || loaded.Roles[0].Code != "editor" {
		t.Fatalf("角色关联失效: %#v", loaded.Roles)
	}

	// 下游用自己的结构读写扩展列，与 kit 互不干扰
	if err := db.Model(&userExtension{}).Where("id = ?", user.ID).
		Updates(map[string]any{"dept": "技术部", "level": 3}).Error; err != nil {
		t.Fatalf("更新扩展列: %v", err)
	}
	var ext userExtension
	if err := db.First(&ext, user.ID).Error; err != nil {
		t.Fatalf("读扩展列: %v", err)
	}
	if ext.Dept != "技术部" || ext.Level != 3 {
		t.Errorf("扩展列未落库: %+v", ext)
	}

	// username 上只该有一个索引：同一列两个未命名唯一索引在 MySQL 上会报 1060
	indexes, err := m.GetIndexes(&adminmodel.User{})
	if err != nil {
		t.Fatalf("读索引: %v", err)
	}
	usernameIdx := 0
	for _, idx := range indexes {
		for _, col := range idx.Columns() {
			if col == "username" {
				usernameIdx++
			}
		}
	}
	if usernameIdx != 1 {
		t.Errorf("username 上有 %d 个索引，期望 1", usernameIdx)
	}
}
