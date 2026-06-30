package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migpostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Migrate(dsn string) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	connConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return err
	}
	db := stdlib.OpenDB(*connConfig.ConnConfig)
	defer db.Close()
	driver, err := migpostgres.WithInstance(db, &migpostgres.Config{})
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

func (p *Postgres) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

func (p *Postgres) Close() {
	if p.pool != nil {
		p.pool.Close()
	}
}

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint violation (23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

func (p *Postgres) UpsertJob(ctx context.Context, name, apiVersion string, spec []byte) (*api.Job, error) {
	const q = `
		INSERT INTO jobs(name, api_version, spec, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (name) DO UPDATE
		  SET api_version = EXCLUDED.api_version,
		      spec        = EXCLUDED.spec,
		      updated_at  = NOW()
		RETURNING id, name, api_version, spec, updated_at;
	`
	var j api.Job
	err := p.pool.QueryRow(ctx, q, name, apiVersion, spec).
		Scan(&j.ID, &j.Name, &j.APIVersion, &j.Spec, &j.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert job: %w", err)
	}
	return &j, nil
}

func (p *Postgres) GetJob(ctx context.Context, name string) (*api.Job, error) {
	const q = `SELECT id, name, api_version, spec, updated_at FROM jobs WHERE name = $1`
	var j api.Job
	err := p.pool.QueryRow(ctx, q, name).
		Scan(&j.ID, &j.Name, &j.APIVersion, &j.Spec, &j.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get job: %w", err)
	}
	return &j, nil
}

func (p *Postgres) ListJobs(ctx context.Context) ([]api.Job, error) {
	const q = `SELECT id, name, api_version, spec, updated_at FROM jobs ORDER BY name`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []api.Job
	for rows.Next() {
		var j api.Job
		if err := rows.Scan(&j.ID, &j.Name, &j.APIVersion, &j.Spec, &j.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (p *Postgres) CreateRun(ctx context.Context, jobName string, params map[string]string, spec []byte, agentSelector []string, triggeredBy string) (*api.Run, error) {
	if params == nil {
		params = map[string]string{}
	}
	if agentSelector == nil {
		agentSelector = []string{}
	}
	if triggeredBy == "" {
		triggeredBy = "api"
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	const q = `
		INSERT INTO runs(job_name, params, spec, agent_selector, triggered_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, job_name, status, params, created_at, updated_at, triggered_by;
	`
	var r api.Run
	var paramsOut []byte
	var status string
	err = p.pool.QueryRow(ctx, q, jobName, paramsJSON, spec, agentSelector, triggeredBy).
		Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy)
	if err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	r.Status = api.RunStatus(status)
	_ = json.Unmarshal(paramsOut, &r.Params)
	if r.Params == nil {
		r.Params = map[string]string{}
	}
	return &r, nil
}

func (p *Postgres) GetRun(ctx context.Context, id string) (*api.Run, error) {
	const q = `SELECT id, job_name, status, params, created_at, updated_at, triggered_by FROM runs WHERE id = $1`
	var r api.Run
	var paramsOut []byte
	var status string
	err := p.pool.QueryRow(ctx, q, id).
		Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	r.Status = api.RunStatus(status)
	_ = json.Unmarshal(paramsOut, &r.Params)
	if r.Params == nil {
		r.Params = map[string]string{}
	}
	return &r, nil
}

func (p *Postgres) GetRunSpec(ctx context.Context, id string) ([]byte, error) {
	const q = `SELECT spec FROM runs WHERE id = $1`
	var spec []byte
	err := p.pool.QueryRow(ctx, q, id).Scan(&spec)
	if err != nil {
		return nil, fmt.Errorf("get run spec: %w", err)
	}
	return spec, nil
}

func (p *Postgres) ListRunsByJob(ctx context.Context, jobName string, limit int) ([]api.Run, error) {
	const q = `
		SELECT id, job_name, status, params, created_at, updated_at, triggered_by
		FROM runs WHERE job_name = $1
		ORDER BY created_at DESC LIMIT $2;
	`
	rows, err := p.pool.Query(ctx, q, jobName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Run
	for rows.Next() {
		var r api.Run
		var status string
		var paramsOut []byte
		if err := rows.Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy); err != nil {
			return nil, err
		}
		r.Status = api.RunStatus(status)
		_ = json.Unmarshal(paramsOut, &r.Params)
		if r.Params == nil {
			r.Params = map[string]string{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) ListActiveRuns(ctx context.Context) ([]api.Run, error) {
	const q = `
		SELECT id, job_name, status, params, created_at, updated_at, triggered_by
		FROM runs WHERE status IN ('Pending', 'Queued', 'Running')
		ORDER BY created_at DESC;
	`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Run
	for rows.Next() {
		var r api.Run
		var status string
		var paramsOut []byte
		if err := rows.Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy); err != nil {
			return nil, err
		}
		r.Status = api.RunStatus(status)
		_ = json.Unmarshal(paramsOut, &r.Params)
		if r.Params == nil {
			r.Params = map[string]string{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) TransitionPendingToQueued(ctx context.Context, limit int) (int, error) {
	// Take a snapshot of Pending runs (no lock)
	rows, err := p.pool.Query(ctx,
		`SELECT id, spec, params FROM runs WHERE status = 'Pending' ORDER BY created_at LIMIT $1`,
		limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id     string
		spec   []byte
		params []byte
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.spec, &c.params); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, c := range candidates {
		queued, err := p.tryQueueRun(ctx, c.id, c.spec, c.params)
		if err != nil {
			return count, err
		}
		if queued {
			count++
		}
	}
	return count, nil
}

// systemLogStepIndex is the sentinel value for logs.step_index that represents
// controller-originated system messages not associated with any step.
const systemLogStepIndex = -1

// tryQueueRun acquires concurrency locks inside a transaction and transitions a run to Queued.
func (p *Postgres) tryQueueRun(ctx context.Context, runID string, specJSON []byte, paramsJSON []byte) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	// Check that no other scheduler has already processed this run (acquire row lock)
	var statusStr string
	err = tx.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1 FOR UPDATE`,
		runID).Scan(&statusStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if statusStr != "Pending" {
		return false, nil
	}

	// Parse concurrency configuration from spec
	var spec dsl.Spec
	if len(specJSON) > 0 {
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			// treat invalid spec as having no concurrency constraints
			spec = dsl.Spec{}
		}
	}

	// Skip runs with unresolved git:// URIs — RunGitResolver resolves them before queuing.
	for _, st := range spec.Steps {
		if st.Uses != nil && strings.HasPrefix(st.Uses.Job, "git://") {
			return false, nil
		}
	}

	// Expand parameter templates in concurrency lock pool/mutex names if present
	var params map[string]string
	if len(paramsJSON) > 0 {
		_ = json.Unmarshal(paramsJSON, &params)
	}
	concurrency, expandErr := dsl.ExpandConcurrency(spec.Concurrency, params)
	if expandErr != nil {
		// Explicitly rollback to release the row lock held by `SELECT ... FOR UPDATE`.
		// tx still holds a lock on this runs row, and AppendLog/MarkRunFinished update
		// the same row on a different connection (p.pool). Calling them without rolling
		// back first would deadlock on our own lock (the defer tx.Rollback(ctx) at the
		// end of the function would be too late).
		tx.Rollback(ctx)
		msg := fmt.Sprintf("concurrency template expansion failed: %v", expandErr)
		if _, lerr := p.AppendLog(ctx, runID, systemLogStepIndex, "stderr", time.Now(), msg); lerr != nil {
			slog.Warn("append system log failed", "runID", runID, "error", lerr)
		}
		if ferr := p.MarkRunFinished(ctx, runID, api.RunFailed); ferr != nil {
			slog.Warn("mark run failed failed", "runID", runID, "error", ferr)
		}
		return false, nil
	}

	// Acquire concurrency locks
	orLockValues := map[string]string{}
	if concurrency != nil {
		if concurrency.Mutex != "" {
			_, lockErr := tx.Exec(ctx,
				`INSERT INTO mutex_holders(mutex_name, run_id) VALUES ($1, $2)`,
				concurrency.Mutex, runID)
			if lockErr != nil {
				if isUniqueViolation(lockErr) {
					return false, nil // another run holds the mutex
				}
				return false, lockErr
			}
		}
		for _, nl := range concurrency.Semaphores {
			// lazily create pool slots if they do not exist yet (propagate errors)
			if _, err := tx.Exec(ctx,
				`INSERT INTO named_lock_slots(pool_name, slot_id)
				 SELECT $1, generate_series(1, $2) ON CONFLICT DO NOTHING`,
				nl.Pool, nl.Capacity); err != nil {
				return false, fmt.Errorf("upsert named_lock_slots pool %q: %w", nl.Pool, err)
			}
			// try to acquire a slot
			const acquireQ = `
				WITH slot AS (
					SELECT slot_id FROM named_lock_slots
					WHERE pool_name = $1 AND run_id IS NULL
					ORDER BY slot_id LIMIT 1 FOR UPDATE SKIP LOCKED
				)
				UPDATE named_lock_slots ns
				SET run_id = $2, acquired_at = NOW()
				FROM slot
				WHERE ns.pool_name = $1 AND ns.slot_id = slot.slot_id
				RETURNING ns.slot_id;
			`
			var slotID int
			slotErr := tx.QueryRow(ctx, acquireQ, nl.Pool, runID).Scan(&slotID)
			if errors.Is(slotErr, pgx.ErrNoRows) {
				return false, nil // no available slot
			}
			if slotErr != nil {
				return false, slotErr
			}
		}
		// OrLocks: try candidates in order and stop at the first one acquired.
		// We reuse mutex_holders, so no new table or release code is needed
		// (the existing `DELETE FROM mutex_holders WHERE run_id = $1` at run end still works).
		// Each candidate INSERT is performed inside a SAVEPOINT (nested tx.Begin).
		// A unique-constraint violation aborts the whole transaction, so without a SAVEPOINT
		// trying the next candidate would fail with "current transaction is aborted".
		for _, ol := range concurrency.OrLocks {
			acquired := ""
			for _, candidate := range ol.In.Literal {
				acquiredCandidate, lockErr := func() (bool, error) {
					sp, err := tx.Begin(ctx)
					if err != nil {
						return false, err
					}
					_, lockErr := sp.Exec(ctx,
						`INSERT INTO mutex_holders(mutex_name, run_id) VALUES ($1, $2)`,
						candidate, runID)
					if lockErr != nil {
						_ = sp.Rollback(ctx)
						if isUniqueViolation(lockErr) {
							return false, nil // this candidate is held by another run; try the next one.
						}
						return false, lockErr
					}
					return true, sp.Commit(ctx)
				}()
				if lockErr != nil {
					return false, lockErr
				}
				if acquiredCandidate {
					acquired = candidate
					break
				}
			}
			if acquired == "" {
				return false, nil // all candidates are exhausted
			}
			orLockValues[strings.ToUpper(ol.Name)+"_LOCK_VALUE"] = acquired
		}
	}

	// Merge OrLock-acquired values into the existing parameters. If there is a key
	// conflict, fail this run the same way a template expansion error would
	// (tx still holds a lock on this runs row, so we must explicitly Rollback to
	// release the lock before calling AppendLog/MarkRunFinished).
	if len(orLockValues) > 0 {
		for k := range orLockValues {
			if _, exists := params[k]; exists {
				tx.Rollback(ctx)
				msg := fmt.Sprintf("or-lock variable %q conflicts with an existing parameter", k)
				if _, lerr := p.AppendLog(ctx, runID, systemLogStepIndex, "stderr", time.Now(), msg); lerr != nil {
					slog.Warn("append system log failed", "runID", runID, "error", lerr)
				}
				if ferr := p.MarkRunFinished(ctx, runID, api.RunFailed); ferr != nil {
					slog.Warn("mark run failed failed", "runID", runID, "error", ferr)
				}
				return false, nil
			}
		}
		mergedJSON, err := json.Marshal(orLockValues)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE runs SET params = params || $1::jsonb WHERE id = $2`,
			mergedJSON, runID); err != nil {
			return false, err
		}
	}

	// Transition to Queued
	if _, err := tx.Exec(ctx,
		`UPDATE runs SET status = 'Queued', updated_at = NOW() WHERE id = $1`,
		runID); err != nil {
		return false, err
	}

	return true, tx.Commit(ctx)
}

func (p *Postgres) ClaimNextRun(ctx context.Context, agentID string, agentLabels []string) (*ClaimedRun, error) {
	if agentLabels == nil {
		agentLabels = []string{}
	}
	const q = `
		WITH picked AS (
			SELECT id FROM runs
			WHERE status = 'Queued'
			  AND (agent_selector = '{}' OR agent_selector <@ $2::TEXT[])
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE runs r
		SET claimed_by = $1, claimed_at = NOW(), updated_at = NOW(), status = 'Running'
		FROM picked WHERE r.id = picked.id
		RETURNING r.id, r.job_name, r.status, r.params, r.spec, r.created_at, r.updated_at;
	`
	var cr ClaimedRun
	var status string
	var paramsOut []byte
	err := p.pool.QueryRow(ctx, q, agentID, agentLabels).
		Scan(&cr.ID, &cr.JobName, &status, &paramsOut, &cr.Spec, &cr.CreatedAt, &cr.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim next run: %w", err)
	}
	cr.Status = api.RunStatus(status)
	_ = json.Unmarshal(paramsOut, &cr.Params)
	if cr.Params == nil {
		cr.Params = map[string]string{}
	}
	return &cr, nil
}

func (p *Postgres) MarkRunRunning(ctx context.Context, runID string) error {
	_, err := p.pool.Exec(ctx, `UPDATE runs SET status = 'Running', updated_at = NOW() WHERE id = $1 AND status NOT IN ('Succeeded', 'Failed', 'Cancelled')`, runID)
	return err
}

func (p *Postgres) MarkRunFinished(ctx context.Context, runID string, status api.RunStatus) error {
	switch status {
	case api.RunSucceeded, api.RunFailed, api.RunCancelled:
	default:
		return fmt.Errorf("invalid terminal status %q", status)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE runs SET status = $1, updated_at = NOW()
WHERE id = $2 AND status NOT IN ('Succeeded', 'Failed', 'Cancelled')`,
		string(status), runID); err != nil {
		return err
	}
	// release mutex
	if _, err := tx.Exec(ctx,
		`DELETE FROM mutex_holders WHERE run_id = $1`, runID); err != nil {
		return err
	}
	// release named lock slot
	if _, err := tx.Exec(ctx,
		`UPDATE named_lock_slots SET run_id = NULL, acquired_at = NULL WHERE run_id = $1`,
		runID); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// DeleteRun deletes a Run. step_reports/logs/run_outputs etc. are cascade-deleted
// by the existing ON DELETE CASCADE constraints.
func (p *Postgres) DeleteRun(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, id)
	return err
}

func (p *Postgres) UpsertStepReport(ctx context.Context, runID string, stepIndex int, stageIndex int, stepName, status string, exitCode *int, startedAt, endedAt *time.Time) error {
	const q = `
		INSERT INTO step_reports(run_id, step_index, stage_index, step_name, status, exit_code, started_at, ended_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (run_id, step_index) DO UPDATE
		  SET stage_index = EXCLUDED.stage_index,
		      step_name   = EXCLUDED.step_name,
		      status      = EXCLUDED.status,
		      exit_code   = COALESCE(EXCLUDED.exit_code, step_reports.exit_code),
		      started_at  = COALESCE(EXCLUDED.started_at, step_reports.started_at),
		      ended_at    = COALESCE(EXCLUDED.ended_at, step_reports.ended_at);
	`
	_, err := p.pool.Exec(ctx, q, runID, stepIndex, stageIndex, stepName, status, exitCode, startedAt, endedAt)
	return err
}

func (p *Postgres) GetRunSteps(ctx context.Context, runID string) ([]api.StepReport, error) {
	const q = `
		SELECT step_index, stage_index, step_name, status, exit_code, started_at, ended_at
		FROM step_reports
		WHERE run_id = $1
		ORDER BY step_index;
	`
	rows, err := p.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.StepReport
	for rows.Next() {
		var s api.StepReport
		if err := rows.Scan(&s.Index, &s.StageIndex, &s.Name, &s.Status, &s.ExitCode, &s.StartedAt, &s.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *Postgres) AppendLog(ctx context.Context, runID string, stepIndex int, stream string, ts time.Time, line string) (int64, error) {
	const q = `
		INSERT INTO logs(run_id, step_index, stream, ts, line)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING seq;
	`
	var seq int64
	err := p.pool.QueryRow(ctx, q, runID, stepIndex, stream, ts, line).Scan(&seq)
	if err != nil {
		return 0, err
	}
	// notify listeners of the new log entry
	_, _ = p.pool.Exec(ctx, "SELECT pg_notify($1, $2)", "log_appended:"+runID, fmt.Sprintf("%d", seq))
	return seq, nil
}

func (p *Postgres) TailLogs(ctx context.Context, runID string, afterSeq int64, limit int) ([]api.LogLine, error) {
	const q = `
		SELECT seq, step_index, stream, ts, line
		FROM logs WHERE run_id = $1 AND seq > $2
		ORDER BY seq LIMIT $3;
	`
	rows, err := p.pool.Query(ctx, q, runID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.LogLine
	for rows.Next() {
		var l api.LogLine
		if err := rows.Scan(&l.Seq, &l.StepIndex, &l.Stream, &l.Timestamp, &l.Line); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (p *Postgres) UpsertAgent(ctx context.Context, agentID, hostname, os, version string, labels []string, env map[string]string) error {
	if labels == nil {
		labels = []string{}
	}
	if env == nil {
		env = map[string]string{}
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return err
	}
	const q = `
		INSERT INTO agents(id, hostname, os, labels, version, env, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (id) DO UPDATE
		  SET hostname     = EXCLUDED.hostname,
		      os           = EXCLUDED.os,
		      labels       = EXCLUDED.labels,
		      version      = EXCLUDED.version,
		      env          = EXCLUDED.env,
		      last_seen_at = NOW();
	`
	_, err = p.pool.Exec(ctx, q, agentID, hostname, os, labels, version, envJSON)
	return err
}

func (p *Postgres) TouchAgent(ctx context.Context, agentID string) error {
	_, err := p.pool.Exec(ctx, `UPDATE agents SET last_seen_at = NOW() WHERE id = $1`, agentID)
	return err
}

func (p *Postgres) DeleteAgent(ctx context.Context, agentID string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM agents WHERE id = $1`, agentID)
	return err
}

func (p *Postgres) ListAgents(ctx context.Context) ([]api.AgentInfo, error) {
	const q = `SELECT id, hostname, os, labels, version, env, last_seen_at FROM agents ORDER BY last_seen_at DESC`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.AgentInfo
	for rows.Next() {
		var a api.AgentInfo
		var envJSON []byte
		if err := rows.Scan(&a.ID, &a.Hostname, &a.OS, &a.Labels, &a.Version, &envJSON, &a.LastSeenAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(envJSON, &a.Env)
		if a.Env == nil {
			a.Env = map[string]string{}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (p *Postgres) GetAgent(ctx context.Context, id string) (*api.AgentInfo, error) {
	const q = `SELECT id, hostname, os, labels, version, env, last_seen_at FROM agents WHERE id = $1`
	var a api.AgentInfo
	var envJSON []byte
	err := p.pool.QueryRow(ctx, q, id).
		Scan(&a.ID, &a.Hostname, &a.OS, &a.Labels, &a.Version, &envJSON, &a.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent: %w", err)
	}
	_ = json.Unmarshal(envJSON, &a.Env)
	if a.Env == nil {
		a.Env = map[string]string{}
	}
	return &a, nil
}

func (p *Postgres) ListRunsByAgent(ctx context.Context, agentID string, limit int) ([]api.Run, error) {
	const q = `
		SELECT id, job_name, status, params, created_at, updated_at, triggered_by
		FROM runs WHERE claimed_by = $1
		ORDER BY created_at DESC LIMIT $2;
	`
	rows, err := p.pool.Query(ctx, q, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Run
	for rows.Next() {
		var r api.Run
		var status string
		var paramsOut []byte
		if err := rows.Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy); err != nil {
			return nil, err
		}
		r.Status = api.RunStatus(status)
		_ = json.Unmarshal(paramsOut, &r.Params)
		if r.Params == nil {
			r.Params = map[string]string{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteStaleAgents(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM agents WHERE last_seen_at < NOW() - $1::interval`,
		olderThan.String())
	if err != nil {
		return 0, fmt.Errorf("delete stale agents: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (p *Postgres) AcquireMutex(ctx context.Context, mutexName, runID string) (bool, error) {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO mutex_holders(mutex_name, run_id) VALUES ($1, $2)`,
		mutexName, runID)
	if err == nil {
		return true, nil
	}
	if isUniqueViolation(err) {
		return false, nil
	}
	return false, err
}

func (p *Postgres) ReleaseMutex(ctx context.Context, mutexName string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM mutex_holders WHERE mutex_name = $1`, mutexName)
	return err
}

func (p *Postgres) UpsertSemaphorePool(ctx context.Context, poolName string, capacity int) error {
	const q = `
		INSERT INTO named_lock_slots(pool_name, slot_id)
		SELECT $1, generate_series(1, $2)
		ON CONFLICT (pool_name, slot_id) DO NOTHING;
	`
	_, err := p.pool.Exec(ctx, q, poolName, capacity)
	return err
}

func (p *Postgres) AcquireSemaphore(ctx context.Context, poolName, runID string) (bool, error) {
	const q = `
		WITH slot AS (
			SELECT slot_id FROM named_lock_slots
			WHERE pool_name = $1 AND run_id IS NULL
			ORDER BY slot_id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE named_lock_slots ns
		SET run_id = $2, acquired_at = NOW()
		FROM slot
		WHERE ns.pool_name = $1 AND ns.slot_id = slot.slot_id
		RETURNING ns.slot_id;
	`
	var slotID int
	err := p.pool.QueryRow(ctx, q, poolName, runID).Scan(&slotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (p *Postgres) ReleaseSemaphore(ctx context.Context, poolName, runID string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE named_lock_slots SET run_id = NULL, acquired_at = NULL
		 WHERE pool_name = $1 AND run_id = $2`,
		poolName, runID)
	return err
}
func (p *Postgres) SetStepOutput(ctx context.Context, runID string, stepIndex int, key, value string) error {
	const q = `
		INSERT INTO step_outputs(run_id, step_index, key, value)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id, step_index, key) DO UPDATE SET value = EXCLUDED.value;
	`
	_, err := p.pool.Exec(ctx, q, runID, stepIndex, key, value)
	return err
}

func (p *Postgres) GetStepOutputs(ctx context.Context, runID string, stepIndex int) (map[string]string, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT key, value FROM step_outputs WHERE run_id = $1 AND step_index = $2`,
		runID, stepIndex)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (p *Postgres) SetRunOutput(ctx context.Context, runID, key, value string) error {
	const q = `
		INSERT INTO run_outputs(run_id, key, value)
		VALUES ($1, $2, $3)
		ON CONFLICT (run_id, key) DO UPDATE SET value = EXCLUDED.value;
	`
	_, err := p.pool.Exec(ctx, q, runID, key, value)
	return err
}

func (p *Postgres) GetRunOutputs(ctx context.Context, runID string) (map[string]string, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT key, value FROM run_outputs WHERE run_id = $1 ORDER BY key`,
		runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

const schedulerLockKey = int64(0x65786364) // 'excd'

// AcquireAdvisoryLock acquires a session-level advisory lock for the given key on a dedicated connection.
func (p *Postgres) AcquireAdvisoryLock(ctx context.Context, key int64) (func(), error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, err
	}
	if !acquired {
		conn.Release()
		return nil, nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
				slog.Warn("advisory unlock failed", "key", key, "error", err)
			}
			conn.Release()
		})
	}, nil
}

// AcquireSchedulerLock acquires a session-level advisory lock on a dedicated connection.
// The same physical connection is used for both acquire and unlock to satisfy Postgres semantics.
func (p *Postgres) AcquireSchedulerLock(ctx context.Context) (release func(), err error) {
	return p.AcquireAdvisoryLock(ctx, schedulerLockKey)
}

func (p *Postgres) ListRunsNeedingArchival(ctx context.Context, limit int) ([]api.Run, error) {
	const q = `
		SELECT id, job_name, status, params, created_at, updated_at
		FROM runs
		WHERE status IN ('Succeeded', 'Failed', 'Cancelled')
		  AND id NOT IN (SELECT run_id FROM run_log_archives)
		ORDER BY updated_at
		LIMIT $1;
	`
	rows, err := p.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Run
	for rows.Next() {
		var r api.Run
		var status string
		var paramsOut []byte
		if err := rows.Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Status = api.RunStatus(status)
		_ = json.Unmarshal(paramsOut, &r.Params)
		if r.Params == nil {
			r.Params = map[string]string{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) CreateLogArchive(ctx context.Context, runID, objectKey string, sizeBytes int64) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO run_log_archives(run_id, object_key, size_bytes)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (run_id) DO UPDATE
		   SET object_key = EXCLUDED.object_key, size_bytes = EXCLUDED.size_bytes, archived_at = NOW()`,
		runID, objectKey, sizeBytes)
	return err
}

func (p *Postgres) GetLogArchive(ctx context.Context, runID string) (*LogArchive, error) {
	var a LogArchive
	err := p.pool.QueryRow(ctx,
		`SELECT run_id, object_key, size_bytes, archived_at FROM run_log_archives WHERE run_id = $1`,
		runID).Scan(&a.RunID, &a.ObjectKey, &a.SizeBytes, &a.ArchivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (p *Postgres) ListenForNotify(ctx context.Context, channel string, callback func(payload string)) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+sanitizeChannel(channel)); err != nil {
		return fmt.Errorf("listen %q: %w", channel, err)
	}

	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err // returns context.Canceled when ctx is cancelled
		}
		callback(n.Payload)
	}
}

// sanitizeChannel quotes a channel name for safe use in a LISTEN statement.
func sanitizeChannel(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func (p *Postgres) UpsertSecret(ctx context.Context, name, scope, scopeRef string, encryptedDEK, ciphertext []byte) (*StoredSecret, error) {
	const q = `
		INSERT INTO secrets(name, scope, scope_ref, encrypted_dek, ciphertext, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (name, scope, scope_ref) DO UPDATE
		  SET encrypted_dek = EXCLUDED.encrypted_dek,
		      ciphertext     = EXCLUDED.ciphertext,
		      updated_at     = NOW()
		RETURNING id, name, scope, scope_ref, encrypted_dek, ciphertext, created_at, updated_at;
	`
	var s StoredSecret
	err := p.pool.QueryRow(ctx, q, name, scope, scopeRef, encryptedDEK, ciphertext).
		Scan(&s.ID, &s.Name, &s.Scope, &s.ScopeRef, &s.EncryptedDEK, &s.Ciphertext, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert secret: %w", err)
	}
	return &s, nil
}

func (p *Postgres) GetSecret(ctx context.Context, name, scope, scopeRef string) (*StoredSecret, error) {
	const q = `SELECT id, name, scope, scope_ref, encrypted_dek, ciphertext, created_at, updated_at
		FROM secrets WHERE name = $1 AND scope = $2 AND scope_ref = $3`
	var s StoredSecret
	err := p.pool.QueryRow(ctx, q, name, scope, scopeRef).
		Scan(&s.ID, &s.Name, &s.Scope, &s.ScopeRef, &s.EncryptedDEK, &s.Ciphertext, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get secret %q: %w", name, err)
	}
	return &s, nil
}

func (p *Postgres) ListSecrets(ctx context.Context, scope, scopeRef string) ([]SecretMeta, error) {
	const q = `SELECT id, name, scope, scope_ref, created_at FROM secrets
		WHERE scope = $1 AND scope_ref = $2 ORDER BY name`
	rows, err := p.pool.Query(ctx, q, scope, scopeRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretMeta
	for rows.Next() {
		var m SecretMeta
		if err := rows.Scan(&m.ID, &m.Name, &m.Scope, &m.ScopeRef, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteSecret(ctx context.Context, name, scope, scopeRef string) error {
	_, err := p.pool.Exec(ctx,
		`DELETE FROM secrets WHERE name = $1 AND scope = $2 AND scope_ref = $3`,
		name, scope, scopeRef)
	return err
}

// CreatePAT creates a Personal Access Token and returns its metadata.
func (p *Postgres) CreatePAT(ctx context.Context, name, tokenHash string, expiresAt *time.Time) (*PAT, error) {
	const q = `INSERT INTO pats(name, token_hash, expires_at) VALUES ($1, $2, $3)
		RETURNING id, name, created_at, expires_at, last_used_at;`
	var pat PAT
	err := p.pool.QueryRow(ctx, q, name, tokenHash, expiresAt).
		Scan(&pat.ID, &pat.Name, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt)
	if err != nil {
		return nil, fmt.Errorf("create pat: %w", err)
	}
	return &pat, nil
}

// UpsertBootstrapPAT updates the hash of an existing row identified by name, or creates a new one.
// It is used to sync the UNIFIED_TOKEN value into the PAT table on each startup, ensuring
// that at most one row exists per name (since token_hash changes every time the value changes,
// the row must be identified uniquely by name).
func (p *Postgres) UpsertBootstrapPAT(ctx context.Context, name, tokenHash string) (*PAT, error) {
	const updateQ = `UPDATE pats SET token_hash = $2 WHERE name = $1
		RETURNING id, name, created_at, expires_at, last_used_at;`
	var pat PAT
	err := p.pool.QueryRow(ctx, updateQ, name, tokenHash).
		Scan(&pat.ID, &pat.Name, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt)
	if err == nil {
		return &pat, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("rotate bootstrap pat: %w", err)
	}

	const insertQ = `INSERT INTO pats(name, token_hash) VALUES ($1, $2)
		RETURNING id, name, created_at, expires_at, last_used_at;`
	if err := p.pool.QueryRow(ctx, insertQ, name, tokenHash).
		Scan(&pat.ID, &pat.Name, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt); err != nil {
		return nil, fmt.Errorf("create bootstrap pat: %w", err)
	}
	return &pat, nil
}

// DeleteBootstrapPATByName deletes the PAT row identified by name (no-op if it does not exist).
func (p *Postgres) DeleteBootstrapPATByName(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM pats WHERE name = $1`, name)
	return err
}

// GetPATByHash retrieves a PAT by token_hash (expired tokens are excluded).
func (p *Postgres) GetPATByHash(ctx context.Context, tokenHash string) (*PAT, error) {
	const q = `SELECT id, name, created_at, expires_at, last_used_at FROM pats
		WHERE token_hash = $1 AND (expires_at IS NULL OR expires_at > NOW())`
	var pat PAT
	err := p.pool.QueryRow(ctx, q, tokenHash).
		Scan(&pat.ID, &pat.Name, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt)
	if err != nil {
		return nil, fmt.Errorf("get pat: %w", err)
	}
	return &pat, nil
}

// ListPATs returns all PATs ordered by creation date ascending.
func (p *Postgres) ListPATs(ctx context.Context) ([]PAT, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, name, created_at, expires_at, last_used_at FROM pats ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PAT
	for rows.Next() {
		var pat PAT
		if err := rows.Scan(&pat.ID, &pat.Name, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, pat)
	}
	return out, rows.Err()
}

// DeletePAT deletes a PAT by ID.
func (p *Postgres) DeletePAT(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM pats WHERE id = $1`, id)
	return err
}

// TouchPAT updates the last_used_at of a PAT to the current time.
func (p *Postgres) TouchPAT(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `UPDATE pats SET last_used_at = NOW() WHERE id = $1`, id)
	return err
}

// UpsertWebhookReceiver creates or updates a WebhookReceiver and returns it.
func (p *Postgres) UpsertWebhookReceiver(ctx context.Context, name string, spec []byte) (*WebhookReceiver, error) {
	const q = `INSERT INTO webhook_receivers(name, spec) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET spec = EXCLUDED.spec, updated_at = NOW()
		RETURNING id, name, spec, updated_at;`
	var wr WebhookReceiver
	err := p.pool.QueryRow(ctx, q, name, spec).Scan(&wr.ID, &wr.Name, &wr.Spec, &wr.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert webhook receiver: %w", err)
	}
	return &wr, nil
}

// GetWebhookReceiver retrieves a WebhookReceiver by name.
func (p *Postgres) GetWebhookReceiver(ctx context.Context, name string) (*WebhookReceiver, error) {
	const q = `SELECT id, name, spec, updated_at FROM webhook_receivers WHERE name = $1`
	var wr WebhookReceiver
	err := p.pool.QueryRow(ctx, q, name).Scan(&wr.ID, &wr.Name, &wr.Spec, &wr.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get webhook receiver %q: %w", name, err)
	}
	return &wr, nil
}

// ListWebhookReceivers returns all WebhookReceivers ordered by name ascending.
func (p *Postgres) ListWebhookReceivers(ctx context.Context) ([]WebhookReceiver, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, name, spec, updated_at FROM webhook_receivers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookReceiver
	for rows.Next() {
		var wr WebhookReceiver
		if err := rows.Scan(&wr.ID, &wr.Name, &wr.Spec, &wr.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, wr)
	}
	return out, rows.Err()
}

// DeleteWebhookReceiver deletes a WebhookReceiver by name.
func (p *Postgres) DeleteWebhookReceiver(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM webhook_receivers WHERE name = $1`, name)
	return err
}

// CreateOIDCState saves an OIDC flow state to the database.
func (p *Postgres) CreateOIDCState(ctx context.Context, state, redirectTo string, expiresAt time.Time) (*OIDCState, error) {
	const q = `INSERT INTO oidc_states(state, redirect_to, expires_at)
		VALUES ($1, $2, $3)
		RETURNING id, state, redirect_to, created_at, expires_at`
	var s OIDCState
	err := p.pool.QueryRow(ctx, q, state, redirectTo, expiresAt).
		Scan(&s.ID, &s.State, &s.RedirectTo, &s.CreatedAt, &s.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("create oidc state: %w", err)
	}
	return &s, nil
}

// GetAndDeleteOIDCState retrieves and deletes a state (used during callback handling).
// Returns nil, nil if the state is not found or has expired.
func (p *Postgres) GetAndDeleteOIDCState(ctx context.Context, state string) (*OIDCState, error) {
	const q = `DELETE FROM oidc_states WHERE state = $1 AND expires_at > NOW()
		RETURNING id, state, redirect_to, created_at, expires_at`
	var s OIDCState
	err := p.pool.QueryRow(ctx, q, state).
		Scan(&s.ID, &s.State, &s.RedirectTo, &s.CreatedAt, &s.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get and delete oidc state: %w", err)
	}
	return &s, nil
}

// DeleteExpiredOIDCStates deletes expired oidc_states rows.
func (p *Postgres) DeleteExpiredOIDCStates(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM oidc_states WHERE expires_at <= NOW()`)
	return err
}

// CreateSession saves a browser session to the database.
func (p *Postgres) CreateSession(ctx context.Context, tokenHash, sub, email, encryptedRefreshToken string, expiresAt time.Time) (*Session, error) {
	const q = `INSERT INTO sessions(token_hash, sub, email, refresh_token, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, token_hash, sub, email, refresh_token, expires_at, last_used_at, created_at`
	var s Session
	err := p.pool.QueryRow(ctx, q, tokenHash, sub, email, encryptedRefreshToken, expiresAt).
		Scan(&s.ID, &s.TokenHash, &s.Sub, &s.Email, &s.RefreshToken, &s.ExpiresAt, &s.LastUsedAt, &s.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &s, nil
}

// GetSessionByHash retrieves a session by token_hash.
func (p *Postgres) GetSessionByHash(ctx context.Context, tokenHash string) (*Session, error) {
	const q = `SELECT id, token_hash, sub, email, refresh_token, expires_at, last_used_at, created_at
		FROM sessions WHERE token_hash = $1`
	var s Session
	err := p.pool.QueryRow(ctx, q, tokenHash).
		Scan(&s.ID, &s.TokenHash, &s.Sub, &s.Email, &s.RefreshToken, &s.ExpiresAt, &s.LastUsedAt, &s.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get session: %w", err)
	}
	return &s, nil
}

// UpdateSessionExpiry updates the expiry time and refresh token of a session.
func (p *Postgres) UpdateSessionExpiry(ctx context.Context, id, encryptedRefreshToken string, expiresAt time.Time) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE sessions SET expires_at = $1, refresh_token = $2 WHERE id = $3`,
		expiresAt, encryptedRefreshToken, id)
	return err
}

// DeleteSession deletes a session by ID.
func (p *Postgres) DeleteSession(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

// TouchSession updates the last_used_at of a session to the current time.
func (p *Postgres) TouchSession(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `UPDATE sessions SET last_used_at = NOW() WHERE id = $1`, id)
	return err
}

func (p *Postgres) UpsertGitCredential(ctx context.Context, name, host, credType, secretRef string) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO git_credentials(name, host, cred_type, secret_ref)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE
		SET host = EXCLUDED.host,
		    cred_type = EXCLUDED.cred_type,
		    secret_ref = EXCLUDED.secret_ref,
		    updated_at = NOW()`,
		name, host, credType, secretRef)
	return err
}

func (p *Postgres) GetGitCredentialByHost(ctx context.Context, host string) (*GitCredential, error) {
	var gc GitCredential
	err := p.pool.QueryRow(ctx,
		`SELECT id, name, host, cred_type, secret_ref, created_at, updated_at
		 FROM git_credentials WHERE host = $1 ORDER BY updated_at DESC LIMIT 1`,
		host).Scan(&gc.ID, &gc.Name, &gc.Host, &gc.CredType, &gc.SecretRef, &gc.CreatedAt, &gc.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &gc, nil
}

func (p *Postgres) ListGitCredentials(ctx context.Context) ([]GitCredential, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, host, cred_type, secret_ref, created_at, updated_at
		 FROM git_credentials ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []GitCredential
	for rows.Next() {
		var gc GitCredential
		if err := rows.Scan(&gc.ID, &gc.Name, &gc.Host, &gc.CredType, &gc.SecretRef, &gc.CreatedAt, &gc.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, gc)
	}
	return list, rows.Err()
}

func (p *Postgres) DeleteGitCredential(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM git_credentials WHERE name = $1`, name)
	return err
}

// EnsureControllerKey tries to store candidateHex in the single row of controller_settings.
// If a row already exists it does nothing and returns the existing value
// (safe against simultaneous first-startup from multiple replicas).
func (p *Postgres) EnsureControllerKey(ctx context.Context, candidateHex string) (string, error) {
	var keyHex string
	err := p.pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO controller_settings (id, controller_key_hex)
			VALUES (1, $1)
			ON CONFLICT (id) DO NOTHING
			RETURNING controller_key_hex
		)
		SELECT controller_key_hex FROM ins
		UNION ALL
		SELECT controller_key_hex FROM controller_settings WHERE id = 1
		LIMIT 1`,
		candidateHex).Scan(&keyHex)
	if err != nil {
		return "", err
	}
	return keyHex, nil
}

func (p *Postgres) ListPendingRuns(ctx context.Context, limit int) ([]PendingRun, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, spec FROM runs WHERE status = 'Pending' ORDER BY created_at LIMIT $1`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []PendingRun
	for rows.Next() {
		var r PendingRun
		if err := rows.Scan(&r.ID, &r.Spec); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

func (p *Postgres) UpdateRunSpec(ctx context.Context, runID string, specJSON []byte) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE runs SET spec = $1, updated_at = NOW() WHERE id = $2 AND status = 'Pending'`,
		specJSON, runID)
	return err
}

// scanSchedule reads a Schedule from a row that implements the Scan interface.
func scanSchedule(row interface{ Scan(...any) error }) (*Schedule, error) {
	var s Schedule
	var paramsJSON []byte
	if err := row.Scan(&s.Name, &s.Cron, &s.JobName, &paramsJSON, &s.LastFiredAt, &s.UpdatedAt); err != nil {
		return nil, fmt.Errorf("scan schedule: %w", err)
	}
	s.Params = map[string]string{}
	if len(paramsJSON) > 0 {
		if err := json.Unmarshal(paramsJSON, &s.Params); err != nil {
			return nil, fmt.Errorf("unmarshal schedule params: %w", err)
		}
	}
	return &s, nil
}

// UpsertSchedule creates or updates a schedule.
func (p *Postgres) UpsertSchedule(ctx context.Context, name, cron, jobName string, params map[string]string) (*Schedule, error) {
	if params == nil {
		params = map[string]string{}
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal schedule params: %w", err)
	}
	const q = `INSERT INTO schedules(name, cron, job_name, params)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO UPDATE SET cron = EXCLUDED.cron, job_name = EXCLUDED.job_name, params = EXCLUDED.params, updated_at = NOW()
		RETURNING name, cron, job_name, params, last_fired_at, updated_at`
	return scanSchedule(p.pool.QueryRow(ctx, q, name, cron, jobName, paramsJSON))
}

// GetSchedule retrieves a schedule by name.
func (p *Postgres) GetSchedule(ctx context.Context, name string) (*Schedule, error) {
	const q = `SELECT name, cron, job_name, params, last_fired_at, updated_at FROM schedules WHERE name = $1`
	s, err := scanSchedule(p.pool.QueryRow(ctx, q, name))
	if err != nil {
		return nil, fmt.Errorf("get schedule name=%s: %w", name, err)
	}
	return s, nil
}

// ListSchedules returns all schedules ordered by name.
func (p *Postgres) ListSchedules(ctx context.Context) ([]Schedule, error) {
	const q = `SELECT name, cron, job_name, params, last_fired_at, updated_at FROM schedules ORDER BY name`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// DeleteSchedule deletes a schedule by name.
func (p *Postgres) DeleteSchedule(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM schedules WHERE name = $1`, name)
	return err
}

// UpdateScheduleLastFiredAt updates the last fired time of a schedule.
func (p *Postgres) UpdateScheduleLastFiredAt(ctx context.Context, name string, firedAt time.Time) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE schedules SET last_fired_at = $1, updated_at = NOW() WHERE name = $2`,
		firedAt, name)
	return err
}

// DeleteJob deletes a job by name.
func (p *Postgres) DeleteJob(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM jobs WHERE name = $1`, name)
	return err
}

// UpsertAppSource creates or updates an AppSource. On update, last_commit is reset.
func (p *Postgres) UpsertAppSource(ctx context.Context, name string, spec []byte) (*AppSource, error) {
	const q = `
		INSERT INTO app_sources(name, spec)
		VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE
		  SET spec = EXCLUDED.spec,
		      last_commit = '',
		      updated_at = NOW()
		RETURNING name, spec, last_synced_at, last_commit, managed_jobs, updated_at`
	var a AppSource
	var managedJobs []string
	err := p.pool.QueryRow(ctx, q, name, spec).
		Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &managedJobs, &a.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert AppSource: %w", err)
	}
	a.ManagedJobs = managedJobs
	return &a, nil
}

// GetAppSource retrieves an AppSource by name.
func (p *Postgres) GetAppSource(ctx context.Context, name string) (*AppSource, error) {
	const q = `SELECT name, spec, last_synced_at, last_commit, managed_jobs, updated_at FROM app_sources WHERE name = $1`
	var a AppSource
	var managedJobs []string
	err := p.pool.QueryRow(ctx, q, name).
		Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &managedJobs, &a.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get AppSource name=%s: %w", name, err)
	}
	a.ManagedJobs = managedJobs
	return &a, nil
}

// ListAppSources returns all AppSources ordered by name.
func (p *Postgres) ListAppSources(ctx context.Context) ([]AppSource, error) {
	const q = `SELECT name, spec, last_synced_at, last_commit, managed_jobs, updated_at FROM app_sources ORDER BY name`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppSource
	for rows.Next() {
		var a AppSource
		var managedJobs []string
		if err := rows.Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &managedJobs, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.ManagedJobs = managedJobs
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAppSource deletes an AppSource by name.
func (p *Postgres) DeleteAppSource(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM app_sources WHERE name = $1`, name)
	return err
}

// UpdateAppSourceSyncState updates the sync state of an AppSource (last commit, sync time, and managed job list).
func (p *Postgres) UpdateAppSourceSyncState(ctx context.Context, name, lastCommit string, syncedAt time.Time, managedJobs []string) error {
	if managedJobs == nil {
		managedJobs = []string{}
	}
	_, err := p.pool.Exec(ctx,
		`UPDATE app_sources SET last_commit = $1, last_synced_at = $2, managed_jobs = $3, updated_at = NOW() WHERE name = $4`,
		lastCommit, syncedAt, managedJobs, name)
	return err
}

// ResetAppSourceCommit resets the last_commit of an AppSource to an empty string.
func (p *Postgres) ResetAppSourceCommit(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE app_sources SET last_commit = '', updated_at = NOW() WHERE name = $1`,
		name)
	return err
}

// ---- Approvals ----

func (p *Postgres) CreatePendingApproval(ctx context.Context, runID string, stepIndex int, stepName, message string, timeoutAt *time.Time) error {
	const q = `
		INSERT INTO run_approvals(run_id, step_index, step_name, message, status, timeout_at)
		VALUES ($1, $2, $3, $4, 'Pending', $5)
		ON CONFLICT (run_id, step_index) DO NOTHING;
	`
	_, err := p.pool.Exec(ctx, q, runID, stepIndex, stepName, message, timeoutAt)
	return err
}

func (p *Postgres) DecideApproval(ctx context.Context, runID string, stepIndex int, status, decidedBy, comment string) (bool, error) {
	const q = `
		UPDATE run_approvals
		SET status = $3, decided_by = $4, comment = $5, decided_at = now()
		WHERE run_id = $1 AND step_index = $2 AND status = 'Pending';
	`
	tag, err := p.pool.Exec(ctx, q, runID, stepIndex, status, decidedBy, comment)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (p *Postgres) GetApproval(ctx context.Context, runID string, stepIndex int) (api.RunApproval, error) {
	const q = `
		SELECT run_id, step_index, step_name, message, status, decided_by, comment, created_at, timeout_at, decided_at
		FROM run_approvals WHERE run_id = $1 AND step_index = $2;
	`
	var a api.RunApproval
	var decidedBy, comment *string
	err := p.pool.QueryRow(ctx, q, runID, stepIndex).Scan(
		&a.RunID, &a.StepIndex, &a.StepName, &a.Message, &a.Status, &decidedBy, &comment, &a.CreatedAt, &a.TimeoutAt, &a.DecidedAt)
	if err != nil {
		return api.RunApproval{}, err
	}
	if decidedBy != nil {
		a.DecidedBy = *decidedBy
	}
	if comment != nil {
		a.Comment = *comment
	}
	return a, nil
}

func (p *Postgres) ListRunApprovals(ctx context.Context, runID string) ([]api.RunApproval, error) {
	const q = `
		SELECT run_id, step_index, step_name, message, status, decided_by, comment, created_at, timeout_at, decided_at
		FROM run_approvals WHERE run_id = $1 ORDER BY step_index;
	`
	rows, err := p.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.RunApproval
	for rows.Next() {
		var a api.RunApproval
		var decidedBy, comment *string
		if err := rows.Scan(&a.RunID, &a.StepIndex, &a.StepName, &a.Message, &a.Status, &decidedBy, &comment, &a.CreatedAt, &a.TimeoutAt, &a.DecidedAt); err != nil {
			return nil, err
		}
		if decidedBy != nil {
			a.DecidedBy = *decidedBy
		}
		if comment != nil {
			a.Comment = *comment
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
