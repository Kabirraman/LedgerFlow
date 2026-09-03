'use client';

/**
 * Simulation Lab (SRS 16.4, 17).
 *
 * Runs the versioned synthetic benchmark against a chosen strategy and baseline, with
 * no external API call possible from this path (AC-009 — the runner this page calls
 * holds no Razorpay gateway at all, not just a disabled one). Three things this screen
 * is careful about, because SRS 25.2 is about not letting a demo number pass as more
 * than it is:
 *
 *   1. Every run shows its dataset version and seed next to the result, not just in a
 *      tooltip — "reproduce" is a first-class field on the response, not an aside.
 *   2. The report block renders in the exact order and with the exact labels SRS 17.4
 *      specifies, because that block is the artifact a judge or reviewer checks the
 *      SRS against.
 *   3. Uplift is signed and shown even when negative (AC-008). A baseline beating
 *      LEDGERFLOW is a result, not a bug to hide.
 */

import { useState } from 'react';

import {
  Button,
  Card,
  CardHeader,
  DataLabel,
  EmptyState,
  ErrorBanner,
  PageHeader,
  Select,
  SkeletonRows,
  TableShell,
  Td,
  Th,
  TextField,
} from '@/components/ui';
import { api } from '@/lib/api';
import { useAuth } from '@/lib/auth';
import {
  formatCount,
  formatDateTime,
  formatMoneyKPI,
  formatPercent,
  formatSignedPercent,
  humanize,
} from '@/lib/format';
import { useApi, useMutation } from '@/lib/hooks';
import { STRATEGIES } from '@/lib/types';
import type { SimulationResponse, SimulationRun, StrategyName } from '@/lib/types';

const RUNS_POLL_MS = 15_000;

const STRATEGY_OPTIONS = STRATEGIES.map((s) => ({ value: s, label: humanize(s) }));

export default function SimulationsPage() {
  const { can } = useAuth();

  const [strategy, setStrategy] = useState<StrategyName>('ledgerflow');
  const [baseline, setBaseline] = useState<StrategyName>('retry_everything');
  const [version, setVersion] = useState('');
  const [seed, setSeed] = useState('');
  const [size, setSize] = useState('');

  const datasets = useApi('datasets', (signal) => api.datasets(signal));
  const runs = useApi('simulations', (signal) => api.simulations(20, signal), {
    pollMs: RUNS_POLL_MS,
  });

  const runMutation = useMutation(api.runSimulation);
  const [selected, setSelected] = useState<SimulationResponse | undefined>(undefined);
  const selectDetail = useMutation((id: string) => api.simulation(id));

  const defaults = datasets.data?.defaults;

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const body: Parameters<typeof api.runSimulation>[0] = { strategy, baseline, evaluate: true };
    if (version.trim()) body.version = version.trim();
    const seedNum = Number.parseInt(seed, 10);
    if (seed.trim() && Number.isFinite(seedNum)) body.seed = seedNum;
    const sizeNum = Number.parseInt(size, 10);
    if (size.trim() && Number.isFinite(sizeNum)) body.size = sizeNum;

    const result = await runMutation.run(body);
    if (result) {
      setSelected(result);
      runs.reload();
    }
  };

  const openRun = async (run: SimulationRun) => {
    setSelected(undefined);
    const result = await selectDetail.run(run.id);
    if (result) setSelected(result);
  };

  return (
    <>
      <PageHeader
        title="Simulation Lab"
        description="A versioned synthetic benchmark, compared against a simple baseline. Nothing on this page can reach Razorpay — the runner behind it holds no gateway (AC-009)."
        right={!can('operator') ? null : <DataLabel label={runs.data?.data_label} />}
      />

      {!can('operator') ? (
        <Card>
          <EmptyState
            title="Operator role required."
            detail="Running or reviewing the benchmark needs the operator role or above."
          />
        </Card>
      ) : (
        <>
          <Card>
            <CardHeader
              title="Run a benchmark"
              subtitle="Target benchmark size is 200 synthetic cases across five scenario mixes (SRS 17.1)."
            />
            <form onSubmit={submit} className="grid grid-cols-1 gap-3 p-4 sm:grid-cols-2 sm:p-5 lg:grid-cols-5">
              <Select
                label="LEDGERFLOW strategy"
                value={strategy}
                onChange={(v) => setStrategy((v || 'ledgerflow') as StrategyName)}
                options={STRATEGY_OPTIONS}
              />
              <Select
                label="Baseline"
                value={baseline}
                onChange={(v) => setBaseline((v || 'retry_everything') as StrategyName)}
                options={STRATEGY_OPTIONS}
              />
              <TextField
                label="Dataset version"
                value={version}
                onChange={setVersion}
                placeholder={defaults?.version || 'default'}
                hint="Leave blank to use the current default."
              />
              <TextField
                label="Seed"
                type="number"
                value={seed}
                onChange={setSeed}
                placeholder={defaults ? String(defaults.seed) : 'default'}
                hint="Fixed seed makes the run reproducible (NFR-008)."
              />
              <TextField
                label="Case count"
                type="number"
                value={size}
                onChange={setSize}
                placeholder={defaults ? String(defaults.size) : '200'}
              />
              <div className="sm:col-span-2 lg:col-span-5">
                <Button type="submit" variant="primary" pending={runMutation.pending}>
                  Run simulation
                </Button>
              </div>
            </form>
            <ErrorBanner error={runMutation.error} className="mx-4 mb-4 sm:mx-5" />
          </Card>

          {selected ? <RunDetail response={selected} /> : null}
          <ErrorBanner error={selectDetail.error} />

          <Card>
            <CardHeader
              title="Past runs"
              subtitle="Newest first. Select one to see its full 17.4 report."
            />
            {runs.loading ? (
              <SkeletonRows rows={4} />
            ) : (runs.data?.runs ?? []).length === 0 ? (
              <EmptyState
                title="No simulation has been run yet."
                detail="Use the form above to run the first one."
              />
            ) : (
              <TableShell
                head={
                  <>
                    <Th>Started</Th>
                    <Th>Strategy</Th>
                    <Th>Baseline</Th>
                    <Th>Dataset</Th>
                    <Th align="right">Recovered</Th>
                    <Th align="right">Recovery rate</Th>
                    <Th align="right">Uplift</Th>
                    <Th align="right">Escalated</Th>
                    <Th align="right">Policy violations</Th>
                    <Th />
                  </>
                }
              >
                {(runs.data?.runs ?? []).map((run) => (
                  <tr key={run.id} className="hover:bg-ink-700/40">
                    <Td>{formatDateTime(run.started_at)}</Td>
                    <Td>{humanize(run.strategy)}</Td>
                    <Td>{humanize(run.baseline)}</Td>
                    <Td className="font-mono text-xs text-dim" title={`seed ${run.seed}`}>
                      {run.dataset_version}
                    </Td>
                    <Td align="right">{formatMoneyKPI(run.result.recovered_amount)}</Td>
                    <Td align="right">{formatPercent(run.result.recovery_rate)}</Td>
                    <Td
                      align="right"
                      className={
                        run.uplift_percent !== undefined && run.uplift_percent < 0
                          ? 'text-block'
                          : 'text-recovered'
                      }
                    >
                      {run.uplift_percent !== undefined ? formatSignedPercent(run.uplift_percent) : '—'}
                    </Td>
                    <Td align="right">{formatCount(run.result.escalated)}</Td>
                    <Td
                      align="right"
                      className={run.result.policy_violations > 0 ? 'text-block' : undefined}
                    >
                      {formatCount(run.result.policy_violations)}
                    </Td>
                    <Td align="right">
                      <Button onClick={() => void openRun(run)} pending={selectDetail.pending}>
                        View report
                      </Button>
                    </Td>
                  </tr>
                ))}
              </TableShell>
            )}
          </Card>
        </>
      )}
    </>
  );
}

function RunDetail({ response }: { response: SimulationResponse }) {
  const { run, report, agent_evaluation } = response;

  return (
    <Card>
      <CardHeader
        title={`Report · ${humanize(run.strategy)} vs ${humanize(run.baseline)}`}
        subtitle={`Dataset ${response.reproduce.dataset_version} · seed ${response.reproduce.seed} · policy ${response.reproduce.policy_version}`}
        right={<DataLabel label={response.data_label} />}
      />

      {/* SRS 17.4: rendered in the order and with the labels the SRS specifies. */}
      <div className="grid grid-cols-1 gap-x-8 gap-y-1 p-4 font-mono text-xs sm:grid-cols-2 sm:p-5">
        {report.map((line) => (
          <div key={line.label} className="flex items-baseline justify-between gap-3 border-b border-line/60 py-1.5">
            <span className="text-dim">{line.label}</span>
            <span className="tnum text-body">{line.value}</span>
          </div>
        ))}
      </div>

      {run.baseline_result ? (
        <div className="grid grid-cols-1 gap-3 border-t border-line p-4 sm:grid-cols-2 sm:p-5">
          <StrategyResult title={humanize(run.strategy)} accent result={run.result} />
          <StrategyResult title={humanize(run.baseline)} result={run.baseline_result} />
        </div>
      ) : null}

      {agent_evaluation ? (
        <div className="border-t border-line p-4 sm:p-5">
          <p className="label mb-3">Agent evaluation (SRS 22.3)</p>
          <div className="grid grid-cols-2 gap-x-6 gap-y-3 sm:grid-cols-4">
            <Metric
              label="Detection precision"
              value={formatPercent(agent_evaluation.detection_precision)}
              target=">= 80%"
            />
            <Metric
              label="Detection recall"
              value={formatPercent(agent_evaluation.detection_recall)}
              target=">= 75%"
            />
            <Metric label="Detection F1" value={formatPercent(agent_evaluation.detection_f1)} />
            <Metric
              label="Diagnosis accuracy"
              value={formatPercent(agent_evaluation.diagnosis_accuracy)}
              target=">= 80%"
            />
            <Metric
              label="Intervention accuracy"
              value={formatPercent(agent_evaluation.intervention_accuracy)}
              target=">= 75%"
              hint={`Graded on ${formatCount(agent_evaluation.interventions_graded)} cases; ${formatCount(agent_evaluation.interventions_deferred)} deferred to UNKNOWN.`}
            />
            <Metric
              label="Calibration error"
              value={agent_evaluation.calibration_samples > 0 ? agent_evaluation.calibration_error.toFixed(3) : '—'}
              hint={`${formatCount(agent_evaluation.calibration_samples)} samples with a verified outcome.`}
            />
            <Metric label="Evidence coverage" value={formatPercent(agent_evaluation.evidence_coverage)} />
            <Metric
              label="Schema-valid rate"
              // model_calls === 0 means no model was ever invoked (deterministic
              // fallback only) — the backend leaves schema_valid_rate at its zero
              // default in that case (evaluate.go only computes it when
              // model_calls > 0), so showing "0.0%" here would misreport "the model
              // always failed" when the true story is "no model ran".
              value={agent_evaluation.model_calls > 0 ? formatPercent(agent_evaluation.schema_valid_rate) : 'N/A'}
              hint={
                agent_evaluation.model_calls > 0
                  ? `${formatCount(agent_evaluation.model_calls)} model calls.`
                  : 'No model calls were made — this run used the deterministic fallback only.'
              }
            />
          </div>
          {agent_evaluation.unauthorized_actions > 0 ? (
            <p className="mt-3 text-xs text-block">
              {formatCount(agent_evaluation.unauthorized_actions)} unauthorized action(s) — AC-002 / AC-003 target
              is zero.
            </p>
          ) : null}
        </div>
      ) : null}
    </Card>
  );
}

function StrategyResult({
  title,
  result,
  accent,
}: {
  title: string;
  result: SimulationRun['result'];
  accent?: boolean;
}) {
  return (
    <div className={`rounded-card border p-4 ${accent ? 'border-accent/30 bg-accent-soft/30' : 'border-line'}`}>
      <p className="text-xs font-semibold text-body">{title}</p>
      <dl className="mt-2 space-y-1 text-xs">
        <Row label="Cases processed" value={formatCount(result.cases_processed)} />
        <Row label="Recovered" value={formatMoneyKPI(result.recovered_amount)} />
        <Row label="Recovery rate" value={formatPercent(result.recovery_rate)} />
        <Row label="Actions executed" value={formatCount(result.actions_executed)} />
        <Row label="Escalated" value={formatCount(result.escalated)} />
        <Row label="Stopped safely" value={formatCount(result.stopped_safely)} />
        <Row label="Blocked" value={formatCount(result.blocked)} />
        <Row label="Errors" value={formatCount(result.errors)} />
      </dl>
    </div>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-2">
      <dt className="text-dim">{label}</dt>
      <dd className="tnum text-body">{value}</dd>
    </div>
  );
}

function Metric({
  label,
  value,
  target,
  hint,
}: {
  label: string;
  value: string;
  target?: string;
  hint?: string;
}) {
  return (
    <div title={hint}>
      <p className="label">{label}</p>
      <p className="tnum mt-1 text-sm font-medium text-body">{value}</p>
      {target ? <p className="text-2xs text-dim">target {target}</p> : null}
    </div>
  );
}