package admin

import (
	"errors"
	"time"

	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

var ErrSuperAdminProtected = errors.New("超级管理员不可修改或删除")

type UserListParams struct {
	Page     int
	PageSize int
	Username string
	Status   *int
}

func GetUserList(params UserListParams) ([]adminmodel.User, int64, error) {
	var users []adminmodel.User
	var total int64

	query := store.DB.Model(&adminmodel.User{}).Preload("Roles").Where("id != ?", 1)

	if params.Username != "" {
		query = query.Where("username LIKE ?", "%"+params.Username+"%")
	}
	if params.Status != nil {
		query = query.Where("status = ?", *params.Status)
	}

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (params.Page - 1) * params.PageSize
	if err := query.Offset(offset).Limit(params.PageSize).Find(&users).Error; err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// RolesIncludeSuperCode 判断给定角色 ID 列表里是否包含 code=super 的角色。
// 用于阻止普通管理员通过创建/更新用户把 super 角色分配出去（super 越权防线之一）。
func RolesIncludeSuperCode(roleIDs []uint) bool {
	if len(roleIDs) == 0 {
		return false
	}
	var count int64
	store.DB.Model(&adminmodel.Role{}).Where("id IN ? AND code = ?", roleIDs, "super").Count(&count)
	return count > 0
}

// UserHasSuperRole 判断用户当前是否持有 code=super 的角色。
// 普通管理员不得修改/删除这类用户（否则可以给对方重置密码后登录，或剥掉其 super 角色），
// 与 id=1 的内置超管保护是同一道防线的两个入口。
func UserHasSuperRole(id uint) bool {
	if id == 0 {
		return false
	}
	var count int64
	store.DB.Model(&adminmodel.Role{}).
		Joins("JOIN user_roles ON user_roles.role_id = sys_roles.id").
		Where("user_roles.user_id = ? AND sys_roles.code = ?", id, "super").
		Count(&count)
	return count > 0
}

func CreateUser(user *adminmodel.User, roleIDs []uint) error {
	if err := CheckPasswordStrength(user.Password, user.Username); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	user.Password = string(hash)

	if err := store.DB.Create(user).Error; err != nil {
		return err
	}
	if len(roleIDs) > 0 {
		var roles []adminmodel.Role
		store.DB.Where("id IN ?", roleIDs).Find(&roles)
		return store.DB.Model(user).Association("Roles").Replace(roles)
	}
	return nil
}

func UpdateUser(id uint, updates map[string]any, roleIDs []uint) error {
	if id == 1 {
		return ErrSuperAdminProtected
	}
	var user adminmodel.User
	if err := store.DB.First(&user, id).Error; err != nil {
		return err
	}

	if pwd, ok := updates["password"].(string); ok && pwd != "" {
		if err := CheckPasswordStrength(pwd, user.Username); err != nil {
			return err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pwd), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		updates["password"] = string(hash)
		// 管理员重置口令同样让该用户的旧 token 失效
		updates["password_changed_at"] = time.Now()
	} else {
		delete(updates, "password")
	}

	if err := store.DB.Model(&adminmodel.User{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return err
	}
	if roleIDs != nil {
		var roles []adminmodel.Role
		if len(roleIDs) > 0 {
			store.DB.Where("id IN ?", roleIDs).Find(&roles)
		}
		return store.DB.Model(&user).Association("Roles").Replace(roles)
	}
	return nil
}

func NewUser(username, password, realName, email, phone string, status int, remark string) *adminmodel.User {
	return &adminmodel.User{
		Username: username,
		Password: password,
		RealName: realName,
		Email:    email,
		Phone:    phone,
		Status:   status,
		Remark:   remark,
	}
}

// DeleteUser 物理删除用户及其角色关联。
// 用户名带唯一索引，软删除会让被删的用户名永远无法再用；操作日志按值记录 username，
// 不依赖 sys_users 行，因此直接硬删。
func DeleteUser(id uint) error {
	if id == 1 {
		return ErrSuperAdminProtected
	}
	return store.DB.Transaction(func(tx *gorm.DB) error {
		// 先删关联行再删主行：user_roles 对 sys_users 有外键，MySQL/PostgreSQL 会按顺序校验
		if err := tx.Where("user_id = ?", id).Delete(&adminmodel.UserRole{}).Error; err != nil {
			return err
		}
		res := tx.Unscoped().Delete(&adminmodel.User{}, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
}
