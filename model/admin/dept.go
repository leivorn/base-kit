package admin

import "github.com/leivorn/base-kit/model"

type Dept struct {
	model.BaseModel
	ParentID uint   `json:"pid" gorm:"default:0"`
	Name     string `json:"name" gorm:"size:64;not null"`
	OrderNo  int    `json:"order" gorm:"default:0"`
	Status   int    `json:"status"`
	Remark   string `json:"remark" gorm:"size:256"`
}

func (Dept) TableName() string {
	return "sys_depts"
}
