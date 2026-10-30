package store

import (
	"os"
	"testing"

	"github.com/leivorn/base-kit/config"
	adminmodel "github.com/leivorn/base-kit/model/admin"
)

// TestMySQLInitMigratesAndSeeds 在真实 MySQL 上跑一遍 Init：建表 + 迁移 + 种子数据。
//
// 有些建表问题只在 MySQL 上暴露、SQLite 一路绿灯，最典型的是同一列上出现两个未命名索引
// （`uniqueIndex;index` 这种写法）会报 1060 Duplicate column name。kit 现在拥有全部模型，
// 这类问题必须在 kit 的 CI 里挡住，不能等下游上线才发现。
//
// 没有 BASEKIT_TEST_MYSQL_DSN 时跳过，本机开发不受影响。
func TestMySQLInitMigratesAndSeeds(t *testing.T) {
	dsn := os.Getenv("BASEKIT_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 BASEKIT_TEST_MYSQL_DSN，跳过 MySQL 迁移测试")
	}

	saved := config.C
	t.Cleanup(func() { config.C = saved })
	config.C.Database.Driver = "mysql"
	config.C.Database.DSN = dsn
	config.C.Server.Mode = "development"

	if err := Init(Options{}); err != nil {
		t.Fatalf("MySQL Init: %v", err)
	}

	// 幂等：连跑两次不能因为重复建索引或重复种子数据失败
	if err := Init(Options{}); err != nil {
		t.Fatalf("MySQL Init 第二次: %v", err)
	}

	var users, roles, menus int64
	DB.Model(&adminmodel.User{}).Count(&users)
	DB.Model(&adminmodel.Role{}).Count(&roles)
	DB.Model(&adminmodel.Menu{}).Count(&menus)
	if users == 0 || roles == 0 || menus == 0 {
		t.Fatalf("种子数据不完整: users=%d roles=%d menus=%d", users, roles, menus)
	}

	// 扩展列这条路径也要在 MySQL 上验证（AutoMigrate 加列不能碰坏既有列）
	if err := DB.AutoMigrate(&userExtension{}); err != nil {
		t.Fatalf("MySQL 迁移扩展模型: %v", err)
	}
	if !DB.Migrator().HasColumn(&userExtension{}, "dept") {
		t.Error("MySQL 上扩展列 dept 未添加")
	}

	// 扩展结构登记进 Options.Models、种子 off 再启动一次：同表的两边列都在，种子数据一行不动
	var before int64
	DB.Model(&adminmodel.RoleMenu{}).Count(&before)
	config.C.Database.Seed = "off"
	config.C.Server.Mode = "production"
	if err := Init(Options{Models: []any{&opLogExtension{}, &userExtension{}}}); err != nil {
		t.Fatalf("MySQL Init（seed=off，同表扩展）: %v", err)
	}
	for _, c := range []struct {
		model any
		col   string
	}{{&adminmodel.OperationLog{}, "body"}, {&opLogExtension{}, "remarks"}, {&adminmodel.User{}, "password_changed_at"}, {&userExtension{}, "dept"}} {
		if !DB.Migrator().HasColumn(c.model, c.col) {
			t.Errorf("MySQL 上 %T 缺列 %s", c.model, c.col)
		}
	}
	var after int64
	DB.Model(&adminmodel.RoleMenu{}).Count(&after)
	if after != before {
		t.Errorf("seed=off 改动了 role_menus: %d -> %d", before, after)
	}
}
