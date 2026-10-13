package middleware

import (
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
)

// RoutePermission 把一条路由映射到菜单权限码：请求命中 Method+Path 时，
// 要求当前用户的任一启用角色在 role_menus 里关联了 auth_code == Code 的启用菜单。
// Path 支持 :param 段（与 Fiber 路由写法一致）。
type RoutePermission struct {
	Method string
	Path   string
	Code   string
}

// compiledRoute 由 init/Register* 预编译正则；热路径上不再现编。
type compiledRoute struct {
	RoutePermission
	re *regexp.Regexp
}

// compileRoutePattern 把 "/admin/system/user/:id" 编译成 ^/admin/system/user/[^/]+$
// （路由表里只用了字面段和 :param 两种形式）。
func compileRoutePattern(path string) *regexp.Regexp {
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if strings.HasPrefix(seg, ":") && len(seg) > 1 {
			segs[i] = "[^/]+"
		} else {
			segs[i] = regexp.QuoteMeta(seg)
		}
	}
	return regexp.MustCompile("^" + strings.Join(segs, "/") + "$")
}

var (
	// 仅需登录、不校验权限码的路由
	authenticatedRoutes []compiledRoute
	// 需要权限码的路由；未登记的 /admin 路由对非 super 一律 403（默认拒绝）
	routePermissions []compiledRoute
)

var baseAuthenticatedRoutes = []RoutePermission{
	{Method: "GET", Path: "/admin/auth/codes"},
	{Method: "POST", Path: "/admin/auth/change-password"},
	{Method: "GET", Path: "/admin/user/info"},
	{Method: "GET", Path: "/admin/menu/all"},
}

var baseRoutePermissions = []RoutePermission{
	{Method: "GET", Path: "/admin/system/role/all", Code: "System:Role:List"},
	{Method: "GET", Path: "/admin/system/role/list", Code: "System:Role:List"},
	// 角色管理专用菜单树仅供"角色管理"页面使用，复用 System:Role:List 权限码：
	// 拥有角色管理列表权限即可调用，不强依赖"菜单管理"权限。
	{Method: "GET", Path: "/admin/system/role/menu-tree", Code: "System:Role:List"},
	{Method: "POST", Path: "/admin/system/role", Code: "System:Role:Create"},
	{Method: "PUT", Path: "/admin/system/role/:id", Code: "System:Role:Edit"},
	{Method: "DELETE", Path: "/admin/system/role/:id", Code: "System:Role:Delete"},
	{Method: "GET", Path: "/admin/system/menu/list", Code: "System:Menu:List"},
	{Method: "GET", Path: "/admin/system/menu/name-exists", Code: "System:Menu:List"},
	{Method: "GET", Path: "/admin/system/menu/path-exists", Code: "System:Menu:List"},
	{Method: "POST", Path: "/admin/system/menu", Code: "System:Menu:Create"},
	{Method: "PUT", Path: "/admin/system/menu/:id", Code: "System:Menu:Edit"},
	{Method: "DELETE", Path: "/admin/system/menu/:id", Code: "System:Menu:Delete"},
	{Method: "GET", Path: "/admin/system/dept/list", Code: "System:Dept:List"},
	{Method: "POST", Path: "/admin/system/dept", Code: "System:Dept:Create"},
	{Method: "PUT", Path: "/admin/system/dept/:id", Code: "System:Dept:Edit"},
	{Method: "DELETE", Path: "/admin/system/dept/:id", Code: "System:Dept:Delete"},
	{Method: "GET", Path: "/admin/system/user/list", Code: "System:User:List"},
	{Method: "POST", Path: "/admin/system/user", Code: "System:User:Create"},
	{Method: "PUT", Path: "/admin/system/user/:id", Code: "System:User:Edit"},
	{Method: "DELETE", Path: "/admin/system/user/:id", Code: "System:User:Delete"},
	{Method: "GET", Path: "/admin/system/config/list", Code: "System:Config:List"},
	{Method: "GET", Path: "/admin/system/config/groups", Code: "System:Config:List"},
	{Method: "POST", Path: "/admin/system/config", Code: "System:Config:Create"},
	{Method: "PUT", Path: "/admin/system/config/:id", Code: "System:Config:Edit"},
	{Method: "DELETE", Path: "/admin/system/config/:id", Code: "System:Config:Delete"},
	{Method: "GET", Path: "/admin/system/operation-log/list", Code: "System:OperationLog:List"},
	{Method: "DELETE", Path: "/admin/system/operation-log/clear", Code: "System:OperationLog:Delete"},
	{Method: "DELETE", Path: "/admin/system/operation-log/:id", Code: "System:OperationLog:Delete"},
}

func init() {
	RegisterAuthenticatedRoutes(baseAuthenticatedRoutes...)
	RegisterRoutePermissions(baseRoutePermissions...)
}

// RegisterRoutePermissions 登记需要权限码的路由。下游项目在 router/project.go 里
// 注册业务路由时一并调用，例如：
//
//	middleware.RegisterRoutePermissions(
//	    middleware.RoutePermission{Method: "GET", Path: "/admin/shop/order/list", Code: "Shop:Order:List"},
//	    middleware.RoutePermission{Method: "PUT", Path: "/admin/shop/order/:id", Code: "Shop:Order:Edit"},
//	)
//
// 未登记的受保护路由对非 super 用户一律 403。须在 app.Listen 之前调用（不加锁）。
func RegisterRoutePermissions(perms ...RoutePermission) {
	for _, p := range perms {
		routePermissions = append(routePermissions, compiledRoute{RoutePermission: p, re: compileRoutePattern(p.Path)})
	}
}

// RegisterAuthenticatedRoutes 登记只需登录、不校验权限码的路由（Code 字段忽略）。
func RegisterAuthenticatedRoutes(routes ...RoutePermission) {
	for _, r := range routes {
		authenticatedRoutes = append(authenticatedRoutes, compiledRoute{RoutePermission: r, re: compileRoutePattern(r.Path)})
	}
}

// PermissionAuth 基于"路由 → 权限码 → 角色菜单"的 RBAC 鉴权，挂在 JWTAuth 之后。
// 判定顺序：内置超管(id=1) 或持 super 角色 → 放行；仅需登录的路由 → 放行；
// 路由登记了权限码且用户角色持有该码 → 放行；其余 403。
// 角色来自 JWTAuth 从数据库装配的 Locals("roles")，不重复查库。
//
// kit 已把它挂在 /admin 前缀上，/admin 下的路由不用再挂；同一请求对同一 method+path 只判定一次，
// 内部重定向（c.Path 改写后 RestartRouting）到别的路由会重新判定，见 request_once.go。
func PermissionAuth() fiber.Handler {
	return func(c *fiber.Ctx) error {
		obj := c.Path()
		act := c.Method()
		// 拼出来的是新字符串。c.Path() 指向 c.Path(override) 会原地改写的缓冲区，直接存它，
		// 重定向到等长路径后读出来就是新路径，判定会被跳过
		route := act + " " + obj
		if c.Locals(permissionGranted) == route {
			return c.Next()
		}

		if isSuperAdminUser(c) {
			return grantPermission(c, route)
		}

		roles, ok := c.Locals("roles").([]string)
		if !ok {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"code":    -1,
				"data":    nil,
				"error":   "Forbidden",
				"message": "Forbidden",
			})
		}

		if hasRole(roles, "super") {
			return grantPermission(c, route)
		}

		if matchesRoute(authenticatedRoutes, act, obj) {
			return grantPermission(c, route)
		}

		code, ok := permissionCodeForRoute(act, obj)
		if ok && roleHasPermissionCode(roles, code) {
			return grantPermission(c, route)
		}

		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
			"code":    -1,
			"data":    nil,
			"error":   "Forbidden",
			"message": "没有权限执行此操作",
		})
	}
}

// grantPermission 记下本请求已按 route 放行，再往下走。
func grantPermission(c *fiber.Ctx, route string) error {
	c.Locals(permissionGranted, route)
	return c.Next()
}

// CasbinAuth 是 PermissionAuth 的旧名。
//
// Deprecated: 请使用 PermissionAuth。保留只为让下游 router/project.go 里已有的
// 引用在 sync-base 后无需改动；本函数不会被移除。
func CasbinAuth() fiber.Handler {
	return PermissionAuth()
}

func isSuperAdminUser(c *fiber.Ctx) bool {
	switch userID := c.Locals("userId").(type) {
	case uint:
		return userID == 1
	case uint64:
		return userID == 1
	case int:
		return userID == 1
	case int64:
		return userID == 1
	case float64:
		return userID == 1
	default:
		return false
	}
}

// OperatorIsSuper 判断当前请求者是否具备超级管理员身份：内置超管 user（id=1）或
// 持有 code=super 的角色。供角色/用户管理 handler 用来限制"授予 super 角色"这类高危操作，
// 防止普通管理员把自己或他人提升到不受权限体系约束的 super 层。
func OperatorIsSuper(c *fiber.Ctx) bool {
	if isSuperAdminUser(c) {
		return true
	}
	roles, _ := c.Locals("roles").([]string)
	return hasRole(roles, "super")
}

func hasRole(roles []string, code string) bool {
	for _, role := range roles {
		if role == code {
			return true
		}
	}
	return false
}

func matchesRoute(routes []compiledRoute, method string, path string) bool {
	for _, route := range routes {
		if route.Method == method && route.re.MatchString(path) {
			return true
		}
	}
	return false
}

func permissionCodeForRoute(method string, path string) (string, bool) {
	for _, route := range routePermissions {
		if route.Method == method && route.re.MatchString(path) {
			return route.Code, true
		}
	}
	return "", false
}

// 权限码查询缓存：每个受权限保护的请求都要做一次 3 表 JOIN，
// 实测把接口吞吐拉低近一半（9.7k -> 5.6k req/s）。角色-菜单映射是低频变更数据，
// 用带 TTL 的进程内缓存，角色/菜单变更时由 handler 调 InvalidatePermissionCache 立即失效。
const permCacheTTL = time.Minute

type permCacheEntry struct {
	allowed   bool
	expiresAt time.Time
}

var (
	permCacheMu sync.RWMutex
	permCache   = map[string]permCacheEntry{}
	// 失效代次：Invalidate 时递增，查库前后代次不一致就放弃回填，
	// 防止"读到旧快照 → 别处提交并失效 → 旧快照回填"把失效抹掉
	permCacheGen uint64
)

func permCacheKey(roles []string, code string) string {
	sorted := append([]string(nil), roles...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",") + "|" + code
}

// InvalidatePermissionCache 清空权限码缓存。
// 角色、菜单的增删改都会改变角色-菜单映射，对应 handler 必须调用本函数，
// 保证权限调整即时生效，而不是等 TTL 过期。
func InvalidatePermissionCache() {
	permCacheMu.Lock()
	permCache = map[string]permCacheEntry{}
	permCacheGen++
	permCacheMu.Unlock()
}

func roleHasPermissionCode(roles []string, code string) bool {
	if len(roles) == 0 || code == "" {
		return false
	}

	key := permCacheKey(roles, code)
	permCacheMu.RLock()
	entry, ok := permCache[key]
	gen := permCacheGen
	permCacheMu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.allowed
	}

	var count int64
	err := store.DB.Model(&adminmodel.Menu{}).
		Joins("JOIN role_menus ON role_menus.menu_id = sys_menus.id").
		Joins("JOIN sys_roles ON sys_roles.id = role_menus.role_id").
		Where("sys_roles.code IN ? AND sys_roles.status = ?", roles, 1).
		Where("sys_menus.auth_code = ? AND sys_menus.status = ?", code, 1).
		Count(&count).Error
	if err != nil {
		// 查询失败按无权限处理，但不缓存，避免一次抖动把用户锁一分钟
		log.Printf("[permission] query failed: %v", err)
		return false
	}

	allowed := count > 0
	permCacheMu.Lock()
	if permCacheGen == gen {
		permCache[key] = permCacheEntry{allowed: allowed, expiresAt: time.Now().Add(permCacheTTL)}
	}
	permCacheMu.Unlock()
	return allowed
}
