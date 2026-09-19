package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"cc-053/internal/models"
	"cc-053/internal/repository"
)

type QuotaHandler struct {
	quotaRepo   *repository.QuotaRepo
	speakerRepo *repository.SpeakerRepo
}

func NewQuotaHandler(quotaRepo *repository.QuotaRepo, speakerRepo *repository.SpeakerRepo) *QuotaHandler {
	return &QuotaHandler{quotaRepo: quotaRepo, speakerRepo: speakerRepo}
}

// CreatePlan 创建调查点遴选方案：要几个人、年龄区间、性别要求
func (h *QuotaHandler) CreatePlan(c *gin.Context) {
	var req models.CreateQuotaPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid request", Detail: err.Error()})
		return
	}
	if req.GenderReq == "" {
		req.GenderReq = "any"
	}
	if req.MaxAge == 0 {
		req.MaxAge = 150
	}
	if req.MinAge > req.MaxAge {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "min_age must not exceed max_age"})
		return
	}

	plan := &models.QuotaPlan{
		DialectPointCode: req.DialectPointCode,
		RequiredCount:    req.RequiredCount,
		MinAge:           req.MinAge,
		MaxAge:           req.MaxAge,
		GenderReq:        req.GenderReq,
		CreatedBy:        req.CreatedBy,
	}
	if err := h.quotaRepo.CreatePlan(plan); err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to create quota plan", Detail: err.Error()})
		return
	}

	c.JSON(http.StatusCreated, models.APIResponse{Code: 201, Message: "quota plan created", Data: plan})
}

func (h *QuotaHandler) ListPlans(c *gin.Context) {
	var p models.Pagination
	if err := c.ShouldBindQuery(&p); err != nil {
		p = models.Pagination{}
	}
	p.Normalize()

	plans, total, err := h.quotaRepo.ListPlans(p.Offset, p.Limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to list quota plans"})
		return
	}

	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Data: gin.H{"items": plans, "total": total, "offset": p.Offset, "limit": p.Limit}})
}

// GetPlan 方案详情（含已录取/排队统计）
func (h *QuotaHandler) GetPlan(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid id"})
		return
	}

	plan, err := h.quotaRepo.GetPlanByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Code: 404, Message: "quota plan not found"})
		return
	}

	accepted, waiting, err := h.quotaRepo.GetPlanStats(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to load plan stats"})
		return
	}

	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Data: models.QuotaPlanWithStats{
		QuotaPlan:     *plan,
		AcceptedCount: accepted,
		WaitingCount:  waiting,
	}})
}

// Apply 报名：按方案条件过筛，不符当场退回并逐条说明；名额满了进等待队列
func (h *QuotaHandler) Apply(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid plan id"})
		return
	}

	var req models.ApplySpeakerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid request", Detail: err.Error()})
		return
	}

	plan, err := h.quotaRepo.GetPlanByID(planID)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Code: 404, Message: "quota plan not found"})
		return
	}

	speaker, err := h.speakerRepo.GetByID(req.SpeakerID)
	if err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Code: 404, Message: "speaker not found"})
		return
	}

	// 按方案条件过筛：年龄、性别逐条核对
	failures := plan.EligibilityFailures(speaker, time.Now().Year())

	app := &models.SpeakerApplication{
		PlanID:    planID,
		SpeakerID: req.SpeakerID,
		Operator:  req.Operator,
	}
	if err := h.quotaRepo.ApplyTx(app, plan, failures); err != nil {
		if errors.Is(err, repository.ErrSpeakerActiveConflict) {
			c.JSON(http.StatusConflict, models.ErrorResponse{Code: 409, Message: "speaker already holds a slot or queue position in another plan"})
			return
		}
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to submit application", Detail: err.Error()})
		return
	}

	switch app.Status {
	case "rejected":
		// 当场退回，逐条说明差在哪一条
		c.JSON(http.StatusUnprocessableEntity, models.APIResponse{
			Code:    422,
			Message: "application rejected: eligibility criteria not met",
			Data:    gin.H{"application": app, "failed_criteria": app.RejectReasons},
		})
	case "waiting":
		c.JSON(http.StatusCreated, models.APIResponse{
			Code:    201,
			Message: "quota full, added to waiting queue",
			Data:    app,
		})
	default:
		c.JSON(http.StatusCreated, models.APIResponse{Code: 201, Message: "application accepted", Data: app})
	}
}

// Withdraw 退出报名：释放名额后按等待队列先后自动顶替，退出与顶替均留痕
func (h *QuotaHandler) Withdraw(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid application id"})
		return
	}

	var req models.WithdrawApplicationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid request", Detail: err.Error()})
		return
	}

	withdrawn, promoted, err := h.quotaRepo.WithdrawTx(id, req.Operator, req.Reason)
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrApplicationNotFound):
			c.JSON(http.StatusNotFound, models.ErrorResponse{Code: 404, Message: "application not found"})
		case errors.Is(err, repository.ErrApplicationNotActive):
			c.JSON(http.StatusConflict, models.ErrorResponse{Code: 409, Message: "application is not active (already withdrawn or rejected)"})
		default:
			c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to withdraw application", Detail: err.Error()})
		}
		return
	}

	data := gin.H{"withdrawn": withdrawn}
	message := "application withdrawn"
	if promoted != nil {
		data["promoted"] = promoted
		message = "application withdrawn; next speaker in queue promoted"
	}
	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Message: message, Data: data})
}

// GetRoster 当前名单：正式录取 + 等待队列（按排队先后）
func (h *QuotaHandler) GetRoster(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid plan id"})
		return
	}

	if _, err := h.quotaRepo.GetPlanByID(planID); err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Code: 404, Message: "quota plan not found"})
		return
	}

	accepted, err := h.quotaRepo.ListRoster(planID, "accepted")
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to list roster"})
		return
	}
	waiting, err := h.quotaRepo.ListRoster(planID, "waiting")
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to list waiting queue"})
		return
	}

	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Data: gin.H{
		"plan_id":  planID,
		"accepted": accepted,
		"waiting":  waiting,
	}})
}

// ListEvents 名单变更历史：每一次录取/退回/排队/退出/顶替都可回看
func (h *QuotaHandler) ListEvents(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, models.ErrorResponse{Code: 400, Message: "invalid plan id"})
		return
	}

	var p models.Pagination
	if err := c.ShouldBindQuery(&p); err != nil {
		p = models.Pagination{}
	}
	p.Normalize()

	if _, err := h.quotaRepo.GetPlanByID(planID); err != nil {
		c.JSON(http.StatusNotFound, models.ErrorResponse{Code: 404, Message: "quota plan not found"})
		return
	}

	events, total, err := h.quotaRepo.ListEvents(planID, p.Offset, p.Limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, models.ErrorResponse{Code: 500, Message: "failed to list roster events"})
		return
	}

	c.JSON(http.StatusOK, models.APIResponse{Code: 200, Data: gin.H{"items": events, "total": total, "offset": p.Offset, "limit": p.Limit}})
}
