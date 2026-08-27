'use client';

/**
 * Status badges.
 *
 * The colour mapping lives here and only here. `pass` / `escalate` / `block` mean the
 * same thing on every screen because every screen reads them from this file — a
 * reviewer scanning an approval queue is reading colour before text, so the same
 * colour meaning two things in two places is a correctness bug.
 *
 * Every mapping falls back to a neutral tone rather than to nothing. If the Go side
 * adds an enum value before this file catches up, the badge renders in grey with the
 * raw value in it, which is legible and honest; a lookup miss that rendered empty
 * would look like missing data.
 */

import { cn, humanize, humanizeStatus } from '@/lib/format';
import type {
  ActionStatus,
  ActionType,
  ApprovalDecision,
  CaseStatus,
  OutcomeType,
  PolicyResult,
  Role,
  RunMode,
  Segment,
  SourceType,
  Urgency,
} from '@/lib/types';

type Tone = 'neutral' | 'accent' | 'pass' | 'escalate' | 'block' | 'recovered';

const TONE_CLASS: Record<Tone, string> = {
  neutral: 'border-line-strong bg-ink-700 text-muted',
  accent: 'border-accent/30 bg-accent-soft text-accent-text',
  pass: 'border-pass/30 bg-pass-soft text-pass',
  escalate: 'border-escalate/30 bg-escalate-soft text-escalate',
  block: 'border-block/30 bg-block-soft text-block',
  recovered: 'border-recovered/30 bg-recovered-soft text-recovered',
};

function Badge({
  tone,
  children,
  title,
  className,
}: {
  tone: Tone;
  children: React.ReactNode;
  title?: string;
  className?: string;
}) {
  return (
    <span className={cn('chip', TONE_CLASS[tone], className)} title={title}>
      {children}
    </span>
  );
}

// --- case status ---

/**
 * Grouped by what the reader should do about it, not by position in the state
 * machine: blue means the pipeline is working on it, amber means it is waiting on a
 * person, red means it stopped, green means money came back.
 */
const STATUS_TONE: Record<CaseStatus, Tone> = {
  NEW: 'neutral',
  ANALYZING: 'accent',
  DIAGNOSED: 'accent',
  PLANNED: 'accent',
  POLICY_REVIEW: 'accent',
  ESCALATED: 'escalate',
  WAITING_HUMAN: 'escalate',
  APPROVED: 'accent',
  REJECTED: 'block',
  EXECUTING: 'accent',
  VERIFYING: 'accent',
  RECOVERED: 'recovered',
  FAILED: 'block',
  RETRYING: 'escalate',
  BLOCKED: 'block',
  CLOSED: 'neutral',
};

const STATUS_HINT: Partial<Record<CaseStatus, string>> = {
  WAITING_HUMAN: 'A reviewer must approve or reject before anything executes.',
  ESCALATED: 'The policy engine or an agent declined to act autonomously.',
  BLOCKED: 'A policy rule blocked this outright. Human approval cannot override a block.',
  RECOVERED: 'A verified payment closed this case.',
  VERIFYING: 'An action executed; recovery is not banked until verification confirms payment.',
  RETRYING: 'A previous attempt failed and the case re-entered the pipeline.',
};

export function StatusBadge({ status, className }: { status: CaseStatus; className?: string }) {
  return (
    <Badge tone={STATUS_TONE[status] ?? 'neutral'} title={STATUS_HINT[status]} className={className}>
      {humanizeStatus(status)}
    </Badge>
  );
}

// --- policy result ---

const POLICY_TONE: Record<PolicyResult, Tone> = {
  PASS: 'pass',
  BLOCK: 'block',
  ESCALATE: 'escalate',
};

const POLICY_HINT: Record<PolicyResult, string> = {
  PASS: 'This rule permitted the action.',
  BLOCK: 'This rule refused the action. A block is final — approval cannot override it.',
  ESCALATE: 'This rule required human approval before the action could proceed.',
};

export function PolicyResultBadge({
  result,
  className,
}: {
  result: PolicyResult;
  className?: string;
}) {
  return (
    <Badge tone={POLICY_TONE[result] ?? 'neutral'} title={POLICY_HINT[result]} className={className}>
      {result}
    </Badge>
  );
}

// --- urgency ---

const URGENCY_TONE: Record<Urgency, Tone> = {
  low: 'neutral',
  medium: 'accent',
  high: 'escalate',
  critical: 'block',
};

export function UrgencyBadge({ urgency, className }: { urgency: Urgency; className?: string }) {
  return (
    <Badge tone={URGENCY_TONE[urgency] ?? 'neutral'} className={className}>
      {humanize(urgency)}
    </Badge>
  );
}

// --- action status ---

const ACTION_STATUS_TONE: Record<ActionStatus, Tone> = {
  pending: 'neutral',
  executed: 'pass',
  failed: 'block',
  skipped: 'neutral',
  ambiguous: 'escalate',
};

const ACTION_STATUS_HINT: Partial<Record<ActionStatus, string>> = {
  pending: 'The action row was reserved before any external call. Nothing has executed yet.',
  ambiguous:
    'The external call did not return a clear result. The reconciler resolves these against Razorpay rather than guessing.',
  executed: 'The external call succeeded. Recovery is only banked once verification confirms payment.',
};

export function ActionStatusBadge({
  status,
  className,
}: {
  status: ActionStatus;
  className?: string;
}) {
  return (
    <Badge
      tone={ACTION_STATUS_TONE[status] ?? 'neutral'}
      title={ACTION_STATUS_HINT[status]}
      className={className}
    >
      {humanize(status)}
    </Badge>
  );
}

// --- outcome ---

const OUTCOME_TONE: Record<OutcomeType, Tone> = {
  recovered: 'recovered',
  not_recovered: 'block',
  pending: 'neutral',
  stopped: 'escalate',
  escalated: 'escalate',
};

export function OutcomeBadge({ outcome, className }: { outcome: OutcomeType; className?: string }) {
  return (
    <Badge tone={OUTCOME_TONE[outcome] ?? 'neutral'} className={className}>
      {humanize(outcome)}
    </Badge>
  );
}

// --- approval decision ---

const DECISION_TONE: Record<ApprovalDecision, Tone> = {
  pending: 'escalate',
  approved: 'pass',
  rejected: 'block',
};

export function DecisionBadge({
  decision,
  className,
}: {
  decision: ApprovalDecision;
  className?: string;
}) {
  return (
    <Badge tone={DECISION_TONE[decision] ?? 'neutral'} className={className}>
      {humanize(decision)}
    </Badge>
  );
}

// --- action type ---

const ACTION_TYPE_HINT: Record<ActionType, string> = {
  retry: 'Re-attempt the original charge. Bounded by the policy retry limit.',
  payment_link: 'Create a Razorpay payment link for the exact trusted amount and send it.',
  reminder: 'Send a reminder without creating a new payment instrument.',
  escalate: 'Hand the case to a human. No external call is made.',
  no_action: 'Deliberately do nothing. Recorded as a decision, not as a gap.',
};

export function ActionTypeBadge({
  action,
  className,
}: {
  action: ActionType;
  className?: string;
}) {
  const tone: Tone =
    action === 'escalate' ? 'escalate' : action === 'no_action' ? 'neutral' : 'accent';
  return (
    <Badge tone={tone} title={ACTION_TYPE_HINT[action]} className={className}>
      {humanize(action)}
    </Badge>
  );
}

// --- source type, segment, mode, role ---

export function SourceBadge({ source, className }: { source: SourceType; className?: string }) {
  return (
    <Badge tone="neutral" className={className}>
      {humanize(source)}
    </Badge>
  );
}

export function SegmentBadge({ segment, className }: { segment: Segment; className?: string }) {
  const tone: Tone = segment === 'high_value' ? 'accent' : 'neutral';
  return (
    <Badge tone={tone} className={className}>
      {humanize(segment)}
    </Badge>
  );
}

const MODE_HINT: Record<RunMode, string> = {
  live_test:
    'Razorpay test mode. Real API calls against test credentials; no live money moves (SRS 5.2).',
  simulation: 'Synthetic benchmark. No external API call is possible from this path (AC-009).',
  review: 'Held for review. No action executes from this state.',
};

export function ModeBadge({ mode, className }: { mode: RunMode; className?: string }) {
  const tone: Tone = mode === 'simulation' ? 'escalate' : 'neutral';
  return (
    <Badge tone={tone} title={MODE_HINT[mode]} className={className}>
      {humanize(mode)}
    </Badge>
  );
}

export function RoleBadge({ role, className }: { role: Role; className?: string }) {
  const tone: Tone = role === 'admin' ? 'accent' : 'neutral';
  return (
    <Badge tone={tone} className={className}>
      {humanize(role)}
    </Badge>
  );
}

/**
 * The "who decided this" marker.
 *
 * A deterministic fallback and a model answer must never look the same on screen.
 * SRS 20.4 requires the system to fall back to a safe state when an agent fails, and
 * a reviewer needs to be able to see that that is what happened.
 */
export function DecidedByBadge({ source, model }: { source?: string; model?: string }) {
  if (!source) return null;
  const isModel = source === 'model';
  return (
    <Badge
      tone={isModel ? 'accent' : 'neutral'}
      title={
        isModel
          ? `Produced by ${model || 'the model'} under a fixed JSON schema, then validated.`
          : 'Produced by the deterministic fallback — the model was unavailable, timed out, or returned output that failed validation.'
      }
    >
      {isModel ? `Model${model ? ` · ${model}` : ''}` : 'Deterministic fallback'}
    </Badge>
  );
}
