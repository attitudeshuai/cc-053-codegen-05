package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"cc-053/internal/models"
	"cc-053/internal/repository"
	"cc-053/internal/services"
)

type SurveyPointHandler struct {
	pointRepo *repository.SurveyPointRepo
	appRepo   *repository.ApplicationRepo
	svc       *services.RecruitmentService
}

func NewSurveyPointHandler(pointRepo *repository.SurveyPointRepo, appRepo *repository.ApplicationRepo, svc *services.RecruitmentService) *SurveyPointHandler {
	return &SurveyPointHandler{pointRepo: pointRepo, appRepo: appRepo, svc: svc}
}

// respondServiceError 把业务错误映射为对应 HTTP 状态码
func respondServiceError(c *gin.Context, err error) {
	var se *services.ServiceError
	if errors.As(err, &se) {
		switch se.Kind {
		case services.KindNotFound:
			c.JSON(http.StatusNotFound, models.ErrorResponse{Code: 404, Message: "not found", Detail: se.Message})
		case services.KindConflict:
			c.JSON(http.StatusConflict, models.ErrorResponse{Code: 409, Message: "conflict", Detail: se.Message})
		case services.KindValidation:
			c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid request", Detail: se.Message})
		}
		return
	}
	c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "internal error", Detail: err.Error()})
}

func parsePointID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid survey point id"})
		return 0, false
	}
	return id, true
}

// Create 建调查点，先定名额总量与年龄/性别条件
func (h *SurveyPointHandler) Create(c *gin.Context) {
	var req models.CreateSurveyPointRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid request", Detail: err.Error()})
		return
	}

	point, err := h.svc.CreatePoint(req.Code, req.Name, req.Remark, req.Criteria, req.Actor)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, models.APIResponse{Code: 201, Message: "survey point created", Data: point})
}

func (h *SurveyPointHandler) GetByID(c *gin.Context) {
	id, ok := parsePointID(c)
	if !ok {
		return
	}
	point, err := h.pointRepo.GetWithCriteria(id)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Code: 404, Message: "survey point not found"})
		return
	}
	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Data: point})
}

func (h *SurveyPointHandler) List(c *gin.Context) {
	var p models.Pagination
	if err := c.ShouldBindQuery(&p); err != nil {
		p = models.Pagination{}
	}
	p.Normalize()

	points, total, err := h.pointRepo.List(p.Offset, p.Limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to list survey points"})
		return
	}
	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Data: gin.H{"items": points, "total": total, "offset": p.Offset, "limit": p.Limit}})
}

// UpdateQuota 调整名额条件；方案容纳不下现有入选人时整体拒绝
func (h *SurveyPointHandler) UpdateQuota(c *gin.Context) {
	id, ok := parsePointID(c)
	if !ok {
		return
	}
	var req models.UpdateQuotaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid request", Detail: err.Error()})
		return
	}

	result, err := h.svc.UpdateQuota(id, req.Criteria, req.Actor, req.Remark)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Message: "quota updated", Data: result})
}

// Apply 报名：当场过筛，入选 / 排队 / 退回（退回写明差在哪条）
func (h *SurveyPointHandler) Apply(c *gin.Context) {
	id, ok := parsePointID(c)
	if !ok {
		return
	}
	var req models.ApplyPointRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid request", Detail: err.Error()})
		return
	}

	result, err := h.svc.Apply(id, req.SpeakerID, req.Actor)
	if err != nil {
		respondServiceError(c, err)
		return
	}

	status := http.StatusCreated
	msg := "application accepted"
	switch result.Status {
	case "waiting":
		msg = "quota full, added to waitlist"
	case "rejected":
		// 业务上"当场退回"用 200 返回明确原因，而不是把客户端当成出错
		status = http.StatusOK
		msg = "application rejected on site"
	}
	c.JSON(status, models.APIResponse{Code: status, Message: msg, Data: result})
}

// Withdraw 退出；如有排队者同条件顶补，结果中带回顶补信息
func (h *SurveyPointHandler) Withdraw(c *gin.Context) {
	id, ok := parsePointID(c)
	if !ok {
		return
	}
	var req models.WithdrawPointRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid request", Detail: err.Error()})
		return
	}

	result, err := h.svc.Withdraw(id, req.SpeakerID, req.Actor, req.Reason)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Message: "withdrawn", Data: result})
}

// Roster 查看名单（可按 status=enrolled/waiting/withdrawn/rejected 过滤）
func (h *SurveyPointHandler) Roster(c *gin.Context) {
	id, ok := parsePointID(c)
	if !ok {
		return
	}
	var q models.RosterQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		q = models.RosterQuery{}
	}
	q.Normalize()

	switch q.Status {
	case "", "enrolled", "waiting", "withdrawn", "rejected":
	default:
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid status", Detail: q.Status})
		return
	}

	items, total, err := h.appRepo.ListRoster(id, q.Status, q.Offset, q.Limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to list roster"})
		return
	}
	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Data: gin.H{"items": items, "total": total, "offset": q.Offset, "limit": q.Limit}})
}

// Events 名单变更流水：回看名单改过几回、是谁在什么时候操作的
func (h *SurveyPointHandler) Events(c *gin.Context) {
	id, ok := parsePointID(c)
	if !ok {
		return
	}
	var q models.EventQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		q = models.EventQuery{}
	}
	q.Normalize()

	items, total, err := h.appRepo.ListEvents(id, q.EventType, q.Offset, q.Limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to list roster events"})
		return
	}
	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Data: gin.H{"items": items, "total": total, "offset": q.Offset, "limit": q.Limit}})
}
