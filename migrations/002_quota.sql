-- Dialect Corpus Platform - Speaker Selection & Quota Management
-- This file is for reference; actual migrations run via internal/database/database.go

CREATE TABLE IF NOT EXISTS quota_plans (
    id BIGSERIAL PRIMARY KEY,
    dialect_point_code VARCHAR(50) NOT NULL,
    required_count INT NOT NULL CHECK (required_count > 0),
    min_age INT NOT NULL DEFAULT 0,
    max_age INT NOT NULL DEFAULT 150,
    gender_req VARCHAR(10) NOT NULL DEFAULT 'any' CHECK (gender_req IN ('any','male','female','other')),
    created_by VARCHAR(200) DEFAULT '',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS speaker_applications (
    id BIGSERIAL PRIMARY KEY,
    plan_id BIGINT NOT NULL REFERENCES quota_plans(id),
    speaker_id BIGINT NOT NULL REFERENCES speakers(id),
    status VARCHAR(20) NOT NULL CHECK (status IN ('accepted','waiting','rejected','withdrawn')),
    reject_reasons JSONB NOT NULL DEFAULT '[]',
    queue_position INT NOT NULL DEFAULT 0,
    operator VARCHAR(200) DEFAULT '',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS roster_events (
    id BIGSERIAL PRIMARY KEY,
    plan_id BIGINT NOT NULL REFERENCES quota_plans(id),
    application_id BIGINT NOT NULL REFERENCES speaker_applications(id),
    speaker_id BIGINT NOT NULL REFERENCES speakers(id),
    event_type VARCHAR(20) NOT NULL CHECK (event_type IN ('accepted','waitlisted','rejected','withdrawn','promoted')),
    from_status VARCHAR(20) NOT NULL DEFAULT '',
    to_status VARCHAR(20) NOT NULL DEFAULT '',
    operator VARCHAR(200) NOT NULL DEFAULT '',
    reason TEXT DEFAULT '',
    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_applications_plan ON speaker_applications(plan_id);
CREATE INDEX IF NOT EXISTS idx_applications_plan_status ON speaker_applications(plan_id, status);
-- 同一发音人全局只能有一条活跃报名（accepted/waiting），保证不同时占两个点的名额
CREATE UNIQUE INDEX IF NOT EXISTS idx_applications_one_active_per_speaker ON speaker_applications (speaker_id) WHERE status IN ('accepted','waiting');
CREATE INDEX IF NOT EXISTS idx_roster_events_plan ON roster_events(plan_id);
CREATE INDEX IF NOT EXISTS idx_roster_events_application ON roster_events(application_id);
