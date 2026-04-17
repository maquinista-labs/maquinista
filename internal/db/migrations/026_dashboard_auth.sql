-- Migration 026: dashboard auth, audit, rate-limits. See
-- plans/active/dashboard.md Phase 6.

CREATE TABLE IF NOT EXISTS operator_credentials (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    username     TEXT        NOT NULL UNIQUE,
    pbkdf2_hash  TEXT        NOT NULL,
    salt         TEXT        NOT NULL,
    iter         INT         NOT NULL DEFAULT 600000,
    totp_secret  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at TIMESTAMPTZ,
    failed_attempts INT      NOT NULL DEFAULT 0,
    locked_until TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS dashboard_sessions (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id UUID        NOT NULL REFERENCES operator_credentials(id) ON DELETE CASCADE,
    token_hash  TEXT        NOT NULL UNIQUE,
    ua          TEXT,
    ip          INET,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_dashboard_sessions_expires
    ON dashboard_sessions (expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS dashboard_audit (
    id           BIGSERIAL   PRIMARY KEY,
    operator_id  UUID        REFERENCES operator_credentials(id) ON DELETE SET NULL,
    action       TEXT        NOT NULL,
    subject      JSONB       NOT NULL,
    ua           TEXT,
    ip           INET,
    ok           BOOLEAN     NOT NULL,
    error        TEXT,
    at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS dashboard_audit_operator_at_idx
    ON dashboard_audit (operator_id, at DESC);
CREATE INDEX IF NOT EXISTS dashboard_audit_action_at_idx
    ON dashboard_audit (action, at DESC);
