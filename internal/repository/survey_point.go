package repository

import (
	"database/sql"

	"cc-053/internal/models"
)

type SurveyPointRepo struct {
	db *sql.DB
}

func NewSurveyPointRepo(db *sql.DB) *SurveyPointRepo {
	return &SurveyPointRepo{db: db}
}

// CreateWithCriteria 在一个事务内创建调查点并写入名额条件，quota_total = 各条件 seats 之和
func (r *SurveyPointRepo) CreateWithCriteria(p *models.SurveyPoint, criteria []models.QuotaCriterion) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := r.CreateWithCriteriaTx(tx, p, criteria); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateWithCriteriaTx 事务内创建调查点与名额条件（调用方持有事务）
func (r *SurveyPointRepo) CreateWithCriteriaTx(tx *sql.Tx, p *models.SurveyPoint, criteria []models.QuotaCriterion) error {
	quotaTotal := 0
	for _, c := range criteria {
		quotaTotal += c.Seats
	}
	p.QuotaTotal = quotaTotal

	err := tx.QueryRow(
		`INSERT INTO survey_points (code, name, quota_total, remark)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, created_at, updated_at`,
		p.Code, p.Name, p.QuotaTotal, p.Remark,
	).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return err
	}

	for i := range criteria {
		c := &criteria[i]
		c.PointID = p.ID
		if err := insertCriterionTx(tx, c); err != nil {
			return err
		}
	}
	p.Criteria = criteria
	return nil
}

func insertCriterionTx(tx *sql.Tx, c *models.QuotaCriterion) error {
	return tx.QueryRow(
		`INSERT INTO quota_criteria (point_id, gender, min_birth_year, max_birth_year, seats)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, created_at, updated_at`,
		c.PointID, c.Gender, c.MinBirthYear, c.MaxBirthYear, c.Seats,
	).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
}

// ReplaceCriteriaTx 替换某点的全部名额条件并重算 quota_total（调用方持有事务）
func (r *SurveyPointRepo) ReplaceCriteriaTx(tx *sql.Tx, pointID int64, criteria []models.QuotaCriterion) error {
	if _, err := tx.Exec(`DELETE FROM quota_criteria WHERE point_id=$1`, pointID); err != nil {
		return err
	}

	quotaTotal := 0
	for i := range criteria {
		c := &criteria[i]
		c.PointID = pointID
		quotaTotal += c.Seats
		if err := insertCriterionTx(tx, c); err != nil {
			return err
		}
	}

	_, err := tx.Exec(
		`UPDATE survey_points SET quota_total=$1, updated_at=NOW() WHERE id=$2`,
		quotaTotal, pointID,
	)
	return err
}

func (r *SurveyPointRepo) UpdateRemark(id int64, remark string) error {
	_, err := r.db.Exec(`UPDATE survey_points SET remark=$1, updated_at=NOW() WHERE id=$2`, remark, id)
	return err
}

// UpdateRemarkTx 事务内更新备注
func (r *SurveyPointRepo) UpdateRemarkTx(tx *sql.Tx, id int64, remark string) error {
	_, err := tx.Exec(`UPDATE survey_points SET remark=$1, updated_at=NOW() WHERE id=$2`, remark, id)
	return err
}

const surveyPointColumns = `id, code, name, quota_total, remark, created_at, updated_at`

func scanSurveyPoint(row interface {
	Scan(dest ...interface{}) error
}, p *models.SurveyPoint) error {
	return row.Scan(&p.ID, &p.Code, &p.Name, &p.QuotaTotal, &p.Remark, &p.CreatedAt, &p.UpdatedAt)
}

func (r *SurveyPointRepo) GetByID(id int64) (*models.SurveyPoint, error) {
	p := &models.SurveyPoint{}
	if err := scanSurveyPoint(
		r.db.QueryRow(`SELECT `+surveyPointColumns+` FROM survey_points WHERE id=$1`, id), p,
	); err != nil {
		return nil, err
	}
	return p, nil
}

// GetTx 可在事务内读取并对调查点加行锁（FOR UPDATE），串行化同点的报名/退出/顶替
func (r *SurveyPointRepo) GetTx(tx *sql.Tx, id int64, forUpdate bool) (*models.SurveyPoint, error) {
	q := `SELECT ` + surveyPointColumns + ` FROM survey_points WHERE id=$1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	p := &models.SurveyPoint{}
	if err := scanSurveyPoint(tx.QueryRow(q, id), p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *SurveyPointRepo) ListCriteria(pointID int64) ([]models.QuotaCriterion, error) {
	rows, err := r.db.Query(
		`SELECT id, point_id, gender, min_birth_year, max_birth_year, seats, created_at, updated_at
		 FROM quota_criteria WHERE point_id=$1 ORDER BY id`, pointID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCriteria(rows)
}

func (r *SurveyPointRepo) ListCriteriaTx(tx *sql.Tx, pointID int64) ([]models.QuotaCriterion, error) {
	rows, err := tx.Query(
		`SELECT id, point_id, gender, min_birth_year, max_birth_year, seats, created_at, updated_at
		 FROM quota_criteria WHERE point_id=$1 ORDER BY id`, pointID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCriteria(rows)
}

func scanCriteria(rows *sql.Rows) ([]models.QuotaCriterion, error) {
	var criteria []models.QuotaCriterion
	for rows.Next() {
		var c models.QuotaCriterion
		if err := rows.Scan(&c.ID, &c.PointID, &c.Gender, &c.MinBirthYear, &c.MaxBirthYear, &c.Seats, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		criteria = append(criteria, c)
	}
	return criteria, nil
}

// GetWithCriteria 读取调查点详情（含名额条件）
func (r *SurveyPointRepo) GetWithCriteria(id int64) (*models.SurveyPoint, error) {
	p, err := r.GetByID(id)
	if err != nil {
		return nil, err
	}
	criteria, err := r.ListCriteria(id)
	if err != nil {
		return nil, err
	}
	p.Criteria = criteria
	return p, nil
}

func (r *SurveyPointRepo) List(offset, limit int) ([]*models.SurveyPoint, int, error) {
	var total int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM survey_points`).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := r.db.Query(
		`SELECT `+surveyPointColumns+` FROM survey_points ORDER BY id DESC LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var points []*models.SurveyPoint
	for rows.Next() {
		p := &models.SurveyPoint{}
		if err := scanSurveyPoint(rows, p); err != nil {
			return nil, 0, err
		}
		points = append(points, p)
	}
	return points, total, nil
}
