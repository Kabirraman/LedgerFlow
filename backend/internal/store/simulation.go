package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// --- benchmark datasets (SRS 17.1, 25.2) ---

// SaveDataset persists a generated benchmark dataset.
//
// (version, seed, size) is unique: re-generating with the same seed returns the
// stored dataset instead of writing a second copy. That is what makes a claimed
// uplift checkable — anyone can regenerate the identical dataset and re-run it
// (SRS 25.2, NFR-008).
func (s *Store) SaveDataset(ctx context.Context, d *domain.BenchmarkDataset) error {
	if d.ID == "" {
		d.ID = NewID("bds")
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = s.now()
	}
	if d.Mix == nil {
		d.Mix = map[string]int{}
	}
	mix, err := json.Marshal(d.Mix)
	if err != nil {
		return fmt.Errorf("marshal dataset mix: %w", err)
	}
	cases, err := json.Marshal(d.Cases)
	if err != nil {
		return fmt.Errorf("marshal dataset cases: %w", err)
	}

	const q = `
		INSERT INTO benchmark_datasets (id, version, seed, size, mix, cases_json, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	_, execErr := s.pool.Exec(ctx, q, d.ID, d.Version, d.Seed, d.Size, mix, cases, d.CreatedAt)
	if execErr == nil {
		return nil
	}
	if !IsUniqueViolation(execErr) {
		return execErr
	}
	existing, getErr := s.FindDataset(ctx, d.Version, d.Seed, d.Size)
	if getErr != nil {
		return execErr
	}
	casesForRun := d.Cases
	*d = *existing
	d.Cases = casesForRun
	return nil
}

const datasetCols = `id, version, seed, size, mix, created_at`

func scanDataset(row rowScanner) (*domain.BenchmarkDataset, error) {
	var d domain.BenchmarkDataset
	var mix []byte
	if err := row.Scan(&d.ID, &d.Version, &d.Seed, &d.Size, &mix, &d.CreatedAt); err != nil {
		return nil, err
	}
	d.Mix = map[string]int{}
	if len(mix) > 0 {
		_ = json.Unmarshal(mix, &d.Mix)
	}
	return &d, nil
}

// GetDataset loads dataset metadata without its (large) case payload.
func (s *Store) GetDataset(ctx context.Context, id string) (*domain.BenchmarkDataset, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+datasetCols+` FROM benchmark_datasets WHERE id = $1`, id)
	d, err := scanDataset(row)
	if err != nil {
		return nil, notFound(err, "dataset "+id)
	}
	return d, nil
}

// FindDataset resolves a dataset by its reproducibility triple.
func (s *Store) FindDataset(ctx context.Context, version string, seed int64, size int) (*domain.BenchmarkDataset, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+datasetCols+
		` FROM benchmark_datasets WHERE version = $1 AND seed = $2 AND size = $3`, version, seed, size)
	d, err := scanDataset(row)
	if err != nil {
		return nil, notFound(err, fmt.Sprintf("dataset %s/%d/%d", version, seed, size))
	}
	return d, nil
}

// LoadDatasetCases returns the dataset with its cases populated.
func (s *Store) LoadDatasetCases(ctx context.Context, id string) (*domain.BenchmarkDataset, error) {
	d, err := s.GetDataset(ctx, id)
	if err != nil {
		return nil, err
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT cases_json FROM benchmark_datasets WHERE id = $1`, id).Scan(&raw); err != nil {
		return nil, notFound(err, "dataset cases "+id)
	}
	if err := json.Unmarshal(raw, &d.Cases); err != nil {
		return nil, fmt.Errorf("decode dataset cases: %w", err)
	}
	return d, nil
}

// ListDatasets returns dataset metadata, newest first.
func (s *Store) ListDatasets(ctx context.Context, limit int) ([]domain.BenchmarkDataset, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT `+datasetCols+
		` FROM benchmark_datasets ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.BenchmarkDataset{}
	for rows.Next() {
		d, err := scanDataset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// --- simulation runs (SRS 17.4) ---

// StartRun records a simulation as running, so a long benchmark is visible in
// the UI while it executes rather than appearing only on completion.
func (s *Store) StartRun(ctx context.Context, r *domain.SimulationRun) error {
	if r.ID == "" {
		r.ID = NewID("sim")
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = s.now()
	}
	if r.Status == "" {
		r.Status = "running"
	}
	const q = `
		INSERT INTO simulation_runs (id, dataset_id, dataset_version, seed, policy_version, strategy,
		                             baseline, status, started_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	_, err := s.pool.Exec(ctx, q, r.ID, r.DatasetID, r.DatasetVersion, r.Seed, r.PolicyVersion,
		r.Strategy, r.Baseline, r.Status, r.StartedAt, r.CreatedBy)
	return err
}

// FinishRun stores the completed result block, baseline comparison, uplift and
// agent evaluation.
func (s *Store) FinishRun(ctx context.Context, r *domain.SimulationRun) error {
	result, err := json.Marshal(r.Result)
	if err != nil {
		return fmt.Errorf("marshal simulation result: %w", err)
	}
	var baseline, evaluation []byte
	if r.BaselineResult != nil {
		if baseline, err = json.Marshal(r.BaselineResult); err != nil {
			return fmt.Errorf("marshal baseline result: %w", err)
		}
	}
	if r.Agreement != nil {
		if evaluation, err = json.Marshal(r.Agreement); err != nil {
			return fmt.Errorf("marshal agent evaluation: %w", err)
		}
	}
	finished := s.now()
	if r.FinishedAt == nil {
		r.FinishedAt = &finished
	}
	if r.DurationMS == 0 {
		r.DurationMS = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	}
	if r.Status == "" || r.Status == "running" {
		r.Status = "completed"
	}

	const q = `
		UPDATE simulation_runs SET status = $2, result_json = $3, baseline_json = $4,
			evaluation_json = $5, uplift_percent = $6, finished_at = $7, duration_ms = $8,
			baseline = $9
		WHERE id = $1`
	_, err = s.pool.Exec(ctx, q, r.ID, r.Status, result, baseline, evaluation,
		r.UpliftPercent, r.FinishedAt, r.DurationMS, r.Baseline)
	return err
}

// FailRun marks a run as failed with a reason, so a crashed benchmark does not
// sit at "running" forever.
func (s *Store) FailRun(ctx context.Context, id, reason string) error {
	payload, _ := json.Marshal(map[string]string{"error": truncate(reason, 500)})
	finished := s.now()
	_, err := s.pool.Exec(ctx, `
		UPDATE simulation_runs SET status = 'failed', result_json = COALESCE(result_json, $3),
			finished_at = $2, duration_ms = EXTRACT(EPOCH FROM ($2 - started_at)) * 1000
		WHERE id = $1 AND status = 'running'`, id, finished, payload)
	return err
}

const runCols = `id, dataset_id, dataset_version, seed, policy_version, strategy, baseline, status,
	result_json, baseline_json, evaluation_json, uplift_percent, started_at, finished_at,
	duration_ms, created_by`

func scanRun(row rowScanner) (*domain.SimulationRun, error) {
	var r domain.SimulationRun
	var result, baseline, evaluation []byte
	err := row.Scan(&r.ID, &r.DatasetID, &r.DatasetVersion, &r.Seed, &r.PolicyVersion,
		&r.Strategy, &r.Baseline, &r.Status, &result, &baseline, &evaluation,
		&r.UpliftPercent, &r.StartedAt, &r.FinishedAt, &r.DurationMS, &r.CreatedBy)
	if err != nil {
		return nil, err
	}
	if len(result) > 0 {
		_ = json.Unmarshal(result, &r.Result)
	}
	if len(baseline) > 0 {
		var b domain.SimulationResult
		if json.Unmarshal(baseline, &b) == nil {
			r.BaselineResult = &b
		}
	}
	if len(evaluation) > 0 {
		var e domain.AgentEvaluation
		if json.Unmarshal(evaluation, &e) == nil {
			r.Agreement = &e
		}
	}
	return &r, nil
}

// GetRun loads one simulation run.
func (s *Store) GetRun(ctx context.Context, id string) (*domain.SimulationRun, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM simulation_runs WHERE id = $1`, id)
	r, err := scanRun(row)
	if err != nil {
		return nil, notFound(err, "simulation run "+id)
	}
	return r, nil
}

// ListRuns returns simulation history, newest first.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]domain.SimulationRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT `+runCols+
		` FROM simulation_runs ORDER BY started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.SimulationRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
