package store

import (
	"fmt"
	"strings"
)

// SeedMode 控制启动时种子数据怎么写（配置 database.seed）。
//
// 种子默认每次启动都按定义「写回」基底数据：菜单按名字覆盖、新菜单授给 super/admin 角色、
// 清理软删行、生产模式删演示账号。库只给本项目用时这正是想要的；库和别的系统共用（比如新老后台
// 切换期间共用一套 sys_* 表）时就会改到别人的数据，这时改用 insert 或 off。
type SeedMode string

const (
	// SeedFull 默认：建缺的、按种子定义写回已有的、清理遗留数据（v1.5 及之前的行为）。
	SeedFull SeedMode = "full"
	// SeedInsert 只补缺：缺的角色/菜单/用户/部门/配置照建，已有的一律不改；
	// 不清理软删行、不删遗留菜单与演示账号、不把菜单重新授给角色。
	SeedInsert SeedMode = "insert"
	// SeedOff 不写任何种子数据，只做 AutoMigrate（只加表加列）。
	// Options.Seed 仍会被调用（模板垫片要在里面拿到 DB），SyncSeedMenus 等种子助手此时什么都不写；
	// 下游 Seed 里自己写的数据要用 CurrentSeedMode 判断后跳过。
	SeedOff SeedMode = "off"
)

// seedMode 是当前这次 Init 采用的模式，Init 开头按配置设置。
var seedMode = SeedFull

// ParseSeedMode 解析配置值：空串按 full；大小写与首尾空白不敏感；不认识的值返回错误。
func ParseSeedMode(v string) (SeedMode, error) {
	switch SeedMode(strings.ToLower(strings.TrimSpace(v))) {
	case "", SeedFull:
		return SeedFull, nil
	case SeedInsert:
		return SeedInsert, nil
	case SeedOff:
		return SeedOff, nil
	default:
		return "", fmt.Errorf("database.seed 取值 %q 不认识，可选 full（默认）/ insert / off", v)
	}
}

// CurrentSeedMode 返回本次启动的种子模式。下游 Seed 里写自己的数据前据此判断：
//
//	if store.CurrentSeedMode() == store.SeedOff {
//	    return
//	}
func CurrentSeedMode() SeedMode {
	return seedMode
}

// seedUpdatesAllowed 报告是否允许改写/删除已有数据（只有 full 允许）。
func seedUpdatesAllowed() bool {
	return seedMode == SeedFull
}

// seedInsertsAllowed 报告是否允许补建缺失的数据（off 不允许）。
func seedInsertsAllowed() bool {
	return seedMode != SeedOff
}
