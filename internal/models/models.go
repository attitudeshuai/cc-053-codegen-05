package models

import (
	"fmt"
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

// QuotaPlan 发音人遴选名额方案：每个调查点需要多少人、年龄与性别要求
type QuotaPlan struct {
	ID               int64     `json:"id"`
	DialectPointCode string    `json:"dialect_point_code"`
	RequiredCount    int       `json:"required_count"`
	MinAge           int       `json:"min_age"`
	MaxAge           int       `json:"max_age"`
	GenderReq        string    `json:"gender_req"` // any | male | female | other
	CreatedBy        string    `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// EligibilityFailures 按方案条件过筛，返回不满足的条目说明（空切片表示全部满足）
func (p *QuotaPlan) EligibilityFailures(s *Speaker, currentYear int) []string {
	var failures []string
	age := currentYear - s.BirthYear
	if age < p.MinAge || age > p.MaxAge {
		failures = append(failures, fmt.Sprintf("年龄 %d 岁不在方案要求的 %d-%d 岁范围内", age, p.MinAge, p.MaxAge))
	}
	if p.GenderReq != "any" && s.Gender != p.GenderReq {
		failures = append(failures, fmt.Sprintf("性别 %s 不符合方案要求的 %s", s.Gender, p.GenderReq))
	}
	return failures
}

// QuotaPlanWithStats 方案及其名额占用统计
type QuotaPlanWithStats struct {
	QuotaPlan
	AcceptedCount int `json:"accepted_count"`
	WaitingCount  int `json:"waiting_count"`
}

// SpeakerApplication 报名记录：一次报名经筛选后处于 录取/排队/退回/退出 之一
type SpeakerApplication struct {
	ID            int64     `json:"id"`
	PlanID        int64     `json:"plan_id"`
	SpeakerID     int64     `json:"speaker_id"`
	Status        string    `json:"status"` // accepted | waiting | rejected | withdrawn
	RejectReasons []string  `json:"reject_reasons"`
	QueuePosition int       `json:"queue_position"`
	Operator      string    `json:"operator"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// RosterEvent 名单变更事件：谁在什么时间把名单从什么状态改成什么状态（可回看）
type RosterEvent struct {
	ID            int64     `json:"id"`
	PlanID        int64     `json:"plan_id"`
	ApplicationID int64     `json:"application_id"`
	SpeakerID     int64     `json:"speaker_id"`
	EventType     string    `json:"event_type"` // accepted | waitlisted | rejected | withdrawn | promoted
	FromStatus    string    `json:"from_status"`
	ToStatus      string    `json:"to_status"`
	Operator      string    `json:"operator"`
	Reason        string    `json:"reason"`
	CreatedAt     time.Time `json:"created_at"`
}

// RosterEntry 名单条目：报名记录 + 发音人公开信息
type RosterEntry struct {
	SpeakerApplication
	SpeakerCodeName  string `json:"speaker_code_name"`
	SpeakerGender    string `json:"speaker_gender"`
	SpeakerBirthYear int    `json:"speaker_birth_year"`
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