package admin

import (
	"time"

	"github.com/leivorn/base-kit/model"
)

// modelBase 构造带指定创建时间的 BaseModel；GORM 在 Create 时保留非零的 CreatedAt。
func modelBase(createdAt time.Time) model.BaseModel {
	return model.BaseModel{CreatedAt: createdAt, UpdatedAt: createdAt}
}
