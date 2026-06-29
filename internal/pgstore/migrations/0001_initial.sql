CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS usage_events (
    id             BIGSERIAL PRIMARY KEY,
    event_type     TEXT        NOT NULL,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    consumer       TEXT        NOT NULL DEFAULT '',
    user_type      TEXT        NOT NULL DEFAULT '',
    subject        TEXT        NOT NULL DEFAULT '',
    service_type   TEXT        NOT NULL DEFAULT '',
    model          TEXT        NOT NULL DEFAULT '',
    provider       TEXT        NOT NULL DEFAULT '',
    prompt_tokens      INT,
    completion_tokens  INT,
    http_status        INT,
    duration_ms        BIGINT,
    cache_hit          BOOLEAN,
    job_id             TEXT,
    job_status         TEXT,
    processing_time_s  FLOAT8
);

CREATE INDEX IF NOT EXISTS ue_occurred_at  ON usage_events (occurred_at DESC);
CREATE INDEX IF NOT EXISTS ue_consumer     ON usage_events (consumer, occurred_at DESC);
CREATE INDEX IF NOT EXISTS ue_service_type ON usage_events (service_type, occurred_at DESC);

INSERT INTO schema_migrations (version) VALUES (1) ON CONFLICT DO NOTHING;
