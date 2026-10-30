package admin

import (
	"strconv"
	"strings"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"
)

// 运行时读取「系统配置」（sys_configs）。每次现查库：这几项只在登录、设密码时用到，
// 量很小，现查能保证后台改完立即生效，不用处理缓存失效。

// ConfigValue 返回启用（status=1）配置项的值；不存在或已停用时 ok=false。
func ConfigValue(key string) (string, bool) {
	var cfg adminmodel.Config
	// Limit(1).Find 而不是 First：配置项缺失是正常情况，First 会让 GORM 打一条 record not found 日志
	res := store.DB.Select("config_value").Where("config_key = ? AND status = ?", key, 1).Limit(1).Find(&cfg)
	if res.Error != nil || res.RowsAffected == 0 {
		return "", false
	}
	return strings.TrimSpace(cfg.ConfigValue), true
}

// configExists 报告配置项这一行是否存在（不论启用与否）。查询出错时按存在处理，交给 ConfigValue 的口径。
func configExists(key string) bool {
	var count int64
	if err := store.DB.Model(&adminmodel.Config{}).Where("config_key = ?", key).Count(&count).Error; err != nil {
		return true
	}
	return count > 0
}

// ConfigBool 把 true/1/yes/on（忽略大小写）当作开启，其余（含缺失、停用）都是关闭。
func ConfigBool(key string) bool {
	v, ok := ConfigValue(key)
	if !ok {
		return false
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// ConfigInt 读整数配置，缺失、停用或不是整数时返回 def。
func ConfigInt(key string, def int) int {
	v, ok := ConfigValue(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
