package admin

import (
	"strconv"

	"github.com/leivorn/base-kit/dto"
	admindto "github.com/leivorn/base-kit/dto/admin"
	"github.com/leivorn/base-kit/middleware"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"

	"github.com/gofiber/fiber/v2"
)

// GetOperationLogList 获取操作日志列表
// @Summary 获取操作日志列表
// @Description 分页查询操作日志，支持按用户名、请求方法、路径和状态筛选
// @Tags 系统管理 - 操作日志
// @Produce json
// @Security BearerAuth
// @Param page query int false "页码" default(1)
// @Param pageSize query int false "每页数量，最大 200" default(20)
// @Param username query string false "操作用户(模糊搜索)"
// @Param method query string false "请求方法: GET/POST/PUT/DELETE"
// @Param path query string false "请求路径(模糊搜索)"
// @Param status query string false "响应状态码"
// @Success 200 {object} dto.Response{data=dto.PageData{items=[]admindto.OperationLogItem}}
// @Router /admin/system/operation-log/list [get]
func GetOperationLogList(c *fiber.Ctx) error {
	page, pageSize := dto.ParsePage(c)
	username := c.Query("username")
	method := c.Query("method")
	path := c.Query("path")
	status := c.Query("status")

	var logs []adminmodel.OperationLog
	var total int64
	query := store.DB.Model(&adminmodel.OperationLog{})

	if username != "" {
		query = query.Where("username LIKE ?", "%"+username+"%")
	}
	if method != "" {
		query = query.Where("method = ?", method)
	}
	if path != "" {
		query = query.Where("path LIKE ?", "%"+path+"%")
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}

	if err := query.Count(&total).Error; err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to get operation logs")
	}
	offset := (page - 1) * pageSize
	if err := query.Order("id DESC").Offset(offset).Limit(pageSize).Find(&logs).Error; err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to get operation logs")
	}

	items := make([]admindto.OperationLogItem, len(logs))
	for i, l := range logs {
		items[i] = admindto.OperationLogItem{
			ID:         l.ID,
			Username:   l.Username,
			Method:     l.Method,
			Path:       l.Path,
			Status:     l.Status,
			Duration:   l.Duration,
			IP:         l.IP,
			UserAgent:  l.UserAgent,
			CreateTime: l.CreatedAt.Format("2006/01/02 15:04:05"),
		}
	}
	return dto.PageSuccess(c, items, total)
}

// DeleteOperationLog 删除操作日志
// @Summary 删除操作日志
// @Tags 系统管理 - 操作日志
// @Produce json
// @Security BearerAuth
// @Description 仅超级管理员可删除（审计记录不应由被审计的人抹掉）
// @Param id path int true "日志ID"
// @Success 200 {object} dto.Response
// @Failure 403 {object} dto.Response
// @Failure 404 {object} dto.Response
// @Router /admin/system/operation-log/{id} [delete]
func DeleteOperationLog(c *fiber.Ctx) error {
	// 审计记录只有超管能删：持有删除权限码的普通管理员也不行，否则可以先操作、再抹掉痕迹。
	// 日常清理交给 server.op_log_retention_days 按保留期自动删
	if !middleware.OperatorIsSuper(c) {
		return dto.Fail(c, fiber.StatusForbidden, msgOperationLogSuperOnly)
	}
	id, _ := strconv.ParseUint(c.Params("id"), 10, 64)
	res := store.DB.Delete(&adminmodel.OperationLog{}, id)
	if res.Error != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to delete operation log")
	}
	if res.RowsAffected == 0 {
		return dto.Fail(c, fiber.StatusNotFound, "Operation log not found")
	}
	return dto.Success(c, nil)
}

// ClearOperationLog 清空操作日志
// @Summary 清空操作日志
// @Tags 系统管理 - 操作日志
// @Produce json
// @Security BearerAuth
// @Description 仅超级管理员可清空；日常清理请用配置 server.op_log_retention_days 按保留期自动删除
// @Success 200 {object} dto.Response
// @Failure 403 {object} dto.Response
// @Router /admin/system/operation-log/clear [delete]
func ClearOperationLog(c *fiber.Ctx) error {
	// 审计记录只有超管能删：持有删除权限码的普通管理员也不行，否则可以先操作、再抹掉痕迹。
	// 日常清理交给 server.op_log_retention_days 按保留期自动删
	if !middleware.OperatorIsSuper(c) {
		return dto.Fail(c, fiber.StatusForbidden, msgOperationLogSuperOnly)
	}
	if err := store.DB.Where("1 = 1").Delete(&adminmodel.OperationLog{}).Error; err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to clear operation logs")
	}
	return dto.Success(c, nil)
}

const msgOperationLogSuperOnly = "只有超级管理员可以删除操作日志"
