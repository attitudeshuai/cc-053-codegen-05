package repository

import (
	"database/sql"
	"strconv"
	"strings"
	"time"

	"cc-053/internal/models"
)

// ApplicationRepo 管理报名占位与名单事件。写操作均以 *sql.Tx 形式暴露，
// 由 RecruitmentService 统一开启事务，保证"过筛 / 入选 / 入队 / 退出 / 顶替 / 留痕"原子完成。
type ApplicationRepo struct {
	db *sql.DB
}

func NewApplicationRepo(db *sql.DB) *ApplicationRepo {
	return &ApplicationRepo{db: db}
}

const applicationColumns = `a.id, a.point_id, a.speaker_id, a.criterion_id, a.status, a.reject_reason,
	a.queue_seq, a.enrolled_at, a.withdrawn_at, a.rejected_at, a.created_at, a.updated_at,
	s.code_name, s.birth_year, s.gender`

const applicationJoin = ` FROM point_applications a JOIN speakers s ON s.id = a.speaker_id`

func scanApplication(row interface {
	Scan(dest ...interface{}) error
}, a *models.PointApplication) error {
	return row.Scan(
		&a.ID, &a.PointID, &a.SpeakerID, &a.CriterionID, &a.Status, &a.RejectReason,
		&a.QueueSeq, &a.EnrolledAt, &a.WithdrawnAt, &a.RejectedAt, &a.CreatedAt, &a.UpdatedAt,
		&a.SpeakerCodeName, &a.BirthYear, &a.Gender,
	)
}

// CreateTx 新建一条报名记录（enrolled / waiting / rejected 由 service 决定）
func (r *ApplicationRepo) CreateTx(tx *sql.Tx, a *models.PointApplication) error {
	var enrolledAt, rejectedAt interface{}
	if a.Status == "enrolled" {
		enrolledAt = time.Now()
	} else if a.Status == "rejected" {
		rejectedAt = time.Now()
	}
	return tx.QueryRow(
		`INSERT INTO point_applications (point_id, speaker_id, criterion_id, status, reject_reason, queue_seq, enrolled_at, rejected_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, created_at, updated_at`,
		a.PointID, a.SpeakerID, a.CriterionID, a.Status, a.RejectReason, a.QueueSeq, enrolledAt, rejectedAt,
	).Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
}

// GetActiveBySpeakerTx 查该发音人当前在任意点的活跃占位（enrolled/waiting）
func (r *ApplicationRepo) GetActiveBySpeakerTx(tx *sql.Tx, speakerID int64) (*models.PointApplication, error) {
	a := &models.PointApplication{}
	err := scanApplication(tx.QueryRow(
		`SELECT `+applicationColumns+applicationJoin+`
		 WHERE a.speaker_id=$1 AND a.status IN ('enrolled','waiting')
		 ORDER BY a.id DESC LIMIT 1`, speakerID,
	), a)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// GetActiveByPointSpeakerTx 查同一点同一发音人的活跃占位
func (r *ApplicationRepo) GetActiveByPointSpeakerTx(tx *sql.Tx, pointID, speakerID int64) (*models.PointApplication, error) {
	a := &models.PointApplication{}
	err := scanApplication(tx.QueryRow(
		`SELECT `+applicationColumns+applicationJoin+`
		 WHERE a.point_id=$1 AND a.speaker_id=$2 AND a.status IN ('enrolled','waiting')`, pointID, speakerID,
	), a)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// CountEnrolledByCriterionTx 某条名额条件下已入选人数
func (r *ApplicationRepo) CountEnrolledByCriterionTx(tx *sql.Tx, criterionID int64) (int, error) {
	var n int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM point_applications WHERE criterion_id=$1 AND status='enrolled'`,
		criterionID,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// NextQueueSeqTx 该点等待队列的下一个序号（单调递增，退出/顶替后也不复用）
func (r *ApplicationRepo) NextQueueSeqTx(tx *sql.Tx, pointID int64) (int64, error) {
	var seq int64
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(queue_seq), 0) + 1 FROM point_applications WHERE point_id=$1`,
		pointID,
	).Scan(&seq); err != nil {
		return 0, err
	}
	return seq, nil
}

// ListWaitingTx 该点全部等待者，按排队先后返回（联带发音人属性用于条件匹配）
func (r *ApplicationRepo) ListWaitingTx(tx *sql.Tx, pointID int64) ([]*models.PointApplication, error) {
	rows, err := tx.Query(
		`SELECT `+applicationColumns+applicationJoin+`
		 WHERE a.point_id=$1 AND a.status='waiting'
		 ORDER BY a.queue_seq`, pointID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []*models.PointApplication
	for rows.Next() {
		a := &models.PointApplication{}
		if err := scanApplication(rows, a); err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, nil
}

// ListEnrolledTx 该点全部入选人（联带发音人属性，用于改名额时的校验与重映射）
func (r *ApplicationRepo) ListEnrolledTx(tx *sql.Tx, pointID int64) ([]*models.PointApplication, error) {
	rows, err := tx.Query(
		`SELECT `+applicationColumns+applicationJoin+`
		 WHERE a.point_id=$1 AND a.status='enrolled'
		 ORDER BY a.enrolled_at, a.id`, pointID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []*models.PointApplication
	for rows.Next() {
		a := &models.PointApplication{}
		if err := scanApplication(rows, a); err != nil {
			return nil, err
		}
		apps = append(apps, a)
	}
	return apps, nil
}

// PromoteTx 等待者顶补入选：原地转 enrolled，记录入选时间与所占名额类别
func (r *ApplicationRepo) PromoteTx(tx *sql.Tx, appID, criterionID int64) error {
	_, err := tx.Exec(
		`UPDATE point_applications
		 SET status='enrolled', criterion_id=$2, queue_seq=NULL, enrolled_at=NOW(), updated_at=NOW()
		 WHERE id=$1 AND status='waiting'`,
		appID, criterionID,
	)
	return err
}

// WithdrawTx 退出：enrolled / waiting 均可退出
func (r *ApplicationRepo) WithdrawTx(tx *sql.Tx, appID int64) error {
	_, err := tx.Exec(
		`UPDATE point_applications SET status='withdrawn', withdrawn_at=NOW(), queue_seq=NULL, updated_at=NOW()
		 WHERE id=$1 AND status IN ('enrolled','waiting')`,
		appID,
	)
	return err
}

// RemapCriterionTx 名额条件替换后，按发音人属性把入选人重新挂到新条件上
func (r *ApplicationRepo) RemapCriterionTx(tx *sql.Tx, appID, criterionID int64) error {
	_, err := tx.Exec(
		`UPDATE point_applications SET criterion_id=$2, updated_at=NOW() WHERE id=$1`,
		appID, criterionID,
	)
	return err
}

// InsertEvent 写入一条名单事件流水（非事务版，建点等无现成事务的场景使用）
func (r *ApplicationRepo) InsertEvent(e *models.RosterEvent) error {
	return r.db.QueryRow(
		`INSERT INTO roster_events (point_id, application_id, speaker_id, event_type, detail, actor)
		 VALUES ($1, $2, $3, $4, COALESCE($5, '{}'::jsonb), $6)
		 RETURNING id, created_at`,
		e.PointID, e.ApplicationID, e.SpeakerID, e.EventType, nullableJSON(e.Detail), e.Actor,
	).Scan(&e.ID, &e.CreatedAt)
}

// InsertEventTx 写入一条名单事件流水
func (r *ApplicationRepo) InsertEventTx(tx *sql.Tx, e *models.RosterEvent) error {
	return tx.QueryRow(
		`INSERT INTO roster_events (point_id, application_id, speaker_id, event_type, detail, actor)
		 VALUES ($1, $2, $3, $4, COALESCE($5, '{}'::jsonb), $6)
		 RETURNING id, created_at`,
		e.PointID, e.ApplicationID, e.SpeakerID, e.EventType, nullableJSON(e.Detail), e.Actor,
	).Scan(&e.ID, &e.CreatedAt)
}

func nullableJSON(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

// ListRoster 名单（可按 status 过滤），联带发音人信息
func (r *ApplicationRepo) ListRoster(pointID int64, status string, offset, limit int) ([]*models.PointApplication, int, error) {
	var conditions []string
	args := []interface{}{pointID}
	argIdx := 1

	conditions = append(conditions, "a.point_id=$"+strconv.Itoa(argIdx))
	argIdx++
	if status != "" {
		conditions = append(conditions, "a.status=$"+strconv.Itoa(argIdx))
		args = append(args, status)
		argIdx++
	}

	where := "WHERE " + strings.Join(conditions, " AND ")

	var total int
	if err := r.db.QueryRow(
		`SELECT COUNT(*) FROM point_applications a `+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT ` + applicationColumns + applicationJoin + ` ` + where +
		` ORDER BY a.id DESC LIMIT $` + strconv.Itoa(argIdx) + ` OFFSET $` + strconv.Itoa(argIdx+1)
	args = append(args, limit, offset)

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var apps []*models.PointApplication
	for rows.Next() {
		a := &models.PointApplication{}
		if err := scanApplication(rows, a); err != nil {
			return nil, 0, err
		}
		apps = append(apps, a)
	}
	return apps, total, nil
}

// ListEvents 名单事件流（可按事件类型过滤），供回看"名单改过几回"
func (r *ApplicationRepo) ListEvents(pointID int64, eventType string, offset, limit int) ([]*models.RosterEvent, int, error) {
	where := "WHERE point_id=$1"
	args := []interface{}{pointID}
	if eventType != "" {
		where += " AND event_type=$2"
		args = append(args, eventType)
	}

	var total int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM roster_events `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT id, point_id, application_id, speaker_id, event_type, detail, actor, created_at
		 FROM roster_events ` + where + ` ORDER BY id DESC LIMIT $` + strconv.Itoa(len(args)+1) +
		` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, limit, offset)

	rows, err := r.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var events []*models.RosterEvent
	for rows.Next() {
		e := &models.RosterEvent{}
		if err := rows.Scan(&e.ID, &e.PointID, &e.ApplicationID, &e.SpeakerID, &e.EventType, &e.Detail, &e.Actor, &e.CreatedAt); err != nil {
			return nil, 0, err
		}
		events = append(events, e)
	}
	return events, total, nil
}
