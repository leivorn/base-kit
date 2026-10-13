package admin

import (
	"strconv"

	"github.com/leivorn/base-kit/dto"
	admindto "github.com/leivorn/base-kit/dto/admin"
	adminmodel "github.com/leivorn/base-kit/model/admin"
	"github.com/leivorn/base-kit/store"
	"github.com/leivorn/base-kit/validator"

	"github.com/gofiber/fiber/v2"
	"gorm.io/gorm"
)

// GetDeptList 获取部门列表
// @Summary 获取部门树形列表
// @Description 返回所有部门的树形结构
// @Tags 系统管理 - 部门
// @Produce json
// @Security BearerAuth
// @Success 200 {object} dto.Response{data=[]object}
// @Failure 401 {object} dto.Response
// @Router /admin/system/dept/list [get]
func GetDeptList(c *fiber.Ctx) error {
	var depts []adminmodel.Dept
	store.DB.Order("order_no ASC").Find(&depts)
	tree := buildDeptTree(depts, 0)
	return dto.Success(c, tree)
}

// CreateDept 创建部门
// @Summary 创建部门
// @Description 创建新部门
// @Tags 系统管理 - 部门
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body admindto.DeptRequest true "部门信息"
// @Success 200 {object} dto.Response{data=dto.IDResponse}
// @Failure 400 {object} dto.Response
// @Router /admin/system/dept [post]
func CreateDept(c *fiber.Ctx) error {
	var req admindto.DeptRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}

	if ok, err := parentExists(store.DB, &adminmodel.Dept{}, req.ParentID); err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to create dept")
	} else if !ok {
		return dto.Fail(c, fiber.StatusBadRequest, "上级部门不存在")
	}

	dept := adminmodel.Dept{
		ParentID: req.ParentID,
		Name:     req.Name,
		OrderNo:  req.OrderNo,
		Status:   req.Status,
		Remark:   req.Remark,
	}
	if err := store.DB.Create(&dept).Error; err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to create dept")
	}
	return dto.Success(c, fiber.Map{"id": dept.ID})
}

// UpdateDept 更新部门
// @Summary 更新部门
// @Description 更新部门信息；上级不能设为自身或其下级
// @Tags 系统管理 - 部门
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "部门ID"
// @Param request body admindto.DeptRequest true "部门信息"
// @Success 200 {object} dto.Response
// @Failure 400 {object} dto.Response
// @Failure 404 {object} dto.Response
// @Router /admin/system/dept/{id} [put]
func UpdateDept(c *fiber.Ctx) error {
	id, _ := strconv.ParseUint(c.Params("id"), 10, 64)
	var req admindto.DeptRequest
	if err := validator.BindAndValidate(c, &req); err != nil {
		return err
	}

	var existing adminmodel.Dept
	if err := store.DB.First(&existing, id).Error; err != nil {
		return dto.Fail(c, fiber.StatusNotFound, "Dept not found")
	}
	if cyclic, err := isSelfOrDescendant(store.DB, &adminmodel.Dept{}, existing.ID, req.ParentID); err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to update dept")
	} else if cyclic {
		return dto.Fail(c, fiber.StatusBadRequest, "上级不能是自身或其下级")
	}
	if ok, err := parentExists(store.DB, &adminmodel.Dept{}, req.ParentID); err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to update dept")
	} else if !ok {
		return dto.Fail(c, fiber.StatusBadRequest, "上级部门不存在")
	}

	updates := map[string]any{
		"parent_id": req.ParentID,
		"name":      req.Name,
		"order_no":  req.OrderNo,
		"status":    req.Status,
		"remark":    req.Remark,
	}
	if err := store.DB.Model(&adminmodel.Dept{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to update dept")
	}
	return dto.Success(c, nil)
}

// DeleteDept 删除部门
// @Summary 删除部门
// @Description 删除指定部门及其全部下级
// @Tags 系统管理 - 部门
// @Produce json
// @Security BearerAuth
// @Param id path int true "部门ID"
// @Success 200 {object} dto.Response
// @Failure 404 {object} dto.Response
// @Router /admin/system/dept/{id} [delete]
func DeleteDept(c *fiber.Ctx) error {
	id, _ := strconv.ParseUint(c.Params("id"), 10, 64)
	var dept adminmodel.Dept
	if err := store.DB.First(&dept, id).Error; err != nil {
		return dto.Fail(c, fiber.StatusNotFound, "Dept not found")
	}

	// 后代在事务内收集，避免收集与删除之间新插入的下级漏删
	err := store.DB.Transaction(func(tx *gorm.DB) error {
		descendants, err := collectDescendantIDs(tx, &adminmodel.Dept{}, dept.ID)
		if err != nil {
			return err
		}
		return tx.Where("id IN ?", append(descendants, dept.ID)).Delete(&adminmodel.Dept{}).Error
	})
	if err != nil {
		return dto.Fail(c, fiber.StatusInternalServerError, "Failed to delete dept")
	}
	return dto.Success(c, nil)
}

func buildDeptTree(depts []adminmodel.Dept, parentID uint) []fiber.Map {
	var tree []fiber.Map
	for _, d := range depts {
		if d.ParentID == parentID {
			node := fiber.Map{
				"id":         d.ID,
				"pid":        d.ParentID,
				"name":       d.Name,
				"status":     d.Status,
				"remark":     d.Remark,
				"order":      d.OrderNo,
				"createTime": d.CreatedAt.Format("2006/01/02 15:04:05"),
			}
			children := buildDeptTree(depts, d.ID)
			if len(children) > 0 {
				node["children"] = children
			}
			tree = append(tree, node)
		}
	}
	return tree
}
