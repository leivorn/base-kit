package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	JWT      JWTConfig      `yaml:"jwt"`
}

type ServerConfig struct {
	// Host 监听地址，空串（默认）监听所有网卡；在 nginx 之后的生产服务填 127.0.0.1，别的机器连不上
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// Mode 为 production 时启动前校验 jwt.secret（见 ValidateProduction），且种子只创建 super 账号
	Mode          string `yaml:"mode"`
	EnableSwagger bool   `yaml:"enable_swagger"`
	CorsOrigins   string `yaml:"cors_origins"`
	SwaggerTitle  string `yaml:"swagger_title"`
	SwaggerDesc   string `yaml:"swagger_desc"`
	// 操作日志保留天数，<=0 表示永久保留（默认）
	OpLogRetentionDays int `yaml:"op_log_retention_days"`
	// 请求体大小上限(MB)，<=0 使用 Fiber 默认 4MB；上传大文件时按需调大
	BodyLimitMB int `yaml:"body_limit_mb"`
}

type DatabaseConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
	// Seed 启动时种子数据的写法：full（默认，空串同）/ insert（只补缺）/ off（不写），见 store.SeedMode。
	// 库和别的系统共用时用 insert 或 off，免得种子改写别人的菜单、角色授权和账号。
	Seed string `yaml:"seed"`
}

type JWTConfig struct {
	Secret        string        `yaml:"secret"`
	Expire        time.Duration `yaml:"expire"`
	RefreshExpire time.Duration `yaml:"refresh_expire"`
}

var C Config

// loadedPath 记录 Load 读的是哪个文件，LoadExtra 据此解析下游自己的配置段。
var loadedPath string

func Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, &C); err != nil {
		return err
	}
	loadedPath = path
	// 环境变量 SERVER_PORT 优先于 config.yaml，dev.sh 自动换端口时依赖此覆盖
	if v := os.Getenv("SERVER_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil && port > 0 && port < 65536 {
			C.Server.Port = port
		}
	}
	// 环境变量 SERVER_HOST 优先于 config.yaml，systemd 单元里用它把服务钉在 127.0.0.1
	if v := strings.TrimSpace(os.Getenv("SERVER_HOST")); v != "" {
		C.Server.Host = v
	}
	return nil
}

// IsProduction 报告 server.mode 是否为 production（忽略大小写与首尾空白）。
func IsProduction() bool {
	return strings.EqualFold(strings.TrimSpace(C.Server.Mode), "production")
}

// 生产模式对 jwt.secret 的最低要求。示例配置里的占位值一旦上线，
// 任何人都能伪造 userId=1 的 token 拿到超管权限，所以直接拒绝启动。
const minJWTSecretLen = 32

var placeholderJWTSecrets = []string{
	"change-this-to-a-strong-secret",
	"your-secret-key-change-in-production",
	"change-this-to-strong-secret-in-production",
}

// ValidateProduction 在生产模式下校验必须人工设置的配置；开发模式不做限制。
func ValidateProduction() error {
	if !IsProduction() {
		return nil
	}
	secret := strings.TrimSpace(C.JWT.Secret)
	for _, placeholder := range placeholderJWTSecrets {
		if strings.EqualFold(secret, placeholder) {
			return fmt.Errorf("jwt.secret is still the example placeholder; set a real secret before running in production mode")
		}
	}
	if len(secret) < minJWTSecretLen {
		return fmt.Errorf("jwt.secret is too short for production mode (%d chars, need at least %d)", len(secret), minJWTSecretLen)
	}
	return nil
}

// Path 返回 Load 实际读取的配置文件路径。
func Path() string {
	return loadedPath
}

// LoadExtra 把同一份配置文件再解析进下游自己的结构体，用来读基底之外的配置段：
//
//	type shopConfig struct {
//	    Payment struct{ AppID string `yaml:"app_id"` } `yaml:"payment"`
//	}
//	var shopCfg shopConfig
//	config.LoadExtra(&shopCfg)
//
// 未知字段会被忽略，所以下游结构体只写自己关心的段即可。
func LoadExtra(dst any) error {
	if loadedPath == "" {
		return fmt.Errorf("配置尚未加载，请先调用 Load 或 basekit.Bootstrap")
	}
	data, err := os.ReadFile(loadedPath)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, dst)
}
