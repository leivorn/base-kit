package admin

import "github.com/leivorn/base-kit/model"

type Config struct {
	model.BaseModel
	ConfigName  string `json:"configName" gorm:"size:128"`
	ConfigKey   string `json:"configKey" gorm:"uniqueIndex;not null;size:128"`
	ConfigValue string `json:"configValue" gorm:"type:text"`
	ConfigGroup string `json:"configGroup" gorm:"size:64;index"`
	Remark      string `json:"remark" gorm:"size:256"`
	Status      int    `json:"status"`
}

func (Config) TableName() string {
	return "sys_configs"
}
