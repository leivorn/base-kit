package admin

import "github.com/leivorn/base-kit/model"

type Role struct {
	model.BaseModel
	Name   string `json:"name" gorm:"uniqueIndex;size:64;not null"`
	Code   string `json:"code" gorm:"uniqueIndex;size:64;not null"`
	Status int    `json:"status" gorm:"comment:0=disabled 1=enabled"`
	Remark string `json:"remark" gorm:"size:256"`
	Menus  []Menu `json:"menus,omitempty" gorm:"many2many:role_menus;"`
}

func (Role) TableName() string {
	return "sys_roles"
}

type RoleMenu struct {
	RoleID uint `gorm:"primaryKey"`
	MenuID uint `gorm:"primaryKey"`
}

func (RoleMenu) TableName() string {
	return "role_menus"
}
