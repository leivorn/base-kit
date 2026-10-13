package middleware

import (
	"sync"
	"time"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"
)

// 用户鉴权信息缓存：JWTAuth 每个请求都要确认用户状态和当前角色，直接查库会把两次
// 查询摊到所有接口上。角色/状态是低频变更数据，用带 TTL 的进程内缓存；用户、角色变更
// 时由 handler 调 InvalidateUserAuthCache 立即失效，不必等 TTL。
// 与 permCache 同一套写法，只是 key 换成 userID。
const userAuthCacheTTL = time.Minute

type userAuthEntry struct {
	username          string
	status            int
	roles             []string // 仅 status=1 的角色 code；始终非 nil，PermissionAuth 断言 []string
	passwordChangedAt *time.Time
	expiresAt         time.Time
}

var (
	userAuthMu    sync.RWMutex
	userAuthCache = map[uint]userAuthEntry{}
	// 失效代次：Invalidate 时递增；loadUserAuth 只在查库前后代次一致时才回填缓存，
	// 防止"读到旧快照 → 别处提交并失效 → 旧快照回填"把失效抹掉、让旧 token 再活一个 TTL
	userAuthGen    = map[uint]uint64{}
	userAuthGenAll uint64
)

// InvalidateUserAuthCache 让指定用户的缓存失效；userID 为 0 时清空全部。
func InvalidateUserAuthCache(userID uint) {
	userAuthMu.Lock()
	defer userAuthMu.Unlock()
	if userID == 0 {
		userAuthCache = map[uint]userAuthEntry{}
		userAuthGenAll++
		return
	}
	delete(userAuthCache, userID)
	userAuthGen[userID]++
}

// loadUserAuth 返回用户的鉴权信息。用户不存在返回 gorm.ErrRecordNotFound；
// 其他数据库错误原样返回，调用方不得把它当成鉴权失败（否则一次库抖动就把人踢下线）。
func loadUserAuth(userID uint) (userAuthEntry, error) {
	userAuthMu.RLock()
	entry, ok := userAuthCache[userID]
	gen, genAll := userAuthGen[userID], userAuthGenAll
	userAuthMu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry, nil
	}

	var user adminmodel.User
	if err := store.DB.Select("id", "username", "status", "password_changed_at").First(&user, userID).Error; err != nil {
		return userAuthEntry{}, err
	}

	var codes []string
	if err := store.DB.Model(&adminmodel.Role{}).
		Joins("JOIN user_roles ON user_roles.role_id = sys_roles.id").
		Where("user_roles.user_id = ? AND sys_roles.status = ?", userID, 1).
		Pluck("sys_roles.code", &codes).Error; err != nil {
		return userAuthEntry{}, err
	}
	if codes == nil {
		codes = []string{}
	}

	entry = userAuthEntry{
		username:          user.Username,
		status:            user.Status,
		roles:             codes,
		passwordChangedAt: user.PasswordChangedAt,
		expiresAt:         time.Now().Add(userAuthCacheTTL),
	}
	userAuthMu.Lock()
	if userAuthGen[userID] == gen && userAuthGenAll == genAll {
		userAuthCache[userID] = entry
	}
	userAuthMu.Unlock()
	return entry, nil
}
