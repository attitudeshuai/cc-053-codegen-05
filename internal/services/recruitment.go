package services

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"cc-053/internal/models"
	"cc-053/internal/repository"
)

// 业务错误类型，handler 据此映射 HTTP 状态码
type ErrorKind string

const (
	KindNotFound   ErrorKind = "not_found"
	KindConflict   ErrorKind = "conflict"
	KindValidation ErrorKind = "validation"
)

type ServiceError struct {
	Kind    ErrorKind
	Message string
}

func (e *ServiceError) Error() string { return e.Message }

func errNotFound(msg string) *ServiceError { return &ServiceError{Kind: KindNotFound, Message: msg} }
func errConflict(msg string) *ServiceError { return &ServiceError{Kind: KindConflict, Message: msg} }
func errValidation(msg string) *ServiceError {
	return &ServiceError{Kind: KindValidation, Message: msg}
}

// ApplyResult 报名处理结果：enrolled 入选 / waiting 排队 / rejected 当场退回
type ApplyResult struct {
	Status        string                 `json:"status"`
	ApplicationID int64                  `json:"application_id"`
	RejectReasons []string               `json:"reject_reasons,omitempty"`
	MatchedRule   *models.QuotaCriterion `json:"matched_rule,omitempty"`
	QueueSeq      *int64                 `json:"queue_seq,omitempty"`
	Promoted      []PromotionInfo        `json:"promoted,omitempty"`
}

// PromotionInfo 一次操作触发的顶补信息
type PromotionInfo struct {
	ApplicationID int64                 `json:"application_id"`
	SpeakerID     int64                 `json:"speaker_id"`
	SpeakerCode   string                `json:"speaker_code_name"`
	QueueSeq      int64                 `json:"queue_seq"`
	Criterion     models.QuotaCriterion `json:"criterion"`
}

// WithdrawResult 退出结果（若有排队者顶补则带回顶补信息）
type WithdrawResult struct {
	WithdrawnApplicationID int64          `json:"withdrawn_application_id"`
	SpeakerID              int64          `json:"speaker_id"`
	PreviousStatus         string         `json:"previous_status"`
	Promoted               *PromotionInfo `json:"promoted,omitempty"`
}

// QuotaUpdateResult 名额调整结果（容量变大时可能顺带顶补排队者）
type QuotaUpdateResult struct {
	Point    *models.SurveyPoint `json:"point"`
	Promoted []PromotionInfo     `json:"promoted,omitempty"`
}

type RecruitmentService struct {
	db       *sql.DB
	points   *repository.SurveyPointRepo
	apps     *repository.ApplicationRepo
	speakers *repository.SpeakerRepo
}

func NewRecruitmentService(db *sql.DB, points *repository.SurveyPointRepo, apps *repository.ApplicationRepo, speakers *repository.SpeakerRepo) *RecruitmentService {
	return &RecruitmentService{db: db, points: points, apps: apps, speakers: speakers}
}

// CreatePoint 建调查点并定名额条件，记录初始方案
func (s *RecruitmentService) CreatePoint(code, name, remark string, in []models.CriterionInput, actor string) (*models.SurveyPoint, error) {
	for _, c := range in {
		if c.MinBirthYear > c.MaxBirthYear {
			return nil, errValidation(fmt.Sprintf("名额条件出生年份区间非法：%d > %d", c.MinBirthYear, c.MaxBirthYear))
		}
	}

	criteria := make([]models.QuotaCriterion, 0, len(in))
	for _, c := range in {
		criteria = append(criteria, models.QuotaCriterion{
			Gender: c.Gender, MinBirthYear: c.MinBirthYear, MaxBirthYear: c.MaxBirthYear, Seats: c.Seats,
		})
	}

	point := &models.SurveyPoint{Code: code, Name: name, Remark: remark}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := s.points.CreateWithCriteriaTx(tx, point, criteria); err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return nil, errConflict(fmt.Sprintf("调查点编号 %q 已存在", code))
		}
		return nil, err
	}
	if err := s.insertEvent(tx, point.ID, nil, nil, "criteria_changed", actor, map[string]interface{}{
		"old_criteria": []interface{}{},
		"new_criteria": criteriaSnapshot(point.Criteria),
		"quota_total":  point.QuotaTotal,
		"action":       "create_point",
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return point, nil
}

// Apply 报名：过筛 → 入选或入队；不符合任意一条当场退回并写明差在哪条
func (s *RecruitmentService) Apply(pointID, speakerID int64, actor string) (*ApplyResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 锁定调查点：同一点的报名/退出/顶替串行化
	if _, err := s.points.GetTx(tx, pointID, true); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound(fmt.Sprintf("调查点 %d 不存在", pointID))
		}
		return nil, err
	}

	speaker, err := s.speakers.GetByIDTx(tx, speakerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound(fmt.Sprintf("发音人 %d 不存在", speakerID))
	}
	if err != nil {
		return nil, err
	}

	// 同一点重复报名
	if exist, err := s.apps.GetActiveByPointSpeakerTx(tx, pointID, speakerID); err != nil {
		return nil, err
	} else if exist != nil {
		state := map[string]string{"enrolled": "已入选", "waiting": "已在等待队列"}[exist.Status]
		return nil, errConflict(fmt.Sprintf("该发音人%s于本调查点，不能重复报名", state))
	}

	// 同一个人不能同时占两个点的名额（排队也视为占位）
	if occupied, err := s.apps.GetActiveBySpeakerTx(tx, speakerID); err != nil {
		return nil, err
	} else if occupied != nil && occupied.PointID != pointID {
		other, _ := s.points.GetTx(tx, occupied.PointID, false)
		where := fmt.Sprintf("调查点 %d", occupied.PointID)
		if other != nil {
			where = fmt.Sprintf("调查点「%s」(%s)", other.Name, other.Code)
		}
		state := map[string]string{"enrolled": "名额", "waiting": "排队位置"}[occupied.Status]
		return nil, errConflict(fmt.Sprintf("该发音人已在%s占用%s，不能同时报名本调查点", where, state))
	}

	criteria, err := s.points.ListCriteriaTx(tx, pointID)
	if err != nil {
		return nil, err
	}

	// 过筛：逐条比对性别与出生年份
	matched, reasons := evaluate(speaker, criteria)

	result := &ApplyResult{}

	if len(matched) == 0 {
		// 当场退回：留存退回记录与原因
		app := &models.PointApplication{
			PointID: pointID, SpeakerID: speakerID,
			Status: "rejected", RejectReason: joinReasons(reasons),
		}
		if err := s.apps.CreateTx(tx, app); err != nil {
			return nil, mapUniqueViolation(err)
		}
		if err := s.insertEvent(tx, pointID, &app.ID, &speakerID, "applied", actor, map[string]interface{}{
			"speaker_code_name": speaker.CodeName,
		}); err != nil {
			return nil, err
		}
		if err := s.insertEvent(tx, pointID, &app.ID, &speakerID, "rejected", actor, map[string]interface{}{
			"reject_reasons": reasons,
		}); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		result.Status = "rejected"
		result.ApplicationID = app.ID
		result.RejectReasons = reasons
		return result, nil
	}

	// 符合条件：在匹配的名额类别里找一个还有空位的
	var freeCriterion *models.QuotaCriterion
	for i := range matched {
		c := &matched[i]
		n, err := s.apps.CountEnrolledByCriterionTx(tx, c.ID)
		if err != nil {
			return nil, err
		}
		if n < c.Seats {
			freeCriterion = c
			break
		}
	}

	if freeCriterion == nil {
		// 名额已满 → 进等待队列
		seq, err := s.apps.NextQueueSeqTx(tx, pointID)
		if err != nil {
			return nil, err
		}
		app := &models.PointApplication{
			PointID: pointID, SpeakerID: speakerID,
			Status: "waiting", QueueSeq: &seq,
		}
		if err := s.apps.CreateTx(tx, app); err != nil {
			return nil, mapUniqueViolation(err)
		}
		if err := s.insertEvent(tx, pointID, &app.ID, &speakerID, "applied", actor, map[string]interface{}{
			"speaker_code_name": speaker.CodeName,
		}); err != nil {
			return nil, err
		}
		if err := s.insertEvent(tx, pointID, &app.ID, &speakerID, "entered_waitlist", actor, map[string]interface{}{
			"queue_seq": seq,
			"reason":    "名额已满",
		}); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		result.Status = "waiting"
		result.ApplicationID = app.ID
		result.QueueSeq = &seq
		return result, nil
	}

	// 入选，占用该名额类别
	cid := freeCriterion.ID
	app := &models.PointApplication{
		PointID: pointID, SpeakerID: speakerID, CriterionID: &cid,
		Status: "enrolled",
	}
	if err := s.apps.CreateTx(tx, app); err != nil {
		return nil, mapUniqueViolation(err)
	}
	if err := s.insertEvent(tx, pointID, &app.ID, &speakerID, "applied", actor, map[string]interface{}{
		"speaker_code_name": speaker.CodeName,
	}); err != nil {
		return nil, err
	}
	if err := s.insertEvent(tx, pointID, &app.ID, &speakerID, "enrolled", actor, map[string]interface{}{
		"criterion_id": freeCriterion.ID,
		"gender":       freeCriterion.Gender,
		"birth_range":  []int{freeCriterion.MinBirthYear, freeCriterion.MaxBirthYear},
		"seats":        freeCriterion.Seats,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	rc := *freeCriterion
	result.Status = "enrolled"
	result.ApplicationID = app.ID
	result.MatchedRule = &rc
	return result, nil
}

// Withdraw 退出；若退出者原本占名额，按排队先后把同条件的队首顶上来
func (s *RecruitmentService) Withdraw(pointID, speakerID int64, actor, reason string) (*WithdrawResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := s.points.GetTx(tx, pointID, true); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound(fmt.Sprintf("调查点 %d 不存在", pointID))
		}
		return nil, err
	}

	app, err := s.apps.GetActiveByPointSpeakerTx(tx, pointID, speakerID)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, errNotFound("该发音人在本调查点没有入选或排队记录，无需退出")
	}

	previousStatus := app.Status
	freedCriterionID := app.CriterionID
	freedSeq := app.QueueSeq

	if err := s.apps.WithdrawTx(tx, app.ID); err != nil {
		return nil, err
	}
	if err := s.insertEvent(tx, pointID, &app.ID, &speakerID, "withdrawn", actor, map[string]interface{}{
		"previous_status": previousStatus,
		"queue_seq":       freedSeq,
		"reason":          reason,
	}); err != nil {
		return nil, err
	}

	result := &WithdrawResult{
		WithdrawnApplicationID: app.ID,
		SpeakerID:              speakerID,
		PreviousStatus:         previousStatus,
	}

	// 只有入选者退出才空出名额；排队者退出不触发顶替
	if previousStatus == "enrolled" {
		criteria, err := s.points.ListCriteriaTx(tx, pointID)
		if err != nil {
			return nil, err
		}
		seat := s.findFreeCriterion(tx, criteria, freedCriterionID)
		if seat != nil {
			promoted, err := s.promoteHead(tx, pointID, *seat, actor)
			if err != nil {
				return nil, err
			}
			if promoted != nil {
				result.Promoted = promoted
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateQuota 修改名额方案：重算名额并把现有入选人重新落位；容纳不下则整体拒绝；
// 新方案有空位时按排队先后顶补。
func (s *RecruitmentService) UpdateQuota(pointID int64, in []models.CriterionInput, actor string, remark *string) (*QuotaUpdateResult, error) {
	for _, c := range in {
		if c.MinBirthYear > c.MaxBirthYear {
			return nil, errValidation(fmt.Sprintf("名额条件出生年份区间非法：%d > %d", c.MinBirthYear, c.MaxBirthYear))
		}
		if c.Seats <= 0 {
			return nil, errValidation("名额数必须大于 0")
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	point, err := s.points.GetTx(tx, pointID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound(fmt.Sprintf("调查点 %d 不存在", pointID))
	}
	if err != nil {
		return nil, err
	}

	oldCriteria, err := s.points.ListCriteriaTx(tx, pointID)
	if err != nil {
		return nil, err
	}
	oldQuota := point.QuotaTotal

	newCriteria := make([]models.QuotaCriterion, 0, len(in))
	for _, c := range in {
		newCriteria = append(newCriteria, models.QuotaCriterion{
			Gender: c.Gender, MinBirthYear: c.MinBirthYear, MaxBirthYear: c.MaxBirthYear, Seats: c.Seats,
		})
	}

	// 替换名额条件（旧条件删除会把报名记录的 criterion_id 置空，下面重新落位）
	if err := s.points.ReplaceCriteriaTx(tx, pointID, newCriteria); err != nil {
		return nil, err
	}

	// 现有入选人按入选先后重新匹配名额类别，匹配/容量不够则整体回滚
	enrolled, err := s.apps.ListEnrolledTx(tx, pointID)
	if err != nil {
		return nil, err
	}
	used := map[int64]int{}
	for _, a := range enrolled {
		c := pickCriterion(a.BirthYear, a.Gender, newCriteria, used)
		if c == nil {
			sp := a.SpeakerCodeName
			return nil, errConflict(fmt.Sprintf(
				"新名额方案无法容纳现有入选人（发音人 %s/%d，性别 %s，出生年份 %d 已无匹配名额），修改已取消",
				sp, a.SpeakerID, a.Gender, a.BirthYear))
		}
		used[c.ID]++
		if err := s.apps.RemapCriterionTx(tx, a.ID, c.ID); err != nil {
			return nil, err
		}
	}

	if remark != nil && *remark != point.Remark {
		if err := s.points.UpdateRemarkTx(tx, pointID, *remark); err != nil {
			return nil, err
		}
		point.Remark = *remark
	}

	// 名额变多产生空位时，按排队先后顶补
	promoted, err := s.fillFromQueue(tx, pointID, newCriteria, used, actor)
	if err != nil {
		return nil, err
	}

	// 留痕：记录改前改后快照
	oldSnapshot := criteriaSnapshot(oldCriteria)
	newSnapshot := criteriaSnapshot(newCriteria)
	newQuota := 0
	for _, c := range in {
		newQuota += c.Seats
	}
	if err := s.insertEvent(tx, pointID, nil, nil, "criteria_changed", actor, map[string]interface{}{
		"old_criteria": oldSnapshot,
		"new_criteria": newSnapshot,
	}); err != nil {
		return nil, err
	}
	if oldQuota != newQuota {
		if err := s.insertEvent(tx, pointID, nil, nil, "quota_changed", actor, map[string]interface{}{
			"old_quota_total": oldQuota,
			"new_quota_total": newQuota,
		}); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	point.Criteria = newCriteria
	point.QuotaTotal = newQuota
	return &QuotaUpdateResult{Point: point, Promoted: promoted}, nil
}

// ===== 内部辅助 =====

// evaluate 逐条过筛，返回匹配的名额类别与不符合项说明（差在哪一条）
func evaluate(speaker *models.Speaker, criteria []models.QuotaCriterion) (matched []models.QuotaCriterion, reasons []string) {
	var sameGender []models.QuotaCriterion
	for _, c := range criteria {
		if c.Gender == speaker.Gender {
			sameGender = append(sameGender, c)
		}
	}

	if len(sameGender) == 0 {
		allowed := gendersOf(criteria)
		return nil, []string{fmt.Sprintf(
			"性别不符：本调查点只招收 %s 发音人，报名者性别为 %s", joinZh(allowed, "、"), genderZh(speaker.Gender))}
	}

	for _, c := range sameGender {
		if speaker.BirthYear >= c.MinBirthYear && speaker.BirthYear <= c.MaxBirthYear {
			matched = append(matched, c)
		}
	}
	if len(matched) == 0 {
		var ranges []string
		for _, c := range sameGender {
			ranges = append(ranges, fmt.Sprintf("%d–%d", c.MinBirthYear, c.MaxBirthYear))
		}
		return nil, []string{fmt.Sprintf(
			"出生年份不符：报名者出生于 %d 年，本调查点 %s 发音人要求出生年份在 %s",
			speaker.BirthYear, genderZh(speaker.Gender), joinZh(ranges, " 或 "))}
	}
	return matched, nil
}

// findFreeCriterion 退出后确定空出的名额类别：优先被释放的类别，否则任有空位的类别
func (s *RecruitmentService) findFreeCriterion(tx *sql.Tx, criteria []models.QuotaCriterion, freedID *int64) *models.QuotaCriterion {
	used := map[int64]int{}
	for i := range criteria {
		c := &criteria[i]
		n, err := s.apps.CountEnrolledByCriterionTx(tx, c.ID)
		if err != nil {
			return nil
		}
		used[c.ID] = n
	}
	if freedID != nil {
		for i := range criteria {
			c := &criteria[i]
			if c.ID == *freedID && used[c.ID] < c.Seats {
				return c
			}
		}
	}
	for i := range criteria {
		c := &criteria[i]
		if used[c.ID] < c.Seats {
			return c
		}
	}
	return nil
}

// promoteHead 把符合给定名额类别、排队最靠前的人顶上来
func (s *RecruitmentService) promoteHead(tx *sql.Tx, pointID int64, seat models.QuotaCriterion, actor string) (*PromotionInfo, error) {
	waiters, err := s.apps.ListWaitingTx(tx, pointID)
	if err != nil {
		return nil, err
	}
	for _, w := range waiters {
		if w.Gender == seat.Gender && w.BirthYear >= seat.MinBirthYear && w.BirthYear <= seat.MaxBirthYear {
			if err := s.apps.PromoteTx(tx, w.ID, seat.ID); err != nil {
				return nil, err
			}
			speakerID := w.SpeakerID
			seq := int64(0)
			if w.QueueSeq != nil {
				seq = *w.QueueSeq
			}
			if err := s.insertEvent(tx, pointID, &w.ID, &speakerID, "promoted", actor, map[string]interface{}{
				"queue_seq":         seq,
				"criterion_id":      seat.ID,
				"speaker_code_name": w.SpeakerCodeName,
				"trigger":           "withdrawn",
			}); err != nil {
				return nil, err
			}
			return &PromotionInfo{
				ApplicationID: w.ID,
				SpeakerID:     w.SpeakerID,
				SpeakerCode:   w.SpeakerCodeName,
				QueueSeq:      seq,
				Criterion:     seat,
			}, nil
		}
	}
	return nil, nil
}

// fillFromQueue 名额扩容后：只要还有符合条件的排队者和空位，就持续顶补
func (s *RecruitmentService) fillFromQueue(tx *sql.Tx, pointID int64, criteria []models.QuotaCriterion, used map[int64]int, actor string) ([]PromotionInfo, error) {
	waiters, err := s.apps.ListWaitingTx(tx, pointID)
	if err != nil {
		return nil, err
	}
	var promoted []PromotionInfo
	for _, w := range waiters {
		c := pickCriterion(w.BirthYear, w.Gender, criteria, used)
		if c == nil {
			continue
		}
		if err := s.apps.PromoteTx(tx, w.ID, c.ID); err != nil {
			return nil, err
		}
		used[c.ID]++
		speakerID := w.SpeakerID
		seq := int64(0)
		if w.QueueSeq != nil {
			seq = *w.QueueSeq
		}
		if err := s.insertEvent(tx, pointID, &w.ID, &speakerID, "promoted", actor, map[string]interface{}{
			"queue_seq":         seq,
			"criterion_id":      c.ID,
			"speaker_code_name": w.SpeakerCodeName,
			"trigger":           "quota_changed",
		}); err != nil {
			return nil, err
		}
		promoted = append(promoted, PromotionInfo{
			ApplicationID: w.ID,
			SpeakerID:     w.SpeakerID,
			SpeakerCode:   w.SpeakerCodeName,
			QueueSeq:      seq,
			Criterion:     *c,
		})
	}
	return promoted, nil
}

// pickCriterion 在仍有空位的类别中挑第一个与发音人属性匹配的（按 criteria 顺序，确定性）
func pickCriterion(birthYear int, gender string, criteria []models.QuotaCriterion, used map[int64]int) *models.QuotaCriterion {
	for i := range criteria {
		c := &criteria[i]
		if c.Gender != gender {
			continue
		}
		if birthYear < c.MinBirthYear || birthYear > c.MaxBirthYear {
			continue
		}
		if used[c.ID] < c.Seats {
			return c
		}
	}
	return nil
}

func (s *RecruitmentService) insertEvent(tx *sql.Tx, pointID int64, appID, speakerID *int64, eventType, actor string, detail map[string]interface{}) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	e := &models.RosterEvent{
		PointID: pointID, ApplicationID: appID, SpeakerID: speakerID,
		EventType: eventType, Detail: raw, Actor: actor,
	}
	return s.apps.InsertEventTx(tx, e)
}

func mapUniqueViolation(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return errConflict("该发音人已在其他调查点占用名额或排队，不能重复占位")
	}
	return err
}

func joinReasons(reasons []string) string {
	out := ""
	for i, r := range reasons {
		if i > 0 {
			out += "；"
		}
		out += r
	}
	return out
}

func gendersOf(cs []models.QuotaCriterion) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cs {
		if !seen[c.Gender] {
			seen[c.Gender] = true
			out = append(out, genderZh(c.Gender))
		}
	}
	return out
}

func genderZh(g string) string {
	switch g {
	case "male":
		return "男性"
	case "female":
		return "女性"
	default:
		return "其他性别"
	}
}

func joinZh(items []string, sep string) string {
	out := ""
	for i, it := range items {
		if i > 0 {
			out += sep
		}
		out += it
	}
	return out
}

func criteriaSnapshot(cs []models.QuotaCriterion) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]interface{}{
			"id":             c.ID,
			"gender":         c.Gender,
			"min_birth_year": c.MinBirthYear,
			"max_birth_year": c.MaxBirthYear,
			"seats":          c.Seats,
		})
	}
	return out
}
