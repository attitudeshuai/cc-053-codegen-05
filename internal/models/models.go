package models

import (
	"encoding/json"
	"time"
)

type Speaker struct {
	ID               int64     `json:"id"`
	CodeName         string    `json:"code_name"`
	BirthYear        int       `json:"birth_year"`
	Gender           string    `json:"gender"`
	DialectPointCode string    `json:"dialect_point_code"`
	Occupation       string    `json:"occupation"`
	YearsAway        int       `json:"years_away"`
	ContactRef       string    `json:"contact_ref,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Wordlist struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	Entries   []Entry   `json:"entries"`
	IsCurrent bool      `json:"is_current"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Entry struct {
	ID     int64  `json:"id"`
	Hanzi  string `json:"hanzi"`
	Gloss  string `json:"gloss"`
	IPARef string `json:"ipa_ref,omitempty"`
	Group  string `json:"group,omitempty"`
}

type Task struct {
	ID         int64     `json:"id"`
	WordlistID int64     `json:"wordlist_id"`
	SpeakerID  int64     `json:"speaker_id"`
	Kind       string    `json:"kind"` // record | annotate
	Assignee   string    `json:"assignee"`
	Status     string    `json:"status"` // pending | in_progress | completed | failed
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Recording struct {
	ID           int64      `json:"id"`
	TaskID       int64      `json:"task_id"`
	ObjectKey    string     `json:"object_key"`
	DurationMs   int        `json:"duration_ms"`
	SampleRate   int        `json:"sample_rate"`
	PeakDB       float64    `json:"peak_db"`
	Device       string     `json:"device"`
	RecordedAt   *time.Time `json:"recorded_at"`
	Status       string     `json:"status"` // pending | processing | completed | rejected
	RejectReason string     `json:"reject_reason,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type Segment struct {
	ID          int64     `json:"id"`
	RecordingID int64     `json:"recording_id"`
	EntryID     int64     `json:"entry_id"`
	StartMs     int       `json:"start_ms"`
	EndMs       int       `json:"end_ms"`
	ObjectKey   string    `json:"object_key"`
	SnrDB       float64   `json:"snr_db"`
	Status      string    `json:"status"` // pending | annotated | in_arbitration | completed | failed
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Annotation struct {
	ID        int64     `json:"id"`
	SegmentID int64     `json:"segment_id"`
	Annotator string    `json:"annotator"`
	IPA       string    `json:"ipa"`
	Tone      string    `json:"tone"`
	Note      string    `json:"note"`
	Decision  string    `json:"decision"` // pending | accept | reject | arbitrated
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Arbitration struct {
	ID                 int64     `json:"id"`
	SegmentID          int64     `json:"segment_id"`
	WinnerAnnotationID int64     `json:"winner_annotation_id"`
	Arbiter            string    `json:"arbiter"`
	Reason             string    `json:"reason"`
	CreatedAt          time.Time `json:"created_at"`
}

type ExportJob struct {
	ID          int64     `json:"id"`
	Filter      string    `json:"filter"`
	Status      string    `json:"status"` // pending | processing | completed | failed
	Progress    int       `json:"progress"`
	OutputKey   string    `json:"output_key"`
	ErrorMessage string   `json:"error_message,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Pagination
type Pagination struct {
	Offset int `form:"offset" json:"offset"`
	Limit  int `form:"limit" json:"limit"`
}

func (p *Pagination) Normalize() {
	if p.Limit <= 0 || p.Limit > 100 {
		p.Limit = 20
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
}

// ===== 发音人遴选与名额管理 =====

// SurveyPoint 调查点：一个方言点的名额计划
type SurveyPoint struct {
	ID         int64            `json:"id"`
	Code       string           `json:"code"`
	Name       string           `json:"name"`
	QuotaTotal int              `json:"quota_total"` // = 各条件 seats 之和，校验一致性
	Remark     string           `json:"remark,omitempty"`
	Criteria   []QuotaCriterion `json:"criteria,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

// QuotaCriterion 名额条件：gender + 出生年份区间，需要 Seats 人
type QuotaCriterion struct {
	ID           int64     `json:"id,omitempty"`
	PointID      int64     `json:"point_id,omitempty"`
	Gender       string    `json:"gender"` // male | female | other
	MinBirthYear int       `json:"min_birth_year"`
	MaxBirthYear int       `json:"max_birth_year"`
	Seats        int       `json:"seats"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

// PointApplication 报名/占位记录
type PointApplication struct {
	ID           int64      `json:"id"`
	PointID      int64      `json:"point_id"`
	SpeakerID    int64      `json:"speaker_id"`
	CriterionID  *int64     `json:"criterion_id,omitempty"`
	Status       string     `json:"status"` // enrolled | waiting | withdrawn | rejected
	RejectReason string     `json:"reject_reason,omitempty"`
	QueueSeq     *int64     `json:"queue_seq,omitempty"`
	EnrolledAt   *time.Time `json:"enrolled_at,omitempty"`
	WithdrawnAt  *time.Time `json:"withdrawn_at,omitempty"`
	RejectedAt   *time.Time `json:"rejected_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`

	// 列表查询时联表带出发音人信息
	SpeakerCodeName string `json:"speaker_code_name,omitempty"`
	BirthYear       int    `json:"speaker_birth_year,omitempty"`
	Gender          string `json:"speaker_gender,omitempty"`
}

// RosterEvent 名单事件流水（报名/入选/退回/入队/退出/顶补/名额变更）
type RosterEvent struct {
	ID            int64           `json:"id"`
	PointID       int64           `json:"point_id"`
	ApplicationID *int64          `json:"application_id,omitempty"`
	SpeakerID     *int64          `json:"speaker_id,omitempty"`
	EventType     string          `json:"event_type"`
	Detail        json.RawMessage `json:"detail"`
	Actor         string          `json:"actor"`
	CreatedAt     time.Time       `json:"created_at"`
}