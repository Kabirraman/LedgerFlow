'use client';

/**
 * Approve / reject controls (SRS 16.3).
 *
 * Two rules from the SRS are enforced here rather than left to each screen:
 *
 *   - Approve is one click. A confirmation dialog on the approve path would slow the
 *     common case without preventing a mistake, since the approval is itself
 *     recorded and reversible only by the audit trail either way.
 *   - Reject requires a reason. The API refuses an empty note with a 400, and this
 *     form refuses to submit one — a rejection with no reason is not a reviewable
 *     record, and the reviewer is the only person who knows why.
 *
 * The optimistic path is deliberately absent. Approving can trigger immediate
 * execution against Razorpay, so the button stays pending until the server says what
 * happened, and `execution_error` is surfaced verbatim when the approval succeeded but
 * the action that followed did not. Those are two different outcomes and collapsing
 * them would hide a failure.
 */

import { useState } from 'react';

import { Button, ErrorBanner } from '@/components/ui';
import { api } from '@/lib/api';
import { useMutation } from '@/lib/hooks';
import type { DecisionResponse } from '@/lib/types';

export function ApprovalActions({
  caseId,
  onDecided,
  compact = false,
}: {
  caseId: string;
  /** Called after a decision lands, so the caller can refetch. */
  onDecided?: (result: DecisionResponse) => void;
  compact?: boolean;
}) {
  const [rejecting, setRejecting] = useState(false);
  const [note, setNote] = useState('');
  const [outcome, setOutcome] = useState<DecisionResponse | undefined>(undefined);

  const approve = useMutation((id: string, n: string) => api.approve(id, n));
  const reject = useMutation((id: string, n: string) => api.reject(id, n));

  const pending = approve.pending || reject.pending;
  const error = approve.error ?? reject.error;

  const settle = (result: DecisionResponse | undefined) => {
    if (!result) return;
    setOutcome(result);
    setRejecting(false);
    setNote('');
    onDecided?.(result);
  };

  if (outcome) {
    return (
      <div className="space-y-1">
        <p className="text-xs text-muted">
          Recorded as <span className="font-medium text-body">{outcome.approval.decision}</span>.
          Case is now <span className="font-mono text-body">{outcome.case_status}</span>.
        </p>
        {outcome.execution_error ? (
          <p className="text-2xs leading-relaxed text-block">
            The approval was recorded, but the action that followed failed:{' '}
            {outcome.execution_error}
          </p>
        ) : null}
      </div>
    );
  }

  return (
    <div className={compact ? 'space-y-2' : 'space-y-3'}>
      <ErrorBanner error={error} />

      {!rejecting ? (
        <div className="flex flex-wrap items-center gap-2">
          <Button
            variant="approve"
            pending={approve.pending}
            disabled={pending}
            onClick={() => void approve.run(caseId, '').then(settle)}
            title="Approve the planned action. If auto-execute is on, it runs immediately."
          >
            Approve
          </Button>
          <Button
            variant="reject"
            disabled={pending}
            onClick={() => setRejecting(true)}
            title="Reject the planned action. A reason is required."
          >
            Reject
          </Button>
        </div>
      ) : (
        <div className="space-y-2">
          <label className="label block" htmlFor={`reject-note-${caseId}`}>
            Reason for rejection (required)
          </label>
          <textarea
            id={`reject-note-${caseId}`}
            className="field min-h-[4.5rem] resize-y"
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="Why should this action not run? This is stored on the approval record."
            autoFocus
          />
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="reject"
              pending={reject.pending}
              disabled={pending || note.trim().length === 0}
              onClick={() => void reject.run(caseId, note.trim()).then(settle)}
            >
              Confirm rejection
            </Button>
            <Button
              onClick={() => {
                setRejecting(false);
                setNote('');
                reject.reset();
              }}
              disabled={pending}
            >
              Cancel
            </Button>
          </div>
          {note.trim().length === 0 ? (
            <p className="text-2xs text-dim">
              The API rejects an empty reason. This field is the audit record for the decision.
            </p>
          ) : null}
        </div>
      )}
    </div>
  );
}
