package services_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"cc-053/internal/database"
	"cc-053/internal/models"
	"cc-053/internal/repository"
	"cc-053/internal/services"
)

func setupRecruitment(t *testing.T) (*services.RecruitmentService, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("DIALECT_TEST_DSN")
	if dsn == "" {
		t.Skip("DIALECT_TEST_DSN not set, skipping integration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	if err := database.RunMigrations(db); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	_, _ = db.Exec(`TRUNCATE roster_events, point_applications, quota_criteria, survey_points, speakers CASCADE`)

	pointRepo := repository.NewSurveyPointRepo(db)
	appRepo := repository.NewApplicationRepo(db)
	speakerRepo := repository.NewSpeakerRepo(db)
	return services.NewRecruitmentService(db, pointRepo, appRepo, speakerRepo), db
}

func createSpeaker(t *testing.T, db *sql.DB, code string, birth int, gender string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(
		`INSERT INTO speakers (code_name, birth_year, gender, dialect_point_code) VALUES ($1,$2,$3,'TEST') RETURNING id`,
		code, birth, gender,
	).Scan(&id)
	if err != nil {
		t.Fatalf("create speaker %s: %v", code, err)
	}
	return id
}

func criteria(gender string, minY, maxY, seats int) models.CriterionInput {
	return models.CriterionInput{Gender: gender, MinBirthYear: minY, MaxBirthYear: maxY, Seats: seats}
}

func TestApplyEnrollRejectWaitlistPromotion(t *testing.T) {
	svc, db := setupRecruitment(t)
	defer db.Close()

	// 名额：男性 1950-1980 共 2 人；女性 1960-1990 共 1 人
	point, err := svc.CreatePoint("P-A", "甲点", "", []models.CriterionInput{
		criteria("male", 1950, 1980, 2),
		criteria("female", 1960, 1990, 1),
	}, "admin")
	if err != nil {
		t.Fatalf("create point: %v", err)
	}
	if point.QuotaTotal != 3 {
		t.Fatalf("quota total = %d, want 3", point.QuotaTotal)
	}

	// 建点/扩容后用于顶补的女性等待者
	s8 := createSpeaker(t, db, "S8", 1985, "female")

	s1 := createSpeaker(t, db, "S1", 1970, "male")
	s2 := createSpeaker(t, db, "S2", 1975, "male")
	s3 := createSpeaker(t, db, "S3", 1940, "male")   // 年龄不符
	s4 := createSpeaker(t, db, "S4", 1970, "female") // 入选（女性名额）
	s5 := createSpeaker(t, db, "S5", 1972, "male")   // 满员后排队
	s6 := createSpeaker(t, db, "S6", 1940, "female") // 年龄不符
	s7 := createSpeaker(t, db, "S7", 1970, "other")  // 性别不符

	mustEnroll := func(speaker int64) {
		t.Helper()
		r, err := svc.Apply(point.ID, speaker, "clerk-a")
		if err != nil {
			t.Fatalf("speaker %d apply: %v", speaker, err)
		}
		if r.Status != "enrolled" {
			t.Fatalf("speaker %d status = %s, want enrolled", speaker, r.Status)
		}
	}
	mustEnroll(s1)
	mustEnroll(s2)

	// 当场退回：年龄不符，原因须点名"出生年份"
	r3, err := svc.Apply(point.ID, s3, "clerk-a")
	if err != nil {
		t.Fatalf("s3 apply: %v", err)
	}
	if r3.Status != "rejected" || len(r3.RejectReasons) != 1 {
		t.Fatalf("s3 result = %+v, want single reject reason", r3)
	}
	if !contains(r3.RejectReasons[0], "出生年份") {
		t.Fatalf("s3 reason = %q, want it to mention birth year", r3.RejectReasons[0])
	}

	mustEnroll(s4)

	// 满员后的男性报名 → 等待队列
	r5, err := svc.Apply(point.ID, s5, "clerk-a")
	if err != nil {
		t.Fatalf("s5 apply: %v", err)
	}
	if r5.Status != "waiting" || r5.QueueSeq == nil || *r5.QueueSeq != 1 {
		t.Fatalf("s5 result = %+v, want waiting queue_seq=1", r5)
	}

	// 满员后的女性报名也排队（扩容时将被顶补）
	r8, err := svc.Apply(point.ID, s8, "clerk-a")
	if err != nil {
		t.Fatalf("s8 apply: %v", err)
	}
	if r8.Status != "waiting" {
		t.Fatalf("s8 status = %s, want waiting", r8.Status)
	}

	// 当场退回：性别不符
	r7, err := svc.Apply(point.ID, s7, "clerk-a")
	if err != nil {
		t.Fatalf("s7 apply: %v", err)
	}
	if r7.Status != "rejected" || !contains(r7.RejectReasons[0], "性别") {
		t.Fatalf("s7 result = %+v, want gender reject", r7)
	}

	// 当场退回：女性年龄不符
	r6, err := svc.Apply(point.ID, s6, "clerk-a")
	if err != nil {
		t.Fatalf("s6 apply: %v", err)
	}
	if r6.Status != "rejected" || !contains(r6.RejectReasons[0], "出生年份") {
		t.Fatalf("s6 result = %+v, want birth-year reject", r6)
	}

	// 已在队列里的人不能报第二个点
	pointB, err := svc.CreatePoint("P-B", "乙点", "", []models.CriterionInput{criteria("male", 1900, 2999, 5)}, "admin")
	if err != nil {
		t.Fatalf("create point B: %v", err)
	}
	_, err = svc.Apply(pointB.ID, s5, "clerk-b")
	if !isConflict(err) {
		t.Fatalf("s5 apply point B: err = %v, want conflict", err)
	}

	// 重复报同一点
	_, err = svc.Apply(point.ID, s1, "clerk-a")
	if !isConflict(err) {
		t.Fatalf("s1 re-apply same point: err = %v, want conflict", err)
	}

	// s1 退出 → s5（排队最靠前且同条件）顶补
	w, err := svc.Withdraw(point.ID, s1, "clerk-a", "临时外出")
	if err != nil {
		t.Fatalf("withdraw s1: %v", err)
	}
	if w.PreviousStatus != "enrolled" {
		t.Fatalf("withdraw previous = %s, want enrolled", w.PreviousStatus)
	}
	if w.Promoted == nil || w.Promoted.SpeakerID != s5 {
		t.Fatalf("promoted = %+v, want s5", w.Promoted)
	}

	// 顶补后 s5 已占名额，仍不能报第二个点
	_, err = svc.Apply(pointB.ID, s5, "clerk-b")
	if !isConflict(err) {
		t.Fatalf("s5 apply point B after promotion: err = %v, want conflict", err)
	}

	// 排队者自己退出，不应触发顶替（此时仅剩 s8 排队，且无空位）
	w2, err := svc.Withdraw(point.ID, s8, "clerk-a", "")
	if err != nil {
		t.Fatalf("withdraw waiting s8: %v", err)
	}
	if w2.PreviousStatus != "waiting" || w2.Promoted != nil {
		t.Fatalf("waiting withdraw result = %+v", w2)
	}

	// 事件流：退出与顶替都要留痕
	var nEvents int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM roster_events WHERE point_id=$1 AND event_type IN ('withdrawn','promoted')`,
		point.ID,
	).Scan(&nEvents); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if nEvents < 3 { // s1 withdrawn + s5 promoted + s8 withdrawn
		t.Fatalf("withdrawn/promoted events = %d, want >= 3", nEvents)
	}

	// 经办人与时间必须落库
	var actor string
	var hasTime bool
	if err := db.QueryRow(
		`SELECT actor, (created_at IS NOT NULL) FROM roster_events WHERE event_type='promoted' LIMIT 1`,
	).Scan(&actor, &hasTime); err != nil {
		t.Fatalf("read promoted event: %v", err)
	}
	if actor != "clerk-a" || !hasTime {
		t.Fatalf("promoted event actor=%q hasTime=%v", actor, hasTime)
	}
}

func TestUpdateQuotaPromotionAndGuard(t *testing.T) {
	svc, db := setupRecruitment(t)
	defer db.Close()

	point, err := svc.CreatePoint("P-C", "丙点", "", []models.CriterionInput{
		criteria("male", 1950, 1980, 2),
		criteria("female", 1960, 1990, 1),
	}, "admin")
	if err != nil {
		t.Fatalf("create point: %v", err)
	}

	s1 := createSpeaker(t, db, "M1", 1970, "male")
	s2 := createSpeaker(t, db, "M2", 1971, "male")
	s3 := createSpeaker(t, db, "F1", 1975, "female")
	s4 := createSpeaker(t, db, "F2", 1985, "female") // 将排队
	s5 := createSpeaker(t, db, "M3", 1920, "male")   // 被退回的人不影响后续

	for _, s := range []int64{s1, s2, s3} {
		if _, err := svc.Apply(point.ID, s, "clerk-a"); err != nil {
			t.Fatalf("enroll %d: %v", s, err)
		}
	}
	if r, err := svc.Apply(point.ID, s4, "clerk-a"); err != nil || r.Status != "waiting" {
		t.Fatalf("s4 = %+v, %v; want waiting", r, err)
	}
	if r, err := svc.Apply(point.ID, s5, "clerk-a"); err != nil || r.Status != "rejected" {
		t.Fatalf("s5 = %+v, %v; want rejected", r, err)
	}

	// 缩容到无法容纳现有入选人（2 名男性 vs 1 个名额）→ 整体拒绝
	_, err = svc.UpdateQuota(point.ID, []models.CriterionInput{
		criteria("male", 1950, 1980, 1),
		criteria("female", 1960, 1990, 1),
	}, "admin", nil)
	if !isConflict(err) {
		t.Fatalf("shrink quota: err = %v, want conflict", err)
	}
	// 拒绝后原方案不变
	var quota int
	if err := db.QueryRow(`SELECT quota_total FROM survey_points WHERE id=$1`, point.ID).Scan(&quota); err != nil {
		t.Fatalf("read quota: %v", err)
	}
	if quota != 3 {
		t.Fatalf("quota after rejected update = %d, want 3", quota)
	}

	// 女性扩容到 2 → s4 按排队顺序顶补
	res, err := svc.UpdateQuota(point.ID, []models.CriterionInput{
		criteria("male", 1950, 1980, 2),
		criteria("female", 1960, 1990, 2),
	}, "admin", nil)
	if err != nil {
		t.Fatalf("expand quota: %v", err)
	}
	if res.Point.QuotaTotal != 4 {
		t.Fatalf("new quota = %d, want 4", res.Point.QuotaTotal)
	}
	if len(res.Promoted) != 1 || res.Promoted[0].SpeakerID != s4 {
		t.Fatalf("promotions = %+v, want s4 only", res.Promoted)
	}

	// 顶补后队列清空
	var waiting int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM point_applications WHERE point_id=$1 AND status='waiting'`, point.ID,
	).Scan(&waiting); err != nil {
		t.Fatalf("count waiting: %v", err)
	}
	if waiting != 0 {
		t.Fatalf("waiting = %d, want 0", waiting)
	}

	// 名额变更事件可回看（含改前改后）
	var changeEvents int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM roster_events WHERE point_id=$1 AND event_type IN ('quota_changed','criteria_changed')`,
		point.ID,
	).Scan(&changeEvents); err != nil {
		t.Fatalf("count change events: %v", err)
	}
	if changeEvents < 2 {
		t.Fatalf("change events = %d, want >= 2", changeEvents)
	}
}

func isConflict(err error) bool {
	if se, ok := err.(*services.ServiceError); ok {
		return se.Kind == services.KindConflict
	}
	return false
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
