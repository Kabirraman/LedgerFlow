/**
 * TypeScript mirror of the Go API's JSON contract.
 *
 * Every type here corresponds to a struct in internal/domain or a handler
 * response in internal/httpapi. The field names are the Go json tags, not the Go
 * field names, and optional fields are the ones carrying `omitempty`.
 *
 * These are hand-written rather than generated. A generator would be better, but
 * the honest tradeoff for a prototype is that a hand-written mirror can drift —
 * so the API contract tests in internal/httpapi/api_test.go assert the shapes
 * that this file depends on most (the error envelope, the policy limits object,
 * the flat success bodies), which is what catches drift before a screen does.
 *
 * MONEY IS ALWAYS PAISE. Every field typed `Money` below is an integer number of
 * paise, exactly as domain.Money serialises. Never do display arithmetic on it
 * without going through formatMoney/formatRupees in ./format — dividing by 100 in
 * one component and not another is how a dashboard ends up showing two different
 * totals for the same money.
 */

/** An integer number of paise. ₹1,000.00 is 100000. */
export type Money = number;

/** RFC3339 timestamp string, as Go's time.Time marshals. */
export type Timestamp = string;

// --- enumerations (internal/domain/enums.go) ---

export const SOURCE_TYPES = [
  'payment_failure',
  'checkout_abandonment',
  'invoice_overdue',
  'subscription_failure',
] as const;
export type SourceType = (typeof SOURCE_TYPES)[number];

export const SEGMENTS = ['new', 'repeat', 'high_value', 'b2b', 'subscription'] as const;
export type Segment = (typeof SEGMENTS)[number];

export const ACTION_TYPES = ['retry', 'payment_link', 'reminder', 'escalate', 'no_action'] as const;
export type ActionType = (typeof ACTION_TYPES)[number];

export const ROOT_CAUSES = [
  'transient_failure',
  'insufficient_funds',
  'checkout_abandonment',
  'overdue_receivable',
  'subscription_failure',
  'authentication_failed',
  'unknown',
] as const;
export type RootCause = (typeof ROOT_CAUSES)[number];

export const CASE_STATUSES = [
  'NEW',
  'ANALYZING',
  'DIAGNOSED',
  'PLANNED',
  'POLICY_REVIEW',
  'ESCALATED',
  'WAITING_HUMAN',
  'APPROVED',
  'REJECTED',
  'EXECUTING',
  'VERIFYING',
  'RECOVERED',
  'FAILED',
  'RETRYING',
  'BLOCKED',
  'CLOSED',
] as const;
export type CaseStatus = (typeof CASE_STATUSES)[number];

export const STRATEGIES = [
  'retry_everything',
  'reminder_everything',
  'static_heuristic',
  'ledgerflow',
] as const;
export type StrategyName = (typeof STRATEGIES)[number];

export type PolicyResult = 'PASS' | 'BLOCK' | 'ESCALATE';
export type Urgency = 'low' | 'medium' | 'high' | 'critical';
export type ActionStatus = 'pending' | 'executed' | 'failed' | 'skipped' | 'ambiguous';
export type OutcomeType = 'recovered' | 'not_recovered' | 'pending' | 'stopped' | 'escalated';
export type ApprovalDecision = 'pending' | 'approved' | 'rejected';
export type RunMode = 'live_test' | 'simulation' | 'review';
export type Environment = 'test' | 'live';
export type Role = 'operator' | 'reviewer' | 'admin';

/**
 * Role ordering, mirroring domain.Role.Permits: admin implies reviewer implies
 * operator. The server enforces this on every route; this copy exists only so the
 * navigation does not offer a link that will 403.
 */
const ROLE_RANK: Record<Role, number> = { operator: 1, reviewer: 2, admin: 3 };

export function permits(held: Role | undefined, required: Role): boolean {
  if (!held) return false;
  return (ROLE_RANK[held] ?? 0) >= ROLE_RANK[required];
}

// --- entities (internal/domain/entities.go) ---

export interface Customer {
  id: string;
  razorpay_customer_id?: string;
  name: string;
  email?: string;
  contact?: string;
  segment: Segment;
  lifetime_value: Money;
  success_rate: number;
  total_payments: number;
  environment: Environment;
  created_at: Timestamp;
}

export interface Transaction {
  id: string;
  razorpay_payment_id?: string;
  razorpay_order_id?: string;
  customer_id: string;
  amount: Money;
  currency: string;
  status: string;
  method?: string;
  failure_reason?: string;
  error_code?: string;
  attempt_count: number;
  environment: Environment;
  created_at: Timestamp;
}

export interface CheckoutSession {
  id: string;
  customer_id: string;
  cart_amount: Money;
  item_count: number;
  page_views: number;
  started_at: Timestamp;
  last_activity_at: Timestamp;
  status: 'active' | 'abandoned' | 'converted';
  environment: Environment;
}

export interface Invoice {
  id: string;
  razorpay_invoice_id?: string;
  customer_id: string;
  invoice_number: string;
  amount: Money;
  amount_paid: Money;
  status: string;
  due_date: Timestamp;
  reminder_count: number;
  environment: Environment;
  created_at: Timestamp;
}

export interface Subscription {
  id: string;
  razorpay_subscription_id?: string;
  customer_id: string;
  plan_id?: string;
  amount: Money;
  status: string;
  failed_charge_count: number;
  current_end: Timestamp;
  environment: Environment;
  created_at: Timestamp;
}

export interface RiskCase {
  id: string;
  reference: string;
  source_type: SourceType;
  customer_id: string;
  transaction_id?: string;
  checkout_session_id?: string;
  invoice_id?: string;
  subscription_id?: string;
  revenue_at_risk: Money;
  risk_score: number;
  urgency: Urgency;
  expected_recovery: Money;
  recovered_amount: Money;
  status: CaseStatus;
  reason_codes: string[] | null;
  evidence_refs: string[] | null;
  stop_reason?: string;
  action_count: number;
  mode: RunMode;
  simulation_id?: string;
  environment: Environment;
  created_at: Timestamp;
  updated_at: Timestamp;
  closed_at?: Timestamp;
}

export interface Diagnosis {
  id: string;
  case_id: string;
  root_cause: RootCause;
  confidence: number;
  evidence: string[] | null;
  uncertainty_flags: string[] | null;
  next_step: string;
  /** "model" or "deterministic" — a fallback diagnosis must be legible as one. */
  source: string;
  model_name?: string;
  latency_ms: number;
  created_at: Timestamp;
}

export interface AgentDecision {
  id: string;
  case_id: string;
  recommended_action: ActionType;
  recovery_probability: number;
  expected_recovery: Money;
  reason_codes: string[] | null;
  alternatives: string[] | null;
  stop_condition: string;
  policy_version: string;
  source: string;
  model_name?: string;
  latency_ms: number;
  created_at: Timestamp;
}

export interface PolicyCheck {
  id: string;
  decision_id: string;
  case_id: string;
  policy_version: string;
  rule: string;
  result: PolicyResult;
  details: string;
  created_at: Timestamp;
}

export interface RecoveryAction {
  id: string;
  case_id: string;
  decision_id: string;
  action_type: ActionType;
  idempotency_key: string;
  external_id?: string;
  external_url?: string;
  amount: Money;
  status: ActionStatus;
  error_code?: string;
  error_message?: string;
  attempt_count: number;
  mode: RunMode;
  environment: Environment;
  requested_at: Timestamp;
  executed_at?: Timestamp;
  latency_ms: number;
}

export interface IngestedEvent {
  id: string;
  source: string;
  external_event_id: string;
  event_type: string;
  payload_json: unknown;
  signature_valid: boolean;
  rejection_reason?: string;
  entity_id?: string;
  entity_timestamp?: Timestamp;
  received_at: Timestamp;
  processed_at?: Timestamp;
  environment: Environment;
}

export interface Approval {
  id: string;
  case_id: string;
  decision_id: string;
  reason: string;
  requested_at: Timestamp;
  reviewer?: string;
  decision: ApprovalDecision;
  decision_note?: string;
  decided_at?: Timestamp;
}

export interface Outcome {
  id: string;
  case_id: string;
  action_id?: string;
  outcome: OutcomeType;
  recovered_amount: Money;
  recovered_at?: Timestamp;
  time_to_recovery_seconds: number;
  verification_source: string;
  notes?: string;
  created_at: Timestamp;
}

export interface AuditLog {
  id: string;
  actor: string;
  entity_type: string;
  entity_id: string;
  case_id?: string;
  action_id?: string;
  event_type: string;
  payload_json?: unknown;
  timestamp: Timestamp;
}

export interface Policy {
  version: string;
  max_retry_count: number;
  max_automated_amount: Money;
  min_action_confidence: number;
  cooldown_minutes: number;
  max_actions_per_customer_per_day: number;
  require_human_approval_above: Money;
  max_reminders_per_case: number;
  max_actions_per_case: number;
  updated_at: Timestamp;
  updated_by?: string;
}

/** The policy fields an admin may edit, as accepted by PUT /api/policies. */
export type PolicyField =
  | 'max_retry_count'
  | 'max_automated_amount'
  | 'min_action_confidence'
  | 'cooldown_minutes'
  | 'max_actions_per_customer_per_day'
  | 'require_human_approval_above'
  | 'max_reminders_per_case'
  | 'max_actions_per_case';

/**
 * One published bound from store.PolicyLimits().
 *
 * `max` is null for a genuinely unbounded field. Rendering a sentinel number
 * there would put a ceiling on the admin form that the server does not enforce,
 * which is a lie in the direction of looking safer than it is.
 */
export interface PolicyBound {
  min: number;
  max: number | null;
}

// --- views (internal/domain/views.go) ---

export interface RecoveryFunnel {
  identified: number;
  diagnosed: number;
  actioned: number;
  recovered: number;
}

export interface SourceBreakdown {
  source_type: SourceType;
  cases: number;
  revenue_at_risk: Money;
  recovered: Money;
  recovery_rate: number;
}

export interface ActivityItem {
  case_id: string;
  reference: string;
  kind: string;
  action_type?: ActionType;
  amount: Money;
  detail: string;
  at: Timestamp;
}

export interface OperationalMetrics {
  webhooks_received: number;
  webhook_signature_failures: number;
  duplicate_events: number;
  duplicate_event_rate: number;
  action_api_failures: number;
  avg_action_latency_ms: number;
  avg_agent_latency_ms: number;
  agent_fallback_count: number;
}

export interface DashboardSummary {
  revenue_at_risk: Money;
  expected_recovery: Money;
  recovered_amount: Money;
  recovery_rate: number;
  automated_actions: number;
  escalated_cases: number;
  blocked_actions: number;
  open_cases: number;
  unresolved_revenue: Money;
  avg_recovery_minutes: number;
  recovered_per_intervention: Money;
  escalation_rate: number;
  policy_violations: number;
  funnel: RecoveryFunnel;
  by_source: SourceBreakdown[] | null;
  activity: ActivityItem[] | null;
  operational: OperationalMetrics;
}

export interface TimelineItem {
  at: Timestamp;
  kind: string;
  title: string;
  detail?: string;
  result?: string;
  actor?: string;
}

export interface CaseDetail {
  case: RiskCase;
  customer?: Customer;
  transaction?: Transaction;
  checkout_session?: CheckoutSession;
  invoice?: Invoice;
  subscription?: Subscription;
  diagnosis?: Diagnosis;
  decision?: AgentDecision;
  policy_checks: PolicyCheck[] | null;
  actions: RecoveryAction[] | null;
  approvals: Approval[] | null;
  outcomes: Outcome[] | null;
  timeline: TimelineItem[] | null;
}

export interface CaseListItem extends RiskCase {
  customer_name: string;
  customer_segment: Segment;
  root_cause?: RootCause;
  confidence?: number;
  recommended_action?: ActionType;
  policy_result?: PolicyResult;
}

export interface CasePage {
  items: CaseListItem[] | null;
  total: number;
  limit: number;
  offset: number;
}

export interface ApprovalQueueItem extends Approval {
  reference: string;
  source_type: SourceType;
  customer_name: string;
  customer_segment: Segment;
  case_status: CaseStatus;
  revenue_at_risk: Money;
  expected_recovery: Money;
  risk_score: number;
  urgency: Urgency;
  recommended_action: ActionType;
  confidence: number;
  reason_codes: string[] | null;
  already_executed: boolean;
  waiting_minutes: number;
}

export interface StrategyMetric {
  segment: Segment;
  source_type: SourceType;
  action_type: ActionType;
  attempts: number;
  successes: number;
  recovered_amount: Money;
  /** null when there is no sample. A never-tried strategy and an always-failing one must not look alike. */
  success_rate: number | null;
  sufficient: boolean;
}

export interface SimulationResult {
  strategy: StrategyName;
  cases_processed: number;
  revenue_at_risk: Money;
  eligible_opportunities: number;
  eligible_amount: Money;
  actions_executed: number;
  recovered_amount: Money;
  recovered_count: number;
  recovery_rate: number;
  escalated: number;
  stopped_safely: number;
  blocked: number;
  policy_violations: number;
  errors: number;
  avg_time_to_recovery_min: number;
  action_breakdown: Record<string, number> | null;
}

export interface AgentEvaluation {
  detection_precision: number;
  detection_recall: number;
  detection_f1: number;
  diagnosis_accuracy: number;
  intervention_accuracy: number;
  interventions_graded: number;
  interventions_deferred: number;
  calibration_error: number;
  calibration_samples: number;
  evidence_coverage: number;
  model_calls: number;
  schema_valid_rate: number;
  unauthorized_actions: number;
}

export interface SimulationRun {
  id: string;
  dataset_id: string;
  dataset_version: string;
  seed: number;
  policy_version: string;
  strategy: StrategyName;
  baseline: StrategyName;
  status: string;
  result: SimulationResult;
  baseline_result?: SimulationResult;
  /** Signed. AC-008 requires that a negative uplift can be shown. */
  uplift_percent?: number;
  agent_evaluation?: AgentEvaluation;
  started_at: Timestamp;
  finished_at?: Timestamp;
  duration_ms: number;
  created_by?: string;
}

export interface BenchmarkDataset {
  id: string;
  version: string;
  seed: number;
  size: number;
  mix: Record<string, number> | null;
  created_at: Timestamp;
}

// --- the AC-010 explanation block (internal/httpapi/explain.go) ---

export interface ExplainItem {
  code: string;
  reading: string;
}

export interface ExplainControl {
  rule: string;
  reading: string;
  result: PolicyResult;
  details?: string;
}

/**
 * The "why this action" block.
 *
 * Assembled server-side from persisted reason codes, policy checks and trusted
 * amounts. It contains no model prose and no private chain-of-thought, which is
 * the constraint AC-010 actually imposes: explain the decision, do not expose the
 * reasoning trace.
 */
export interface Explanation {
  headline: string;
  because: ExplainItem[] | null;
  evidence: string[] | null;
  considered?: string[] | null;
  controls: ExplainControl[] | null;
  stop_condition?: string;
  /** "model" | "deterministic" */
  decided_by?: string;
  model_name?: string;
  uncertainty?: string[] | null;
  confidence: number;
  expected_recovery: Money;
}

// --- handler response envelopes (internal/httpapi) ---

export interface SessionUser {
  user_id: string;
  email: string;
  name?: string;
  role: Role;
}

export interface DashboardResponse {
  summary: DashboardSummary;
  data_label: string;
  as_of: Timestamp;
}

export interface CasesResponse {
  page: CasePage;
  data_label: string;
}

export interface CaseDetailResponse {
  case: CaseDetail;
  explanation: Explanation;
  allowed_transitions: CaseStatus[] | null;
  data_label: string;
}

export interface ReanalyzeResponse {
  progress?: unknown;
  restarted: boolean;
}

export interface VerifyResponse {
  action_id: string;
  report?: unknown;
}

export interface DecisionResponse {
  approval: Approval;
  case_status: CaseStatus;
  progress?: unknown;
  execution_error?: string;
}

export interface ApprovalsResponse {
  approvals: ApprovalQueueItem[] | null;
  limit: number;
  /** Rows in this response — capped by `limit`. */
  returned: number;
  /** True when requests whose action already ran are being withheld (the default). */
  hiding_executed: boolean;
  /** The real backlog, counted in the database and unaffected by `limit`. */
  total_pending: number;
}

export interface AuditResponse {
  case_id: string;
  audit: AuditLog[] | null;
  count: number;
}

export interface StrategiesResponse {
  strategies: StrategyMetric[] | null;
  totals: {
    attempts: number;
    successes: number;
    recovered_amount: Money;
    success_rate: number;
    min_attempts_signal: number;
  };
  data_label: string;
}

export interface CounterValue {
  count: number;
  sum: number;
  mean: number;
}

export interface OpsMetricsResponse {
  counters: Record<string, CounterValue>;
  as_of: Timestamp;
}

export interface OpsEventsResponse {
  events: IngestedEvent[] | null;
  limit: number;
}

export interface PoliciesResponse {
  active: Policy;
  history: Policy[] | null;
  default: Policy;
  limits: Record<string, PolicyBound>;
}

export interface PolicyUpdateResponse {
  policy: Policy;
  activated: boolean;
}

export interface ReportLine {
  label: string;
  value: string;
}

export interface SimulationResponse {
  run: SimulationRun;
  /** The SRS 17.4 output block, in the order and with the labels the SRS specifies. */
  report: ReportLine[];
  data_label: string;
  reproduce: {
    dataset_version: string;
    seed: number;
    policy_version: string;
    strategy: StrategyName;
    baseline: StrategyName;
  };
  agent_evaluation?: AgentEvaluation;
}

export interface SimulationsListResponse {
  runs: SimulationRun[] | null;
  limit: number;
  data_label: string;
}

export interface DatasetsResponse {
  datasets: BenchmarkDataset[] | null;
  defaults: {
    version: string;
    seed: number;
    size: number;
    strategies: StrategyName[];
  };
  declared_mix: Record<string, number>;
}

export interface VersionResponse {
  version: string;
  environment: string;
  razorpay_mode: string;
  razorpay_configured: boolean;
  gateway: string;
  gateway_external: boolean;
  model_configured: boolean;
  model: string;
  auto_execute: boolean;
  live_mode_supported: boolean;
}

export interface HealthResponse {
  status: 'ok' | 'degraded';
  time: Timestamp;
  env: string;
  mode: string;
  version: string;
  database?: string;
}

export interface CheckoutStartResponse {
  session: CheckoutSession;
  customer: Customer;
}

export interface CheckoutAbandonResponse {
  session_id: string;
  case_id: string;
  case_reference: string;
  case_created: boolean;
  reason: string;
}

export interface SyncReport {
  from?: string;
  to?: string;
  [key: string]: unknown;
}

export interface SyncResponse {
  report: SyncReport;
}
