package repository

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"

	"cc-053/internal/models"
)

var (
	ErrApplicationNotFound   = errors.New("application not found")
	ErrApplicationNotActive  = errors.New("application is not in an active state")
	ErrSpeakerActiveConflict = errors.New("speaker already holds a slot or queue position")
)

type QuotaRepo struct {
	db *sql.DB
}

func NewQuotaRepo(db *sql.DB) *QuotaRepo {
	return &QuotaRepo{db: db}
}

func (r *QuotaRepo) CreatePlan(p *models.QuotaPlan) error {
	return r.db.QueryRow(
		`INSERT INTO quota_plans (dialect_point_code, required_count, min_age, max_age, gender_req, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at, updated_at`,
		p.DialectPointCode, p.RequiredCount, p.MinAge, p.MaxAge, p.GenderReq, p.CreatedBy,
	).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
}

func (r *QuotaRepo) GetPlanByID(id int64) (*models.QuotaPlan, error) {
	p := &models.QuotaPlan{}
	err := r.db.QueryRow(
		`SELECT id, dialect_point_code, required_count, min_age, max_age, gender_req, created_by, created_at, updated_at
		 FROM quota_plans WHERE id=$1`, id,
	).Scan(&p.ID, &p.DialectPointCode, &p.RequiredCount, &p.MinAge, &p.MaxAge, &p.GenderReq, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (r *QuotaRepo) GetPlanStats(planID int64) (accepted int, waiting int, err error) {
	err = r.db.QueryRow(
		`SELECT COUNT(*) FILTER (WHERE status='accepted'), COUNT(*) FILTER (WHERE status='waiting')
		 FROM speaker_applications WHERE plan_id=$1`, planID,
	).Scan(&accepted, &waiting)
	return
}

func (r *QuotaRepo) ListPlans(offset, limit int) ([]*models.QuotaPlanWithStats, int, error) {
	var total int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM quota_plans`).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := r.db.Query(
		`SELECT p.id, p.dialect_point_code, p.required_count, p.min_age, p.max_age, p.gender_req, p.created_by, p.created_at, p.updated_at,
		        COUNT(a.id) FILTER (WHERE a.status='accepted') AS accepted_count,
		        COUNT(a.id) FILTER (WHERE a.status='waiting') AS waiting_count
		 FROM quota_plans p
		 LEFT JOIN speaker_applications a ON a.plan_id = p.id
		 GROUP BY p.id
		 ORDER BY p.id DESC
		 LIMIT $1 OFFSET $2`, limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var plans []*models.QuotaPlanWithStats
	for rows.Next() {
		p := &models.QuotaPlanWithStats{}
		if err := rows.Scan(&p.ID, &p.DialectPointCode, &p.RequiredCount, &p.MinAge, &p.MaxAge, &p.GenderReq, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.AcceptedCount, &p.WaitingCount); err != nil {
			return nil, 0, err
		}
		plans = append(plans, p)
	}
	return plans, total, rows.Err()
}

// ApplyTx 在单个事务内完成报名：锁定方案行 → 查占用冲突 → 按名额决定 录取/排队/退回，
// 并写入名单变更事件。failures 为条件不符说明（由调用方按方案条件算出），非空则当场退回。
func (r *QuotaRepo) ApplyTx(app *models.SpeakerApplication, plan *models.QuotaPlan, failures []string) error {
	if failures == nil {
		failures = []string{}
	}

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 锁定方案行，串行化同一方案的报名，防止并发超录
	if _, err = tx.Exec(`SELECT id FROM quota_plans WHERE id=$1 FOR UPDATE`, plan.ID); err != nil {
		return err
	}

	// 同一发音人不能同时占两个点的名额（含排队中）
	var conflictPlanID int64
	err = tx.QueryRow(
		`SELECT plan_id FROM speaker_applications WHERE speaker_id=$1 AND status IN ('accepted','waiting') LIMIT 1`,
		app.SpeakerID,
	).Scan(&conflictPlanID)
	switch {
	case err == nil:
		failures = append(failures, fmt.Sprintf("该发音人已在方案 #%d 中占用名额或排队，不能同时占两个调查点的名额", conflictPlanID))
	case err != sql.ErrNoRows:
		return err
	}

	status := "accepted"
	queuePosition := 0
	if len(failures) > 0 {
		status = "rejected"
	} else {
		var acceptedCount int
		if err = tx.QueryRow(
			`SELECT COUNT(*) FROM speaker_applications WHERE plan_id=$1 AND status='accepted'`, plan.ID,
		).Scan(&acceptedCount); err != nil {
			return err
		}
		if acceptedCount >= plan.RequiredCount {
			status = "waiting"
			if err = tx.QueryRow(
				`SELECT COALESCE(MAX(queue_position),0)+1 FROM speaker_applications WHERE plan_id=$1 AND status='waiting'`, plan.ID,
			).Scan(&queuePosition); err != nil {
				return err
			}
		}
	}

	reasonsJSON, err := json.Marshal(failures)
	if err != nil {
		return err
	}

	err = tx.QueryRow(
		`INSERT INTO speaker_applications (plan_id, speaker_id, status, reject_reasons, queue_position, operator)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at, updated_at`,
		plan.ID, app.SpeakerID, status, reasonsJSON, queuePosition, app.Operator,
	).Scan(&app.ID, &app.CreatedAt, &app.UpdatedAt)
	if err != nil {
		// 兜底：并发下同一发音人在不同方案同时报名，由部分唯一索引拦截
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			return ErrSpeakerActiveConflict
		}
		return err
	}

	app.PlanID = plan.ID
	app.Status = status
	app.RejectReasons = failures
	app.QueuePosition = queuePosition

	eventType := map[string]string{"accepted": "accepted", "waiting": "waitlisted", "rejected": "rejected"}[status]
	if err = insertRosterEvent(tx, &models.RosterEvent{
		PlanID:        plan.ID,
		ApplicationID: app.ID,
		SpeakerID:     app.SpeakerID,
		EventType:     eventType,
		FromStatus:    "",
		ToStatus:      status,
		Operator:      app.Operator,
		Reason:        strings.Join(failures, "；"),
	}); err != nil {
		return err
	}

	return tx.Commit()
}

// WithdrawTx 退出报名：状态置为 withdrawn 并记录事件；若退出的是正式名额，
// 按等待队列先后自动顶替并记录顶替事件。返回退出记录与被顶替上来的记录（可能为 nil）。
func (r *QuotaRepo) WithdrawTx(appID int64, operator, reason string) (*models.SpeakerApplication, *models.SpeakerApplication, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	// 先查出方案 ID（不加锁），再按 方案→报名 的统一顺序加锁，与 ApplyTx 保持一致避免死锁
	var planID int64
	err = tx.QueryRow(`SELECT plan_id FROM speaker_applications WHERE id=$1`, appID).Scan(&planID)
	if err == sql.ErrNoRows {
		return nil, nil, ErrApplicationNotFound
	}
	if err != nil {
		return nil, nil, err
	}

	if _, err = tx.Exec(`SELECT id FROM quota_plans WHERE id=$1 FOR UPDATE`, planID); err != nil {
		return nil, nil, err
	}

	app := &models.SpeakerApplication{}
	var reasonsJSON []byte
	err = tx.QueryRow(
		`SELECT id, plan_id, speaker_id, status, reject_reasons, queue_position, operator, created_at, updated_at
		 FROM speaker_applications WHERE id=$1 FOR UPDATE`, appID,
	).Scan(&app.ID, &app.PlanID, &app.SpeakerID, &app.Status, &reasonsJSON, &app.QueuePosition, &app.Operator, &app.CreatedAt, &app.UpdatedAt)
	if err != nil {
		return nil, nil, err
	}
	_ = json.Unmarshal(reasonsJSON, &app.RejectReasons)

	if app.Status != "accepted" && app.Status != "waiting" {
		return nil, nil, ErrApplicationNotActive
	}
	fromStatus := app.Status

	if err = tx.QueryRow(
		`UPDATE speaker_applications SET status='withdrawn', queue_position=0, updated_at=NOW()
		 WHERE id=$1
		 RETURNING updated_at`, appID,
	).Scan(&app.UpdatedAt); err != nil {
		return nil, nil, err
	}
	app.Status = "withdrawn"
	app.QueuePosition = 0

	if err = insertRosterEvent(tx, &models.RosterEvent{
		PlanID:        planID,
		ApplicationID: appID,
		SpeakerID:     app.SpeakerID,
		EventType:     "withdrawn",
		FromStatus:    fromStatus,
		ToStatus:      "withdrawn",
		Operator:      operator,
		Reason:        reason,
	}); err != nil {
		return nil, nil, err
	}

	// 正式名额空出时，按等待队列先后自动顶替
	var promoted *models.SpeakerApplication
	if fromStatus == "accepted" {
		promoted, err = promoteNextInQueue(tx, planID, appID)
		if err != nil {
			return nil, nil, err
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	return app, promoted, nil
}

// promoteNextInQueue 取等待队列中排最前的报名顶替为正式名额；队列为空时返回 nil
func promoteNextInQueue(tx *sql.Tx, planID, withdrawnAppID int64) (*models.SpeakerApplication, error) {
	candidate := &models.SpeakerApplication{PlanID: planID}
	err := tx.QueryRow(
		`SELECT id, speaker_id, operator FROM speaker_applications
		 WHERE plan_id=$1 AND status='waiting'
		 ORDER BY queue_position, id LIMIT 1
		 FOR UPDATE`, planID,
	).Scan(&candidate.ID, &candidate.SpeakerID, &candidate.Operator)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if err = tx.QueryRow(
		`UPDATE speaker_applications SET status='accepted', queue_position=0, updated_at=NOW()
		 WHERE id=$1
		 RETURNING created_at, updated_at`, candidate.ID,
	).Scan(&candidate.CreatedAt, &candidate.UpdatedAt); err != nil {
		return nil, err
	}
	candidate.Status = "accepted"
	candidate.QueuePosition = 0

	if err = insertRosterEvent(tx, &models.RosterEvent{
		PlanID:        planID,
		ApplicationID: candidate.ID,
		SpeakerID:     candidate.SpeakerID,
		EventType:     "promoted",
		FromStatus:    "waiting",
		ToStatus:      "accepted",
		Operator:      "system",
		Reason:        fmt.Sprintf("报名 #%d 退出，按等待队列顺序自动顶替", withdrawnAppID),
	}); err != nil {
		return nil, err
	}
	return candidate, nil
}

func insertRosterEvent(tx *sql.Tx, e *models.RosterEvent) error {
	return tx.QueryRow(
		`INSERT INTO roster_events (plan_id, application_id, speaker_id, event_type, from_status, to_status, operator, reason)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, created_at`,
		e.PlanID, e.ApplicationID, e.SpeakerID, e.EventType, e.FromStatus, e.ToStatus, e.Operator, e.Reason,
	).Scan(&e.ID, &e.CreatedAt)
}

// ListRoster 查某方案下指定状态的名单（含发音人公开信息）；accepted 按报名先后、waiting 按队列位置排序
func (r *QuotaRepo) ListRoster(planID int64, status string) ([]*models.RosterEntry, error) {
	rows, err := r.db.Query(
		`SELECT a.id, a.plan_id, a.speaker_id, a.status, a.reject_reasons, a.queue_position, a.operator, a.created_at, a.updated_at,
		        s.code_name, s.gender, s.birth_year
		 FROM speaker_applications a
		 JOIN speakers s ON s.id = a.speaker_id
		 WHERE a.plan_id=$1 AND a.status=$2
		 ORDER BY a.queue_position, a.id`, planID, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*models.RosterEntry
	for rows.Next() {
		e := &models.RosterEntry{}
		var reasonsJSON []byte
		if err := rows.Scan(&e.ID, &e.PlanID, &e.SpeakerID, &e.Status, &reasonsJSON, &e.QueuePosition, &e.Operator, &e.CreatedAt, &e.UpdatedAt, &e.SpeakerCodeName, &e.SpeakerGender, &e.SpeakerBirthYear); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(reasonsJSON, &e.RejectReasons)
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListEvents 名单变更历史（按发生顺序，可回看名单改过几回）
func (r *QuotaRepo) ListEvents(planID int64, offset, limit int) ([]*models.RosterEvent, int, error) {
	var total int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM roster_events WHERE plan_id=$1`, planID).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := r.db.Query(
		`SELECT id, plan_id, application_id, speaker_id, event_type, from_status, to_status, operator, reason, created_at
		 FROM roster_events WHERE plan_id=$1
		 ORDER BY id LIMIT $2 OFFSET $3`, planID, limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var events []*models.RosterEvent
	for rows.Next() {
		e := &models.RosterEvent{}
		if err := rows.Scan(&e.ID, &e.PlanID, &e.ApplicationID, &e.SpeakerID, &e.EventType, &e.FromStatus, &e.ToStatus, &e.Operator, &e.Reason, &e.CreatedAt); err != nil {
			return nil, 0, err
		}
		events = append(events, e)
	}
	return events, total, rows.Err()
}
