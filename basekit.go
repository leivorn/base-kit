// Package basekit 是 base 基底的后端框架层：配置、鉴权、权限码、数据层与系统管理模块。
//
// 项目从 https://github.com/leivorn/base 模板派生，模板的 server/main.go 只需要：
//
//	basekit.Run(basekit.Options{
//	    Models: store.ProjectModels(),
//	    Seed:   store.ProjectSeed,
//	    Routes: router.Setup,
//	})
//
// 框架层的 bug 修复和新功能通过 go get github.com/leivorn/base-kit@latest 获取，
// 不必再走 git merge。
package basekit

import (
	"fmt"
	"log"
	"os"

	"github.com/leivorn/base-kit/config"
	"github.com/leivorn/base-kit/router"
	adminsvc "github.com/leivorn/base-kit/service/admin"
	"github.com/leivorn/base-kit/store"
	"github.com/leivorn/base-kit/validator"

	// 注册后台管理 DTO 的中文校验消息（init 副作用）
	_ "github.com/leivorn/base-kit/validator/admin"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"gorm.io/gorm"
)

// Options 是下游项目对基底的全部扩展点。零值可用：不传任何回调就是一个纯后台管理服务。
type Options struct {
	// ConfigPath 配置文件路径，默认 "config.yaml"。Config 非空时忽略本字段。
	ConfigPath string
	// Config 直接注入配置，跳过读文件。测试用。
	Config *config.Config

	// Models 追加到 AutoMigrate 的下游模型
	Models []any
	// Seed 在基底种子数据之后执行，用来种下游的菜单与业务数据
	Seed func(db *gorm.DB)

	// PreRoutes 在基底 /admin 路由之前注册。Fiber 先注册先匹配，
	// 想覆盖基底某个接口就在这里注册同样的方法和路径。
	// 这里的路由排在 kit 的 JWTAuth / PermissionAuth / OperationLog 前面，命中后不会再经过它们，要自己挂。
	PreRoutes func(app *fiber.App)
	// Routes 在基底路由之后注册，放下游自己的业务路由。
	// 需要权限码的路由记得用 middleware.RegisterRoutePermissions 登记。
	// /admin 下的路由已经经过 kit 挂在 /admin 前缀上的 JWTAuth / PermissionAuth / OperationLog，
	// 不要再挂；/api 等其他前缀需要时自己挂。
	Routes func(app *fiber.App)

	// Swagger 仅在 config 的 enable_swagger 为 true 时调用，由下游挂载 UI。
	// 基底不依赖 swag：注解散落在各 handler，但生成物属于下游项目。
	Swagger func(app *fiber.App)

	// Fiber 在 fiber.New 之前调整配置（超时、Prefork 等）。
	Fiber func(cfg *fiber.Config)
}

// Bootstrap 加载配置、初始化校验器和数据库（含迁移与种子数据），但不创建 HTTP 服务。
// 只需要数据层的场景（定时任务、命令行工具）可以只调它。
func Bootstrap(opts Options) error {
	if opts.Config != nil {
		config.C = *opts.Config
	} else {
		path := opts.ConfigPath
		if path == "" {
			path = "config.yaml"
		}
		if err := config.Load(path); err != nil {
			return fmt.Errorf("加载配置 %s: %w", path, err)
		}
	}
	// 生产模式下占位或过短的 jwt.secret 直接拒绝启动：否则任何人都能伪造超管 token
	if err := config.ValidateProduction(); err != nil {
		return fmt.Errorf("%w（生成方式: openssl rand -base64 48）", err)
	}

	validator.Init()

	return store.Init(store.Options{Models: opts.Models, Seed: opts.Seed})
}

// NewApp 完成 Bootstrap 并组装好 fiber.App，但不监听端口。测试里用它拿到 app 直接发请求。
func NewApp(opts Options) (*fiber.App, error) {
	if err := Bootstrap(opts); err != nil {
		return nil, err
	}

	cfg := fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			msg := err.Error()
			if code >= fiber.StatusInternalServerError {
				// 5xx 的原文可能带 DB 错误、文件路径（recover 中间件把 panic 也转成这种 error）：
				// 日志里留全，生产模式对外只给一句通用的；开发模式保留原文方便调试
				log.Printf("[%d] %s %s: %v", code, c.Method(), c.Path(), err)
				if config.IsProduction() {
					msg = "Internal Server Error"
				}
			}
			return c.Status(code).JSON(fiber.Map{
				"code":    -1,
				"data":    nil,
				"error":   msg,
				"message": msg,
			})
		},
	}
	// 请求体上限走配置（server.body_limit_mb），未配置时保持 Fiber 默认 4MB
	if config.C.Server.BodyLimitMB > 0 {
		cfg.BodyLimit = config.C.Server.BodyLimitMB * 1024 * 1024
	}
	if opts.Fiber != nil {
		opts.Fiber(&cfg)
	}
	app := fiber.New(cfg)

	app.Use(recover.New())
	app.Use(logger.New())
	// gzip/brotli 压缩：客户端带 Accept-Encoding 才生效，公网/隧道场景收益明显
	app.Use(compress.New())

	corsOrigins := config.C.Server.CorsOrigins
	if corsOrigins == "" {
		corsOrigins = "*"
	}
	app.Use(cors.New(cors.Config{
		AllowOrigins: corsOrigins,
		AllowMethods: "GET,POST,PUT,DELETE,OPTIONS",
		AllowHeaders: "Origin,Content-Type,Accept,Authorization,Accept-Language",
		// 通配来源下浏览器不允许带凭证，配置了具体域名才打开
		AllowCredentials: corsOrigins != "*",
	}))

	if config.C.Server.EnableSwagger && opts.Swagger != nil {
		opts.Swagger(app)
	}

	// 顺序即优先级：PreRoutes 能覆盖基底路由，Routes 补充业务路由
	if opts.PreRoutes != nil {
		opts.PreRoutes(app)
	}
	router.SetupAdmin(app)
	if opts.Routes != nil {
		opts.Routes(app)
	}

	return app, nil
}

// Run 组装并启动服务，出错直接终止进程——它是 main 的最后一行，没有调用方能处理错误。
//
// 带子命令时只执行命令、不起服务：
//
//	<server> reset-password [用户名]   把账号（默认 super）重置为随机强口令并打印出来
//
// 用于超管忘记密码、或生产模式因弱口令被拦在登录页外（后台里没有别人能替它重置）。
// 在服务所在目录、以服务的运行用户执行，读同一份 config.yaml。
func Run(opts Options) {
	if len(os.Args) > 1 && os.Args[1] == "reset-password" {
		if err := resetPassword(opts, os.Args[2:]); err != nil {
			log.Fatalf("reset-password: %v", err)
		}
		return
	}

	app, err := NewApp(opts)
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	addr := fmt.Sprintf(":%d", config.C.Server.Port)
	log.Printf("Server starting on %s", addr)
	log.Fatal(app.Listen(addr))
}

func resetPassword(opts Options, args []string) error {
	username := "super"
	if len(args) > 0 && args[0] != "" {
		username = args[0]
	}
	if err := Bootstrap(opts); err != nil {
		return err
	}
	user, err := adminsvc.GetUserByUsername(username)
	if err != nil {
		return fmt.Errorf("找不到用户 %s: %w", username, err)
	}
	password := store.RandomPassword()
	if err := adminsvc.ChangePassword(user.ID, password); err != nil {
		return err
	}
	fmt.Printf("\n用户 %s 的密码已重置为: %s\n登录后请尽快在右上角「修改密码」改成自己的密码。\n", username, password)
	if user.Status != 1 {
		fmt.Printf("注意: 该账号当前是禁用状态，仍然登录不了。\n")
	}
	fmt.Printf("若该账号刚因多次登录失败被锁定，需等锁定结束（最长 15 分钟）或重启服务。\n")
	return nil
}
