package admin

import "github.com/leivorn/base-kit/model"

type Menu struct {
	model.BaseModel
	ParentID  uint   `json:"pid" gorm:"default:0;comment:parent menu id"`
	Name      string `json:"name" gorm:"size:64;not null"`
	Path      string `json:"path" gorm:"size:128"`
	Component string `json:"component" gorm:"size:128"`
	Redirect  string `json:"redirect,omitempty" gorm:"size:128"`
	Type      string `json:"type" gorm:"size:16;not null;comment:catalog|menu|button|embedded|link"`
	Icon      string `json:"icon" gorm:"size:64"`
	Title     string `json:"title" gorm:"size:64;not null"`
	AuthCode  string `json:"authCode,omitempty" gorm:"size:64;index"`
	OrderNo   int    `json:"order" gorm:"default:0"`
	Status    int    `json:"status" gorm:"comment:0=disabled 1=enabled"`
	KeepAlive bool   `json:"keepAlive" gorm:"default:false"`
	AffixTab  bool   `json:"affixTab" gorm:"default:false"`
	IframeSrc string `json:"iframeSrc,omitempty" gorm:"size:256"`
	Link      string `json:"link,omitempty" gorm:"size:256"`

	// Vben Admin 路由 meta 装饰字段
	ActiveIcon         string `json:"activeIcon,omitempty" gorm:"size:64;comment:激活态图标"`
	ActivePath         string `json:"activePath,omitempty" gorm:"size:128;comment:激活父菜单的路径"`
	HideInMenu         bool   `json:"hideInMenu" gorm:"default:false;comment:在侧边菜单中隐藏"`
	HideInBreadcrumb   bool   `json:"hideInBreadcrumb" gorm:"default:false;comment:在面包屑中隐藏"`
	HideInTab          bool   `json:"hideInTab" gorm:"default:false;comment:在标签栏中隐藏"`
	HideChildrenInMenu bool   `json:"hideChildrenInMenu" gorm:"default:false;comment:隐藏子菜单"`
	BadgeType          string `json:"badgeType,omitempty" gorm:"size:16;comment:dot|normal"`
	Badge              string `json:"badge,omitempty" gorm:"size:32;comment:徽章内容"`
	BadgeVariants      string `json:"badgeVariants,omitempty" gorm:"size:32;comment:default|primary|destructive|success|warning"`

	Children []Menu `json:"children,omitempty" gorm:"-"`
}

func (Menu) TableName() string {
	return "sys_menus"
}
