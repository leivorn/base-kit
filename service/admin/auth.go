package admin

import (
	"errors"
	"log"
	"time"

	"github.com/leivorn/base-kit/config"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"golang.org/x/crypto/bcrypt"
)

func Authenticate(username, password string) (*adminmodel.User, error) {
	var user adminmodel.User
	if err := store.DB.Preload("Roles").Where("username = ? AND status = 1", username).First(&user).Error; err != nil {
		return nil, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
		return nil, err
	}
	// 生产环境不认默认种子密码和弱口令：哪怕库里的哈希对得上，也当作登录失败，
	// 逼运维先把口令改合格（超管被拦时用 `<server> reset-password super` 重置）
	if config.IsProduction() {
		if password == store.DefaultSeedPassword {
			log.Printf("[auth] WARN: login rejected for %q: account still uses the default seed password", username)
			return nil, ErrDefaultPassword
		}
		if !passwordMeetsBaseline(password, user.Username) {
			log.Printf("[auth] WARN: login rejected for %q: password does not meet the strength policy; reset it in the admin UI (or `<server> reset-password %s`)", username, user.Username)
			return nil, ErrWeakPassword
		}
	}
	return &user, nil
}

// ErrDefaultPassword 生产环境用默认种子密码登录（对外与密码错误同一提示）。
var ErrDefaultPassword = errors.New("default seed password is not allowed in production")

// ErrWeakPassword 生产环境用不达标的弱口令登录（对外与密码错误同一提示，不泄露"密码其实是对的"）。
var ErrWeakPassword = errors.New("weak password is not allowed in production")

func GetUserByID(id uint) (*adminmodel.User, error) {
	var user adminmodel.User
	if err := store.DB.Preload("Roles").First(&user, id).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func GetUserByUsername(username string) (*adminmodel.User, error) {
	var user adminmodel.User
	if err := store.DB.Preload("Roles").Where("username = ?", username).First(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}

func VerifyPassword(user *adminmodel.User, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password))
}

// ChangePassword 更新口令并记录改密时间，JWTAuth 据此让改密前签发的 token 失效。
func ChangePassword(userID uint, newPassword string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := time.Now()
	return store.DB.Model(&adminmodel.User{}).Where("id = ?", userID).Updates(map[string]any{
		"password":            string(hash),
		"password_changed_at": now,
	}).Error
}

// ActiveRoles 返回用户已启用（status=1）的角色。
// 权限判定（roleHasPermissionCode）只认启用角色，菜单、权限码、角色 claim 也必须
// 用同一口径，否则被禁用角色的用户前端仍看得到菜单和按钮，点击才 403。
func ActiveRoles(user *adminmodel.User) []adminmodel.Role {
	roles := make([]adminmodel.Role, 0, len(user.Roles))
	for _, r := range user.Roles {
		if r.Status == 1 {
			roles = append(roles, r)
		}
	}
	return roles
}

// IsSuperUser 判断用户是否为内置超管（id=1）或持有启用的 super 角色。
// PermissionAuth 对二者全量放行，菜单、权限码、首页必须用同一口径，
// 否则第二个 super 账号看不到界面上新建的菜单（新菜单不会自动挂到 super 角色上）。
func IsSuperUser(user *adminmodel.User) bool {
	if user.ID == 1 {
		return true
	}
	for _, r := range ActiveRoles(user) {
		if r.Code == "super" {
			return true
		}
	}
	return false
}

// ActiveRoleIDs 返回用户已启用角色的 ID 列表。
func ActiveRoleIDs(user *adminmodel.User) []uint {
	active := ActiveRoles(user)
	ids := make([]uint, len(active))
	for i, r := range active {
		ids[i] = r.ID
	}
	return ids
}

// GetRoleNames 返回用户已启用角色的 code 列表（写进 JWT、返回给前端）。
func GetRoleNames(user *adminmodel.User) []string {
	active := ActiveRoles(user)
	names := make([]string, len(active))
	for i, r := range active {
		names[i] = r.Code
	}
	return names
}

func GetAccessCodes(user *adminmodel.User) []string {
	var codes []string
	var menus []adminmodel.Menu

	if IsSuperUser(user) {
		store.DB.Where("auth_code != ''").Find(&menus)
	} else {
		roleIDs := ActiveRoleIDs(user)
		if len(roleIDs) == 0 {
			return nil
		}
		store.DB.
			Joins("JOIN role_menus ON role_menus.menu_id = sys_menus.id").
			Where("role_menus.role_id IN ? AND sys_menus.auth_code != ''", roleIDs).
			Find(&menus)
	}

	seen := make(map[string]bool)
	for _, m := range menus {
		if m.AuthCode != "" && !seen[m.AuthCode] {
			codes = append(codes, m.AuthCode)
			seen[m.AuthCode] = true
		}
	}
	return codes
}
