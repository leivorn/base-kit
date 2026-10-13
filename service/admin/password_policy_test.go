package admin

import (
	"strings"
	"testing"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestCheckPasswordRules(t *testing.T) {
	cases := []struct {
		name, password, username string
		wantReason               string // 空表示合格
	}{
		{"合格", "Np7xQ2wLs9", "alice", ""},
		{"中文也算字母", "密码强度测试12", "alice", ""},
		{"太短", "abc123", "alice", "不能少于8位"},
		{"纯字母", "abcdefgh", "alice", "同时包含字母和数字"},
		{"纯数字", "12345678", "alice", "同时包含字母和数字"},
		{"常见弱口令忽略大小写", "Admin888", "alice", "过于常见"},
		{"含用户名", "zhangsan2024", "ZhangSan", "不能包含用户名"},
		{"超过 64 位", strings.Repeat("a1", 33), "alice", "不能超过64位"},
		// 64 个字符以内但超过 bcrypt 的 72 字节上限
		{"超过 72 字节", strings.Repeat("密", 30) + "1", "alice", "不能超过64位"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPassword(tc.password, tc.username, PasswordMinLength)
			if tc.wantReason == "" {
				if err != nil {
					t.Fatalf("期望合格，得到 %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("期望含 %q 的错误，得到 %v", tc.wantReason, err)
			}
		})
	}
}

// 配置 password_min_length 只能往上调，且只约束设新口令；生产登录按固定基线判断，
// 调高配置不能把已有 8 位合格口令的人锁在门外。
func TestPasswordMinLengthConfig(t *testing.T) {
	useTestDB(t)
	setConfig := func(value string, status int) {
		t.Helper()
		store.DB.Unscoped().Where("config_key = ?", "password_min_length").Delete(&adminmodel.Config{})
		if err := store.DB.Create(&adminmodel.Config{ConfigKey: "password_min_length", ConfigValue: value, Status: status}).Error; err != nil {
			t.Fatalf("写配置: %v", err)
		}
	}

	setConfig("12", 1)
	if err := CheckPasswordStrength("Np7xQ2wLs9", "alice"); err == nil || !strings.Contains(err.Error(), "不能少于12位") {
		t.Fatalf("配置 12 时 10 位口令应被拒，得到 %v", err)
	}
	if !passwordMeetsBaseline("Np7xQ2wLs9", "alice") {
		t.Fatal("登录基线不应跟随配置调高")
	}

	setConfig("4", 1)
	if got := PasswordMinLen(); got != PasswordMinLength {
		t.Fatalf("配置低于下限时应取 %d，得到 %d", PasswordMinLength, got)
	}

	setConfig("12", 0)
	if got := PasswordMinLen(); got != PasswordMinLength {
		t.Fatalf("停用的配置项不应生效，得到 %d", got)
	}
}

// 生产首建超管和 reset-password 用的随机口令必须过策略，否则生产登录会把超管拦在外面。
func TestRandomPasswordMeetsPolicy(t *testing.T) {
	for range 5000 {
		p := store.RandomPassword()
		if err := checkPassword(p, "super", PasswordMinLength); err != nil {
			t.Fatalf("随机口令 %q 不合格: %v", p, err)
		}
	}
}

func useTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/test.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&adminmodel.Config{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	prev := store.DB
	store.DB = db
	t.Cleanup(func() {
		store.DB = prev
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
}
