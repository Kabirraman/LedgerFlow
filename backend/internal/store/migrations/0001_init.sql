-- LEDGERFLOW initial schema (SRS 14.1).
--
-- Conventions:
--   * All monetary amounts are BIGINT paise, matching Razorpay's API. Integer
--     money keeps recovery totals exact.
--   * environment is stamped on every business record (SRS FR-006) and
--     constrained to 'test' so live data cannot land in a prototype database.
--   * Tables that record decisions or side effects are append-only by
--     application convention (SRS FR-052); the only UPDATEs are the explicit
--     status transitions noted below.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     TEXT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------- users

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL CHECK (role IN ('operator','reviewer','admin')),
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------ customers

CREATE TABLE IF NOT EXISTS customers (
    id                   TEXT PRIMARY KEY,
    razorpay_customer_id TEXT,
    name                 TEXT NOT NULL DEFAULT '',
    email                TEXT NOT NULL DEFAULT '',
    contact              TEXT NOT NULL DEFAULT '',
    segment              TEXT NOT NULL CHECK (segment IN ('new','repeat','high_value','b2b','subscription')),
    lifetime_value       BIGINT NOT NULL DEFAULT 0,
    success_rate         DOUBLE PRECISION NOT NULL DEFAULT 0,
    total_payments       INTEGER NOT NULL DEFAULT 0,
    environment          TEXT NOT NULL DEFAULT 'test' CHECK (environment = 'test'),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT customers_success_rate_range CHECK (success_rate >= 0 AND success_rate <= 1)
);

CREATE UNIQUE INDEX IF NOT EXISTS customers_razorpay_id_uidx
    ON customers (razorpay_customer_id) WHERE razorpay_customer_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS customers_segment_idx ON customers (segment);

-- --------------------------------------------------------- transactions

CREATE TABLE IF NOT EXISTS transactions (
    id                  TEXT PRIMARY KEY,
    razorpay_payment_id TEXT,
    razorpay_order_id   TEXT,
    customer_id         TEXT NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    amount              BIGINT NOT NULL CHECK (amount >= 0),
    currency            TEXT NOT NULL DEFAULT 'INR',
    status              TEXT NOT NULL,
    method              TEXT NOT NULL DEFAULT '',
    failure_reason      TEXT NOT NULL DEFAULT '',
    error_code          TEXT NOT NULL DEFAULT '',
    attempt_count       INTEGER NOT NULL DEFAULT 1,
    environment         TEXT NOT NULL DEFAULT 'test' CHECK (environment = 'test'),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A Razorpay payment id maps to exactly one transaction row. This is the
-- reconciliation anchor for backfill and webhook ingestion (SRS FR-003).
CREATE UNIQUE INDEX IF NOT EXISTS transactions_razorpay_payment_uidx
    ON transactions (razorpay_payment_id) WHERE razorpay_payment_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS transactions_customer_idx ON transactions (customer_id);
CREATE INDEX IF NOT EXISTS transactions_status_created_idx ON transactions (status, created_at DESC);

-- ---------------------------------------------------- checkout_sessions

CREATE TABLE IF NOT EXISTS checkout_sessions (
    id               TEXT PRIMARY KEY,
    customer_id      TEXT NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    cart_amount      BIGINT NOT NULL CHECK (cart_amount >= 0),
    item_count       INTEGER NOT NULL DEFAULT 1,
    page_views       INTEGER NOT NULL DEFAULT 1,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_activity_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','abandoned','converted')),
    environment      TEXT NOT NULL DEFAULT 'test' CHECK (environment = 'test')
);

CREATE INDEX IF NOT EXISTS checkout_sessions_status_idx ON checkout_sessions (status, last_activity_at);

-- ------------------------------------------------------------- invoices

CREATE TABLE IF NOT EXISTS invoices (
    id                  TEXT PRIMARY KEY,
    razorpay_invoice_id TEXT,
    customer_id         TEXT NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    invoice_number      TEXT NOT NULL DEFAULT '',
    amount              BIGINT NOT NULL CHECK (amount >= 0),
    amount_paid         BIGINT NOT NULL DEFAULT 0,
    status              TEXT NOT NULL DEFAULT 'issued',
    due_date            TIMESTAMPTZ NOT NULL,
    reminder_count      INTEGER NOT NULL DEFAULT 0,
    environment         TEXT NOT NULL DEFAULT 'test' CHECK (environment = 'test'),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS invoices_razorpay_id_uidx
    ON invoices (razorpay_invoice_id) WHERE razorpay_invoice_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS invoices_status_due_idx ON invoices (status, due_date);

-- -------------------------------------------------------- subscriptions

CREATE TABLE IF NOT EXISTS subscriptions (
    id                       TEXT PRIMARY KEY,
    razorpay_subscription_id TEXT,
    customer_id              TEXT NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    plan_id                  TEXT NOT NULL DEFAULT '',
    amount                   BIGINT NOT NULL CHECK (amount >= 0),
    status                   TEXT NOT NULL DEFAULT 'active',
    failed_charge_count      INTEGER NOT NULL DEFAULT 0,
    current_end              TIMESTAMPTZ,
    environment              TEXT NOT NULL DEFAULT 'test' CHECK (environment = 'test'),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS subscriptions_razorpay_id_uidx
    ON subscriptions (razorpay_subscription_id) WHERE razorpay_subscription_id IS NOT NULL;

-- ------------------------------------------------------------ risk_cases

-- case_seq backs the human-facing REV-#### reference.
CREATE SEQUENCE IF NOT EXISTS risk_case_seq START 1;

CREATE TABLE IF NOT EXISTS risk_cases (
    id                  TEXT PRIMARY KEY,
    reference           TEXT NOT NULL UNIQUE,
    source_type         TEXT NOT NULL CHECK (source_type IN
                          ('payment_failure','checkout_abandonment','invoice_overdue','subscription_failure')),
    customer_id         TEXT NOT NULL REFERENCES customers (id) ON DELETE CASCADE,
    transaction_id      TEXT REFERENCES transactions (id) ON DELETE SET NULL,
    checkout_session_id TEXT REFERENCES checkout_sessions (id) ON DELETE SET NULL,
    invoice_id          TEXT REFERENCES invoices (id) ON DELETE SET NULL,
    subscription_id     TEXT REFERENCES subscriptions (id) ON DELETE SET NULL,
    revenue_at_risk     BIGINT NOT NULL DEFAULT 0,
    risk_score          DOUBLE PRECISION NOT NULL DEFAULT 0,
    urgency             TEXT NOT NULL DEFAULT 'low' CHECK (urgency IN ('low','medium','high','critical')),
    expected_recovery   BIGINT NOT NULL DEFAULT 0,
    recovered_amount    BIGINT NOT NULL DEFAULT 0,
    status              TEXT NOT NULL DEFAULT 'NEW',
    reason_codes        JSONB NOT NULL DEFAULT '[]'::jsonb,
    evidence_refs       JSONB NOT NULL DEFAULT '[]'::jsonb,
    stop_reason         TEXT NOT NULL DEFAULT '',
    action_count        INTEGER NOT NULL DEFAULT 0,
    mode                TEXT NOT NULL DEFAULT 'live_test' CHECK (mode IN ('live_test','simulation','review')),
    simulation_id       TEXT,
    environment         TEXT NOT NULL DEFAULT 'test' CHECK (environment = 'test'),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at           TIMESTAMPTZ,
    -- Exactly one source pointer must be set, and it must match source_type.
    CONSTRAINT risk_cases_source_pointer CHECK (
        (source_type = 'payment_failure'       AND transaction_id      IS NOT NULL) OR
        (source_type = 'checkout_abandonment'  AND checkout_session_id IS NOT NULL) OR
        (source_type = 'invoice_overdue'       AND invoice_id          IS NOT NULL) OR
        (source_type = 'subscription_failure'  AND subscription_id     IS NOT NULL)
    )
);

-- One open case per source record: prevents duplicate cases when a webhook is
-- redelivered or a backfill overlaps an earlier sync (SRS FR-003).
CREATE UNIQUE INDEX IF NOT EXISTS risk_cases_open_transaction_uidx
    ON risk_cases (transaction_id) WHERE transaction_id IS NOT NULL AND mode = 'live_test';
CREATE UNIQUE INDEX IF NOT EXISTS risk_cases_open_checkout_uidx
    ON risk_cases (checkout_session_id) WHERE checkout_session_id IS NOT NULL AND mode = 'live_test';
CREATE UNIQUE INDEX IF NOT EXISTS risk_cases_open_invoice_uidx
    ON risk_cases (invoice_id) WHERE invoice_id IS NOT NULL AND mode = 'live_test';
CREATE UNIQUE INDEX IF NOT EXISTS risk_cases_open_subscription_uidx
    ON risk_cases (subscription_id) WHERE subscription_id IS NOT NULL AND mode = 'live_test';

CREATE INDEX IF NOT EXISTS risk_cases_queue_idx ON risk_cases (status, expected_recovery DESC, risk_score DESC);
CREATE INDEX IF NOT EXISTS risk_cases_customer_idx ON risk_cases (customer_id, created_at DESC);
CREATE INDEX IF NOT EXISTS risk_cases_source_idx ON risk_cases (source_type, created_at DESC);
CREATE INDEX IF NOT EXISTS risk_cases_mode_idx ON risk_cases (mode);
CREATE INDEX IF NOT EXISTS risk_cases_simulation_idx ON risk_cases (simulation_id) WHERE simulation_id IS NOT NULL;

-- ------------------------------------------------------------ diagnoses

CREATE TABLE IF NOT EXISTS diagnoses (
    id                TEXT PRIMARY KEY,
    case_id           TEXT NOT NULL REFERENCES risk_cases (id) ON DELETE CASCADE,
    root_cause        TEXT NOT NULL CHECK (root_cause IN
                        ('transient_failure','insufficient_funds','checkout_abandonment',
                         'overdue_receivable','subscription_failure','authentication_failed','unknown')),
    confidence        DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (confidence >= 0 AND confidence <= 1),
    evidence_json     JSONB NOT NULL DEFAULT '[]'::jsonb,
    uncertainty_flags JSONB NOT NULL DEFAULT '[]'::jsonb,
    next_step         TEXT NOT NULL DEFAULT '',
    source            TEXT NOT NULL DEFAULT 'deterministic',
    model_name        TEXT NOT NULL DEFAULT '',
    latency_ms        BIGINT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS diagnoses_case_idx ON diagnoses (case_id, created_at DESC);

-- ------------------------------------------------------ agent_decisions

CREATE TABLE IF NOT EXISTS agent_decisions (
    id                   TEXT PRIMARY KEY,
    case_id              TEXT NOT NULL REFERENCES risk_cases (id) ON DELETE CASCADE,
    recommended_action   TEXT NOT NULL CHECK (recommended_action IN
                           ('retry','payment_link','reminder','escalate','no_action')),
    recovery_probability DOUBLE PRECISION NOT NULL DEFAULT 0
                           CHECK (recovery_probability >= 0 AND recovery_probability <= 1),
    expected_recovery    BIGINT NOT NULL DEFAULT 0,
    reason_codes         JSONB NOT NULL DEFAULT '[]'::jsonb,
    alternatives         JSONB NOT NULL DEFAULT '[]'::jsonb,
    stop_condition       TEXT NOT NULL DEFAULT '',
    policy_version       TEXT NOT NULL DEFAULT 'v1',
    source               TEXT NOT NULL DEFAULT 'deterministic',
    model_name           TEXT NOT NULL DEFAULT '',
    latency_ms           BIGINT NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS agent_decisions_case_idx ON agent_decisions (case_id, created_at DESC);

-- -------------------------------------------------------- policy_checks

CREATE TABLE IF NOT EXISTS policy_checks (
    id             TEXT PRIMARY KEY,
    decision_id    TEXT NOT NULL REFERENCES agent_decisions (id) ON DELETE CASCADE,
    case_id        TEXT NOT NULL REFERENCES risk_cases (id) ON DELETE CASCADE,
    policy_version TEXT NOT NULL,
    rule           TEXT NOT NULL,
    result         TEXT NOT NULL CHECK (result IN ('PASS','BLOCK','ESCALATE')),
    details        TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS policy_checks_case_idx ON policy_checks (case_id, created_at);
CREATE INDEX IF NOT EXISTS policy_checks_decision_idx ON policy_checks (decision_id);
CREATE INDEX IF NOT EXISTS policy_checks_result_idx ON policy_checks (result);

-- ------------------------------------------------------ recovery_actions

CREATE TABLE IF NOT EXISTS recovery_actions (
    id              TEXT PRIMARY KEY,
    case_id         TEXT NOT NULL REFERENCES risk_cases (id) ON DELETE CASCADE,
    decision_id     TEXT REFERENCES agent_decisions (id) ON DELETE SET NULL,
    action_type     TEXT NOT NULL CHECK (action_type IN ('retry','payment_link','reminder','escalate','no_action')),
    idempotency_key TEXT NOT NULL,
    external_id     TEXT NOT NULL DEFAULT '',
    external_url    TEXT NOT NULL DEFAULT '',
    amount          BIGINT NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending','executed','failed','skipped','ambiguous')),
    error_code      TEXT NOT NULL DEFAULT '',
    error_message   TEXT NOT NULL DEFAULT '',
    attempt_count   INTEGER NOT NULL DEFAULT 0,
    mode            TEXT NOT NULL DEFAULT 'live_test' CHECK (mode IN ('live_test','simulation','review')),
    environment     TEXT NOT NULL DEFAULT 'test' CHECK (environment = 'test'),
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    executed_at     TIMESTAMPTZ,
    latency_ms      BIGINT NOT NULL DEFAULT 0
);

-- THE idempotency guarantee (SRS FR-043, 20.1, AC-006). A duplicate action
-- request collides here and the executor returns the existing row instead of
-- performing a second side effect.
CREATE UNIQUE INDEX IF NOT EXISTS recovery_actions_idempotency_uidx
    ON recovery_actions (idempotency_key);
CREATE INDEX IF NOT EXISTS recovery_actions_case_idx ON recovery_actions (case_id, requested_at DESC);
-- The approval queue asks "has this decision already been acted on?" for every
-- pending row (SRS 16.3), which is a lookup by decision_id.
CREATE INDEX IF NOT EXISTS recovery_actions_decision_idx
    ON recovery_actions (decision_id) WHERE decision_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS recovery_actions_external_idx
    ON recovery_actions (external_id) WHERE external_id <> '';
CREATE INDEX IF NOT EXISTS recovery_actions_status_idx ON recovery_actions (status, requested_at DESC);

-- --------------------------------------------------------------- events

CREATE TABLE IF NOT EXISTS events (
    id                TEXT PRIMARY KEY,
    source            TEXT NOT NULL,
    external_event_id TEXT NOT NULL,
    event_type        TEXT NOT NULL,
    payload_json      JSONB NOT NULL,
    signature_valid   BOOLEAN NOT NULL DEFAULT FALSE,
    rejection_reason  TEXT NOT NULL DEFAULT '',
    entity_id         TEXT NOT NULL DEFAULT '',
    entity_timestamp  TIMESTAMPTZ,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at      TIMESTAMPTZ,
    environment       TEXT NOT NULL DEFAULT 'test' CHECK (environment = 'test')
);

-- Event deduplication (SRS FR-003, AC-006).
CREATE UNIQUE INDEX IF NOT EXISTS events_external_id_uidx ON events (external_event_id);
CREATE INDEX IF NOT EXISTS events_type_received_idx ON events (event_type, received_at DESC);
CREATE INDEX IF NOT EXISTS events_entity_idx ON events (entity_id) WHERE entity_id <> '';
CREATE INDEX IF NOT EXISTS events_unprocessed_idx ON events (received_at) WHERE processed_at IS NULL;

-- ------------------------------------------------------------ approvals

CREATE TABLE IF NOT EXISTS approvals (
    id            TEXT PRIMARY KEY,
    case_id       TEXT NOT NULL REFERENCES risk_cases (id) ON DELETE CASCADE,
    decision_id   TEXT NOT NULL REFERENCES agent_decisions (id) ON DELETE CASCADE,
    reason        TEXT NOT NULL DEFAULT '',
    requested_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewer      TEXT NOT NULL DEFAULT '',
    decision      TEXT NOT NULL DEFAULT 'pending' CHECK (decision IN ('pending','approved','rejected')),
    decision_note TEXT NOT NULL DEFAULT '',
    decided_at    TIMESTAMPTZ
);

-- One pending approval per decision keeps the queue unambiguous.
CREATE UNIQUE INDEX IF NOT EXISTS approvals_pending_decision_uidx
    ON approvals (decision_id) WHERE decision = 'pending';
CREATE INDEX IF NOT EXISTS approvals_queue_idx ON approvals (decision, requested_at);
CREATE INDEX IF NOT EXISTS approvals_case_idx ON approvals (case_id);

-- ------------------------------------------------------------- outcomes

CREATE TABLE IF NOT EXISTS outcomes (
    id                       TEXT PRIMARY KEY,
    case_id                  TEXT NOT NULL REFERENCES risk_cases (id) ON DELETE CASCADE,
    action_id                TEXT REFERENCES recovery_actions (id) ON DELETE SET NULL,
    outcome                  TEXT NOT NULL CHECK (outcome IN
                               ('recovered','not_recovered','pending','stopped','escalated')),
    recovered_amount         BIGINT NOT NULL DEFAULT 0,
    recovered_at             TIMESTAMPTZ,
    time_to_recovery_seconds BIGINT NOT NULL DEFAULT 0,
    verification_source      TEXT NOT NULL DEFAULT '',
    notes                    TEXT NOT NULL DEFAULT '',
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A given action yields at most one recovered outcome: this is the second line
-- of defence against double-counting recovered revenue.
CREATE UNIQUE INDEX IF NOT EXISTS outcomes_recovered_action_uidx
    ON outcomes (action_id) WHERE action_id IS NOT NULL AND outcome = 'recovered';
CREATE INDEX IF NOT EXISTS outcomes_case_idx ON outcomes (case_id, created_at DESC);
CREATE INDEX IF NOT EXISTS outcomes_type_idx ON outcomes (outcome);

-- ----------------------------------------------------------- audit_logs

CREATE TABLE IF NOT EXISTS audit_logs (
    id           BIGSERIAL PRIMARY KEY,
    actor        TEXT NOT NULL,
    entity_type  TEXT NOT NULL,
    entity_id    TEXT NOT NULL,
    case_id      TEXT,
    action_id    TEXT,
    event_type   TEXT NOT NULL,
    payload_json JSONB,
    timestamp    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS audit_logs_case_idx ON audit_logs (case_id, timestamp);
CREATE INDEX IF NOT EXISTS audit_logs_entity_idx ON audit_logs (entity_type, entity_id, timestamp);
CREATE INDEX IF NOT EXISTS audit_logs_event_idx ON audit_logs (event_type, timestamp DESC);

-- ------------------------------------------------------ strategy_metrics

CREATE TABLE IF NOT EXISTS strategy_metrics (
    id               TEXT PRIMARY KEY,
    segment          TEXT NOT NULL,
    source_type      TEXT NOT NULL,
    action_type      TEXT NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    successes        INTEGER NOT NULL DEFAULT 0,
    recovered_amount BIGINT NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT strategy_metrics_unique UNIQUE (segment, source_type, action_type)
);

-- ------------------------------------------------------------- policies

CREATE TABLE IF NOT EXISTS policies (
    version                          TEXT PRIMARY KEY,
    max_retry_count                  INTEGER NOT NULL,
    max_automated_amount             BIGINT NOT NULL,
    min_action_confidence            DOUBLE PRECISION NOT NULL,
    cooldown_minutes                 INTEGER NOT NULL,
    max_actions_per_customer_per_day INTEGER NOT NULL,
    require_human_approval_above     BIGINT NOT NULL,
    max_reminders_per_case           INTEGER NOT NULL DEFAULT 2,
    max_actions_per_case             INTEGER NOT NULL DEFAULT 3,
    is_active                        BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at                       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by                       TEXT NOT NULL DEFAULT ''
);

-- Exactly one policy version may be active at a time.
CREATE UNIQUE INDEX IF NOT EXISTS policies_single_active_uidx ON policies (is_active) WHERE is_active;

-- --------------------------------------------- benchmark + simulation

CREATE TABLE IF NOT EXISTS benchmark_datasets (
    id         TEXT PRIMARY KEY,
    version    TEXT NOT NULL,
    seed       BIGINT NOT NULL,
    size       INTEGER NOT NULL,
    mix        JSONB NOT NULL DEFAULT '{}'::jsonb,
    cases_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT benchmark_datasets_version_seed UNIQUE (version, seed, size)
);

CREATE TABLE IF NOT EXISTS simulation_runs (
    id              TEXT PRIMARY KEY,
    dataset_id      TEXT NOT NULL REFERENCES benchmark_datasets (id) ON DELETE CASCADE,
    dataset_version TEXT NOT NULL,
    seed            BIGINT NOT NULL,
    policy_version  TEXT NOT NULL,
    strategy        TEXT NOT NULL,
    baseline        TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'running',
    result_json     JSONB,
    baseline_json   JSONB,
    evaluation_json JSONB,
    uplift_percent  DOUBLE PRECISION,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    duration_ms     BIGINT NOT NULL DEFAULT 0,
    created_by      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS simulation_runs_started_idx ON simulation_runs (started_at DESC);

-- ------------------------------------------------------------- metrics

-- Lightweight operational counters (SRS 18.3). Kept as a key/value table so a
-- new counter does not require a migration.
CREATE TABLE IF NOT EXISTS operational_counters (
    name       TEXT PRIMARY KEY,
    value      BIGINT NOT NULL DEFAULT 0,
    sum_value  BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
