package router

import (
	admin "github.com/leivorn/base-kit/handler/admin"
	"github.com/leivorn/base-kit/middleware"

	"github.com/gofiber/fiber/v2"
)

// SetupAdmin registers all /admin/* routes (backend management).
//
// JWTAuth、PermissionAuth、OperationLog 挂在 /admin 前缀上：Fiber 的分组带 handler、Group.Use 注册的
// 都是前缀中间件，不只管下面这些路由。之后注册的 /admin 路由——包括下游 basekit.Options.Routes 里的
// 业务路由——不挂也会鉴权、校验权限码、记操作日志，下游不要再挂一遍（重复挂载同一请求也只生效一次，
// 见 middleware/request_once.go）。前缀按字符串匹配，/admin-xxx 这类路径同样会经过它们。
//
// 在它们之前注册、命中后不再往下走的路由不经过这三个中间件：/admin/auth 下的登录/登出/刷新，
// 和 Options.PreRoutes 里的路由（覆盖 kit 接口时要自己挂）。/api 等其他前缀也要自己挂。
func SetupAdmin(app *fiber.App) {
	g := app.Group("/admin")

	// Public auth routes
	auth := g.Group("/auth")
	auth.Get("/captcha", admin.GetCaptcha)
	auth.Post("/captcha/verify", admin.VerifyCaptcha)
	auth.Post("/login", admin.Login)
	auth.Post("/logout", admin.Logout)
	auth.Post("/refresh", admin.RefreshToken)

	// Protected routes: JWTAuth 装配用户/角色，PermissionAuth 按 middleware/permission.go 的路由表校验权限码
	protected := g.Group("", middleware.JWTAuth(), middleware.PermissionAuth())

	// Operation log middleware (records POST/PUT/DELETE)
	protected.Use(middleware.OperationLog())

	// Auth info
	protected.Get("/auth/codes", admin.GetAccessCodes)
	protected.Post("/auth/change-password", admin.ChangePassword)

	// User info
	protected.Get("/user/info", admin.GetUserInfo)

	// Menu routes (for frontend routing)
	protected.Get("/menu/all", admin.GetAllMenus)

	// System management
	sys := protected.Group("/system")

	// Roles
	sys.Get("/role/all", admin.GetAllRoles)
	sys.Get("/role/list", admin.GetRoleList)
	// 角色管理专用菜单树：不依赖"菜单管理"权限，便于在没有菜单管理权限时仍能完成角色授权
	sys.Get("/role/menu-tree", admin.GetRoleMenuTree)
	sys.Post("/role", admin.CreateRole)
	sys.Put("/role/:id", admin.UpdateRole)
	sys.Delete("/role/:id", admin.DeleteRole)

	// Menus management
	sys.Get("/menu/list", admin.GetMenuList)
	sys.Get("/menu/name-exists", admin.CheckMenuNameExists)
	sys.Get("/menu/path-exists", admin.CheckMenuPathExists)
	sys.Post("/menu", admin.CreateMenu)
	sys.Put("/menu/:id", admin.UpdateMenu)
	sys.Delete("/menu/:id", admin.DeleteMenu)

	// Departments
	sys.Get("/dept/list", admin.GetDeptList)
	sys.Post("/dept", admin.CreateDept)
	sys.Put("/dept/:id", admin.UpdateDept)
	sys.Delete("/dept/:id", admin.DeleteDept)

	// Users management
	sys.Get("/user/list", admin.GetUserList)
	sys.Post("/user", admin.CreateUser)
	sys.Put("/user/:id", admin.UpdateUser)
	sys.Delete("/user/:id", admin.DeleteUser)

	// Configs management
	sys.Get("/config/list", admin.GetConfigList)
	sys.Get("/config/groups", admin.GetConfigGroups)
	sys.Post("/config", admin.CreateConfig)
	sys.Put("/config/:id", admin.UpdateConfig)
	sys.Delete("/config/:id", admin.DeleteConfig)

	// Operation logs
	sys.Get("/operation-log/list", admin.GetOperationLogList)
	sys.Delete("/operation-log/clear", admin.ClearOperationLog)
	sys.Delete("/operation-log/:id", admin.DeleteOperationLog)
}
