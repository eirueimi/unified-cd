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

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/golang-migrate/migrate/v4"
	migpostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Postgres struct {
	pool           *pgxpool.Pool
	backgroundPool *pgxpool.Pool
	lockPool       *pgxpool.Pool
	listenPool     *pgxpool.Pool
	ownsPools      bool
	closeOnce      *sync.Once
}

// PostgresPoolConfig bounds each independently reserved controller pool.
type PostgresPoolConfig struct {
	APIMaxConns        int32
	BackgroundMaxConns int32
	LockMaxConns       int32
	ListenMaxConns     int32
}

// DefaultPostgresPoolConfig returns the controller's production pool limits.
func DefaultPostgresPoolConfig() PostgresPoolConfig {
	return PostgresPoolConfig{
		APIMaxConns:        128,
		BackgroundMaxConns: 32,
		LockMaxConns:       16,
		ListenMaxConns:     128,
	}
}

// listenAcquireTimeout bounds how long ListenForNotify waits to acquire a
// listenPool connection, independently of the caller's context. Without this,
// Acquire(ctx) blocks on ctx alone — and for the SSE handler, ctx lives as
// long as the viewer's browser tab stays open. Past ListenMaxConns concurrent
// viewers of the SAME run's SSE endpoint, the (N+1)th connects, gets a 200,
// and then simply never receives another byte: nothing ever fails, so
// nothing ever surfaces an error — the acquire just sits "in progress" until
// the viewer gives up. This bound is deliberately about the pool having ANY
// free connection, not about getting useful work done once one is acquired:
// failure to acquire within a few seconds means the pool is saturated, not
// merely busy, and the caller deserves an answer instead of an indefinite
// wait. 5s matches the timeout AcquireAdvisoryLock already uses for its
// unlock path below — long enough to ride out a burst of concurrent
// reconnects competing for the same pool, short enough that genuine
// exhaustion fails within one request's worth of patience.
//
// pgxpool has no separate "max acquire wait" config to reach for instead:
// unlike database/sql-style pools, pgxpool.Config carries no MaxConnWaitTime
// field, and Pool.Acquire(ctx) delegates straight to puddle/v2's
// Pool.Acquire(ctx), which purely watches ctx.Done(). A wrapping
// context.WithTimeout is therefore the only lever here, not a workaround for
// a better one.
//
// This does not need a new metric: puddle's Acquire increments
// canceledAcquireCount from a bare <-ctx.Done() case with no distinction
// between context.Canceled and context.DeadlineExceeded, so a timeout here
// is already counted by the existing
// unifiedcd_db_pool_canceled_acquires_total{pool="listen"} (see
// internal/metrics/pool_collector.go, PR #151).
const listenAcquireTimeout = 5 * time.Second

func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	return NewPostgresWithPoolConfig(ctx, dsn, DefaultPostgresPoolConfig())
}

// NewPostgresWithPoolConfig creates isolated API, background, lock, and listen pools.
func NewPostgresWithPoolConfig(ctx context.Context, dsn string, cfg PostgresPoolConfig) (*Postgres, error) {
	type poolSpec struct {
		name string
		max  int32
		dst  **pgxpool.Pool
	}
	p := &Postgres{ownsPools: true, closeOnce: &sync.Once{}}
	specs := []poolSpec{
		{name: "api", max: cfg.APIMaxConns, dst: &p.pool},
		{name: "background", max: cfg.BackgroundMaxConns, dst: &p.backgroundPool},
		{name: "lock", max: cfg.LockMaxConns, dst: &p.lockPool},
		{name: "listen", max: cfg.ListenMaxConns, dst: &p.listenPool},
	}
	for _, spec := range specs {
		pool, err := newPostgresPool(ctx, dsn, spec.name, spec.max)
		if err != nil {
			p.Close()
			return nil, err
		}
		*spec.dst = pool
	}
	for _, spec := range specs {
		if err := (*spec.dst).Ping(ctx); err != nil {
			p.Close()
			return nil, fmt.Errorf("ping postgres %s pool: %w", spec.name, err)
		}
	}
	return p, nil
}

func newPostgresPool(ctx context.Context, dsn, name string, maxConns int32) (*pgxpool.Pool, error) {
	if maxConns <= 0 {
		return nil, fmt.Errorf("postgres %s pool max connections must be positive", name)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres %s pool config: %w", name, err)
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres %s pool: %w", name, err)
	}
	return pool, nil
}

// BackgroundStore returns a non-owning view whose ordinary queries use the
// background pool. Advisory locks and notification listeners remain isolated
// in their dedicated pools.
func (p *Postgres) BackgroundStore() *Postgres {
	return &Postgres{
		pool:           p.backgroundPool,
		backgroundPool: p.backgroundPool,
		lockPool:       p.lockPool,
		listenPool:     p.listenPool,
	}
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
	// Guard against migration-renumbering drift: version numbers alone can
	// claim a file is applied when its schema objects never materialized.
	return verifySchema(db)
}

func (p *Postgres) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

func (p *Postgres) Close() {
	if !p.ownsPools || p.closeOnce == nil {
		return
	}
	p.closeOnce.Do(func() {
		for _, pool := range []*pgxpool.Pool{p.pool, p.backgroundPool, p.lockPool, p.listenPool} {
			if pool != nil {
				pool.Close()
			}
		}
	})
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
func (p *Postgres) CreateRun(ctx context.Context, jobName string, params map[string]string, spec []byte, agentSelector []string, requiredCaps []string, triggeredBy string, displayName string) (*api.Run, error) {
	if params == nil {
		params = map[string]string{}
	}
	if agentSelector == nil {
		agentSelector = []string{}
	}
	if requiredCaps == nil {
		requiredCaps = []string{}
	}
	if triggeredBy == "" {
		triggeredBy = "api"
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	// Derive the detached flag from the run's spec so ClaimNextRun can filter on
	// a persisted column without re-parsing the spec on the hot claim path.
	var detached bool
	if len(spec) > 0 {
		var s dsl.Spec
		if json.Unmarshal(spec, &s) == nil {
			detached = s.Detached
		}
	}
	const q = `
		INSERT INTO runs(job_name, params, spec, agent_selector, required_caps, triggered_by, detached, display_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
		RETURNING id, job_name, status, params, created_at, updated_at, triggered_by, display_name;
	`
	var r api.Run
	var paramsOut []byte
	var status string
	var displayNameOut *string
	err = p.pool.QueryRow(ctx, q, jobName, paramsJSON, spec, agentSelector, requiredCaps, triggeredBy, detached, displayName).
		Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy, &displayNameOut)
	if err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}
	r.Status = api.RunStatus(status)
	_ = json.Unmarshal(paramsOut, &r.Params)
	if r.Params == nil {
		r.Params = map[string]string{}
	}
	if displayNameOut != nil {
		r.DisplayName = *displayNameOut
	}
	return &r, nil
}

// ListRunningRunIDsByAgent returns IDs of Running runs claimed by agentID.
// Used by the agent-orphan recovery paths (startup reconcile / force
// shutdown) to find runs a dead agent process left behind.
func (p *Postgres) ListRunningRunIDsByAgent(ctx context.Context, agentID string) ([]string, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id FROM runs WHERE claimed_by = $1 AND status = 'Running'`, agentID)
	if err != nil {
		return nil, fmt.Errorf("list running runs by agent: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListReconcilableRunIDsByAgent returns IDs of Running runs claimed by
// agentID whose claimed_at is older than grace. Used by the heartbeat
// reconcile path to fail runs the agent no longer reports as active, while
// excluding a run claimed within the grace window so a fresh claim racing
// the agent's next heartbeat is never mistaken for an orphan.
func (p *Postgres) ListReconcilableRunIDsByAgent(ctx context.Context, agentID string, grace time.Duration) ([]string, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id FROM runs WHERE claimed_by = $1 AND status = 'Running' AND claimed_at < NOW() - make_interval(secs => $2)`,
		agentID, grace.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list reconcilable runs by agent: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListChildRunIDs returns the IDs of runs directly spawned by parentRunID via
// call: steps, read from the step reports that recorded each child_run_id.
func (p *Postgres) ListChildRunIDs(ctx context.Context, parentRunID string) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT child_run_id::text FROM step_reports WHERE run_id = $1 AND child_run_id IS NOT NULL`, parentRunID)
	if err != nil {
		return nil, fmt.Errorf("list child runs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan child run id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ErrRunNotFound is returned by GetRun when no run exists with the requested ID.
// It is distinct from transient database errors (pool exhaustion, timeouts,
// dropped connections) so callers can map a genuine "not found" to HTTP 404
// while surfacing infrastructure failures as 5xx. Match it with errors.Is.
var ErrRunNotFound = errors.New("run not found")

// ErrArchiveIncomplete is returned by TrimRunLogs when the run's logs table
// holds more rows (or a higher seq) than the archive record claims to cover
// — either the run's log count exceeded the archiver's TailLogs cap, or
// lines were appended after the archive was written. Trimming would destroy
// data no archive has a copy of, so TrimRunLogs rolls back and leaves the
// logs rows and trimmed_at untouched. Match it with errors.Is.
var ErrArchiveIncomplete = errors.New("archive does not cover all of the run's logs")

func (p *Postgres) GetRun(ctx context.Context, id string) (*api.Run, error) {
	const q = `SELECT id, job_name, status, params, created_at, updated_at, triggered_by, claimed_by, display_name FROM runs WHERE id = $1`
	var r api.Run
	var paramsOut []byte
	var status string
	var claimedBy *string
	var displayName *string
	err := p.pool.QueryRow(ctx, q, id).
		Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy, &claimedBy, &displayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	r.Status = api.RunStatus(status)
	if claimedBy != nil {
		r.ClaimedBy = *claimedBy
	}
	if displayName != nil {
		r.DisplayName = *displayName
	}
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
		SELECT id, job_name, status, params, created_at, updated_at, triggered_by, display_name
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
		var displayName *string
		if err := rows.Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy, &displayName); err != nil {
			return nil, err
		}
		r.Status = api.RunStatus(status)
		_ = json.Unmarshal(paramsOut, &r.Params)
		if r.Params == nil {
			r.Params = map[string]string{}
		}
		if displayName != nil {
			r.DisplayName = *displayName
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) ListActiveRuns(ctx context.Context) ([]api.Run, error) {
	const q = `
		SELECT id, job_name, status, params, created_at, updated_at, triggered_by, display_name
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
		var displayName *string
		if err := rows.Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy, &displayName); err != nil {
			return nil, err
		}
		r.Status = api.RunStatus(status)
		_ = json.Unmarshal(paramsOut, &r.Params)
		if r.Params == nil {
			r.Params = map[string]string{}
		}
		if displayName != nil {
			r.DisplayName = *displayName
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TransitionPendingToQueued examines the limit oldest Pending runs and queues
// those whose concurrency constraints allow it. It always starts at the head of
// the Pending queue; callers that must guarantee every Pending run is eventually
// examined should use TransitionPendingToQueuedFrom instead.
func (p *Postgres) TransitionPendingToQueued(ctx context.Context, limit int) (int, error) {
	n, _, err := p.TransitionPendingToQueuedFrom(ctx, limit, nil)
	return n, err
}

// TransitionPendingToQueuedFrom examines up to limit Pending runs starting after
// the cursor, and returns the cursor to resume from on the next call.
//
// The cursor exists because the window is a FIXED size over an ORDERED set, and a
// Pending run that cannot be queued STAYS Pending: runs blocked on a held mutex are
// never written at all (tryQueueRun rolls back and leaves the status untouched), so
// they sit at the head of created_at order and re-fill the window on every tick,
// forever. Past the window size, a later run that declares no concurrency at all is
// then never EXAMINED — not deferred, not queued behind anything, simply never
// looked at — for as long as the blocked head persists (measured: 252.6s and 787.6s
// against a 0.185s baseline, with an idle agent long-polling for work throughout).
//
// next is non-nil when the batch came back full, i.e. there may be more Pending runs
// after it; the caller should pass it back to advance past the runs it just examined.
// A short batch means the end of the Pending set was reached and next is nil, so the
// caller wraps to the head. Every Pending run is therefore examined within
// ceil(N/limit) ticks, at unchanged per-tick cost — the fix is that the window
// MOVES, not that it grew.
func (p *Postgres) TransitionPendingToQueuedFrom(ctx context.Context, limit int, after *PendingCursor) (queued int, next *PendingCursor, err error) {
	var cursorTime time.Time
	var cursorID string
	if after != nil {
		cursorTime = after.CreatedAt
		cursorID = after.ID
	}
	// Keyset pagination on (created_at, id) rather than OFFSET: rows leave the
	// Pending set under the scan, and OFFSET would silently skip their successors.
	// The tuple comparison also breaks created_at ties deterministically.
	rows, err := p.pool.Query(ctx,
		`SELECT id, spec, params, created_at FROM runs
		 WHERE status = 'Pending'
		   AND ($2::timestamptz IS NULL OR (created_at, id) > ($2::timestamptz, $3::uuid))
		 ORDER BY created_at, id LIMIT $1`,
		limit, nullableTime(cursorTime), nullableUUID(cursorID))
	if err != nil {
		return 0, nil, err
	}
	type candidate struct {
		id        string
		spec      []byte
		params    []byte
		createdAt time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.spec, &c.params, &c.createdAt); err != nil {
			rows.Close()
			return 0, nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	if len(candidates) == limit && limit > 0 {
		last := candidates[len(candidates)-1]
		next = &PendingCursor{CreatedAt: last.createdAt, ID: last.id}
	}

	count := 0
	for _, c := range candidates {
		queued, err := p.tryQueueRun(ctx, c.id, c.spec, c.params)
		if err != nil {
			return count, next, err
		}
		if queued {
			count++
		}
	}
	return count, next, nil
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nullableUUID(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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

// ClaimNextRun claims the next queued NON-detached run for the agent. Detached
// runs are claimed separately via ClaimNextRunDetached so an agent's normal and
// detached budgets do not draw from the same pool (see the detached-runs design).
func (p *Postgres) ClaimNextRun(ctx context.Context, agentID string, agentLabels []string) (*ClaimedRun, error) {
	return p.claimNextRun(ctx, agentID, agentLabels, false)
}

// ClaimNextRunDetached claims the next queued detached run for the agent.
func (p *Postgres) ClaimNextRunDetached(ctx context.Context, agentID string, agentLabels []string) (*ClaimedRun, error) {
	return p.claimNextRun(ctx, agentID, agentLabels, true)
}

func (p *Postgres) claimNextRun(ctx context.Context, agentID string, agentLabels []string, detached bool) (*ClaimedRun, error) {
	if agentLabels == nil {
		agentLabels = []string{}
	}
	// LEFT JOIN (rather than an inner join against a "me" CTE) is deliberate:
	// if the claiming agent has no row yet in `agents`, a.capabilities is NULL,
	// which falls into the "skip cap check" branch below (treated as legacy)
	// rather than an inner join silently excluding every run because the
	// agent side of the join is empty. In practice the claim handler always
	// upserts the agent before calling ClaimNextRun, but tests/callers that
	// invoke this directly for an unregistered agent ID must still fall back
	// to label-only matching.
	const q = `
		WITH picked AS (
		    SELECT r.id FROM runs r
		    LEFT JOIN agents a ON a.id = $1
		    WHERE r.status = 'Queued'
		      AND r.detached = $3
		      AND (r.agent_selector = '{}' OR r.agent_selector <@ $2::TEXT[])
		      AND (a.capabilities IS NULL OR r.required_caps <@ a.capabilities)
		    ORDER BY r.created_at
		    LIMIT 1
		    FOR UPDATE OF r SKIP LOCKED
		)
		UPDATE runs r SET claimed_by = $1, claimed_at = NOW(), updated_at = NOW(), status = 'Running'
		FROM picked WHERE r.id = picked.id
		RETURNING r.id, r.job_name, r.status, r.params, r.spec, r.created_at, r.updated_at;
	`
	var cr ClaimedRun
	var status string
	var paramsOut []byte
	err := p.pool.QueryRow(ctx, q, agentID, agentLabels, detached).
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

// RequeueClaimedRun reverses exactly the three columns claimNextRun's UPDATE
// wrote (status, claimed_by, claimed_at), so the run goes back to the queue in
// the state it was picked from. Nothing else is written between the claim and
// a caller's decision to requeue: the run's concurrency slot was taken at
// Pending -> Queued and is untouched here, its planned steps were written at
// creation, and no step has reported, so the run loses nothing by going back.
//
// The CAS on status = 'Running' AND claimed_by = the same agent means this can
// never take a run away from an agent that is already executing it, and never
// resurrects a run that was cancelled in the interim.
func (p *Postgres) RequeueClaimedRun(ctx context.Context, runID, agentID string) (bool, error) {
	ct, err := p.pool.Exec(ctx, `
		UPDATE runs SET status = 'Queued', claimed_by = NULL, claimed_at = NULL, updated_at = NOW()
		WHERE id = $1 AND status = 'Running' AND claimed_by = $2`, runID, agentID)
	if err != nil {
		return false, fmt.Errorf("requeue claimed run: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

func (p *Postgres) MarkRunRunning(ctx context.Context, runID string) error {
	_, err := p.pool.Exec(ctx, `UPDATE runs SET status = 'Running', updated_at = NOW() WHERE id = $1 AND status NOT IN ('Succeeded', 'Failed', 'Cancelled')`, runID)
	return err
}

// MarkRunFinished transitions a run to a terminal status via a CAS that refuses to
// overwrite an already-terminal run, and releases the run's mutex/semaphore locks.
// It is idempotent: a no-op (the run was already terminal) returns nil, matching the
// expectations of the schedulers/reapers that call it. Callers that need to know
// whether the run actually transitioned should use FinishRun instead.
func (p *Postgres) MarkRunFinished(ctx context.Context, runID string, status api.RunStatus) error {
	_, err := p.FinishRun(ctx, runID, status)
	return err
}

// FinishRun is like MarkRunFinished but reports whether the run actually
// transitioned to the terminal status. updated is false when the CAS matched no
// rows because the run was already terminal (e.g. the reaper already Failed it),
// so the caller can signal that the finish report was a late/no-op rather than a
// fresh success. The CAS guard (WHERE status NOT IN terminal set) is unchanged.
func (p *Postgres) FinishRun(ctx context.Context, runID string, status api.RunStatus) (updated bool, err error) {
	return p.finishRunFrom(ctx, runID, "", status)
}

// FinishRunIfStatus is FinishRun with an additional CAS on the run's CURRENT
// status: the write lands only if the run is still in expected. It exists for
// callers that decided to terminalize a run from a snapshot taken earlier and must
// not act on a run that moved since — chiefly the queued-run reaper, whose whole
// premise ("no live agent can claim this") is falsified the moment an agent claims
// it. FinishRun's own guard (NOT IN terminal) is too weak for that: it refuses to
// overwrite a TERMINAL run but happily overwrites a Running one, so a claim that
// landed milliseconds before the write is invisible to it.
//
// Returns updated=false, no error, when the run has moved on — the caller should
// treat that as "someone else is now responsible for this run", not as a failure.
func (p *Postgres) FinishRunIfStatus(ctx context.Context, runID string, expected api.RunStatus, status api.RunStatus) (updated bool, err error) {
	if expected == "" {
		return false, fmt.Errorf("expected status must not be empty")
	}
	return p.finishRunFrom(ctx, runID, expected, status)
}

// finishRunFrom terminalizes a run and releases its concurrency locks in one
// transaction. expected, when non-empty, additionally requires the run to still be
// in that status.
//
// The lock releases are GATED on the status CAS having matched a row. They used to
// be unconditional, which is the more dangerous half of a stale-snapshot write: a
// caller that failed to terminalize a run — because another writer got there first,
// or because the run had moved to a status the CAS refuses — still freed that run's
// mutex and named-lock slot out from under its live holder, letting a successor
// acquire a mutex the original holder is still executing inside. A caller that did
// not terminalize the run does not own the run's locks and must not release them.
func (p *Postgres) finishRunFrom(ctx context.Context, runID string, expected, status api.RunStatus) (updated bool, err error) {
	switch status {
	case api.RunSucceeded, api.RunFailed, api.RunCancelled:
	default:
		return false, fmt.Errorf("invalid terminal status %q", status)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE runs SET status = $1, updated_at = NOW()
WHERE id = $2 AND status NOT IN ('Succeeded', 'Failed', 'Cancelled')
  AND ($3 = '' OR status = $3)`,
		string(status), runID, string(expected))
	if err != nil {
		return false, err
	}
	updated = tag.RowsAffected() > 0
	if !updated {
		// Nothing was terminalized, so nothing is ours to release. Commit the
		// (empty) transaction rather than returning an error: "the run moved"
		// is an ordinary outcome, not a failure.
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}
	// release mutex
	if _, err := tx.Exec(ctx,
		`DELETE FROM mutex_holders WHERE run_id = $1`, runID); err != nil {
		return false, err
	}
	// release named lock slot
	if _, err := tx.Exec(ctx,
		`UPDATE named_lock_slots SET run_id = NULL, acquired_at = NULL WHERE run_id = $1`,
		runID); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return updated, nil
}

// DeleteRun deletes a Run. step_reports/logs/run_outputs etc. are cascade-deleted
// by the existing ON DELETE CASCADE constraints.
func (p *Postgres) DeleteRun(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, id)
	return err
}

func (p *Postgres) UpsertStepReport(ctx context.Context, runID string, stepIndex int, stageIndex int, stepName, variant, status string, exitCode *int, startedAt, endedAt *time.Time, childRunID, callJobName string) error {
	const q = `
		INSERT INTO step_reports(run_id, step_index, variant, stage_index, step_name, status, exit_code, started_at, ended_at, child_run_id, call_job_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10,'')::uuid, NULLIF($11,''))
		ON CONFLICT (run_id, step_index, variant) DO UPDATE
		  SET stage_index   = EXCLUDED.stage_index,
		      step_name     = EXCLUDED.step_name,
		      status        = EXCLUDED.status,
		      exit_code     = COALESCE(EXCLUDED.exit_code, step_reports.exit_code),
		      started_at    = COALESCE(EXCLUDED.started_at, step_reports.started_at),
		      ended_at      = COALESCE(EXCLUDED.ended_at, step_reports.ended_at),
		      child_run_id  = COALESCE(EXCLUDED.child_run_id, step_reports.child_run_id),
		      call_job_name = COALESCE(EXCLUDED.call_job_name, step_reports.call_job_name);
	`
	_, err := p.pool.Exec(ctx, q, runID, stepIndex, variant, stageIndex, stepName, status, exitCode, startedAt, endedAt, childRunID, callJobName)
	return err
}

// MarkRunStepsInterrupted terminalizes a reaped run's Running step reports: a
// step executing when its agent died becomes Failed, with ended_at stamped
// where not already set. Only Running is affected — not-yet-started steps have
// no step_reports row (the UI's "Pending" is filled in from the run plan), and
// terminal steps (Succeeded/Failed/Skipped/Cancelled) are left untouched.
// WaitingApproval is deliberately out of scope: it has a companion run_approvals
// row and its own controller lifecycle. Call steps whose child run is separately
// cancelled are unaffected in the UI because GetRunSteps coalesces the child's
// status over the parent step's own. Returns the number of step rows updated.
func (p *Postgres) MarkRunStepsInterrupted(ctx context.Context, runID string) (int64, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE step_reports
		   SET status   = 'Failed',
		       ended_at = COALESCE(ended_at, NOW())
		 WHERE run_id = $1
		   AND status = 'Running'`, runID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (p *Postgres) GetRunSteps(ctx context.Context, runID string) ([]api.StepReport, error) {
	const q = `
		SELECT sr.step_index, sr.stage_index, sr.step_name,
		       COALESCE(child.status, sr.status),
		       sr.exit_code, sr.started_at, sr.ended_at, sr.variant,
		       COALESCE(sr.child_run_id::text, ''), COALESCE(sr.call_job_name, '')
		FROM step_reports sr
		LEFT JOIN runs child ON child.id = sr.child_run_id
		WHERE sr.run_id = $1
		ORDER BY sr.step_index, sr.variant;
	`
	rows, err := p.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.StepReport
	for rows.Next() {
		var s api.StepReport
		if err := rows.Scan(&s.Index, &s.StageIndex, &s.Name, &s.Status, &s.ExitCode, &s.StartedAt, &s.EndedAt, &s.Variant, &s.ChildRunID, &s.CallJobName); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetRunParent returns the call step (and parent run) that launched childRunID,
// or nil if the run was not created by a call step.
func (p *Postgres) GetRunParent(ctx context.Context, childRunID string) (*api.CalledBy, error) {
	const q = `
		SELECT sr.run_id::text, r.job_name, sr.step_name
		FROM step_reports sr
		JOIN runs r ON r.id = sr.run_id
		WHERE sr.child_run_id = $1::uuid
		LIMIT 1;
	`
	var cb api.CalledBy
	err := p.pool.QueryRow(ctx, q, childRunID).Scan(&cb.ParentRunID, &cb.ParentJobName, &cb.StepName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &cb, nil
}

// UpsertSidecarStatus records a user sidecar container's phase/exit code for
// display, keyed by (runID, idx). ON CONFLICT overwrites phase/exit_code so
// the row always reflects the most recently reported transition.
func (p *Postgres) UpsertSidecarStatus(ctx context.Context, runID string, idx int, name, phase string, exitCode *int) error {
	const q = `INSERT INTO sidecar_status (run_id, idx, name, phase, exit_code, updated_at)
	           VALUES ($1,$2,$3,$4,$5, now())
	           ON CONFLICT (run_id, idx) DO UPDATE SET phase=$4, exit_code=$5, updated_at=now()`
	_, err := p.pool.Exec(ctx, q, runID, idx, name, phase, exitCode)
	return err
}

// GetSidecarStatuses returns every reported sidecar status for a run, ordered
// by idx (declared sidecar order).
func (p *Postgres) GetSidecarStatuses(ctx context.Context, runID string) ([]api.SidecarStatusRequest, error) {
	const q = `SELECT run_id, idx, name, phase, exit_code FROM sidecar_status WHERE run_id=$1 ORDER BY idx`
	rows, err := p.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.SidecarStatusRequest
	for rows.Next() {
		var s api.SidecarStatusRequest
		if err := rows.Scan(&s.RunID, &s.Index, &s.Name, &s.Phase, &s.ExitCode); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AppendLog stores one log line and notifies SSE listeners. Once the run's
// logs are archived (a run_log_archives record exists) the run is SEALED:
// the line is silently dropped and AppendLog returns (0, nil) — lines
// arriving after archival would never be captured by the archive, would
// block log trimming, and would be invisible ghost rows after a trim. The
// guard lives in the INSERT itself so the hot append path costs no extra
// round trip. Real seqs start at 1, so 0 is unambiguous.
func (p *Postgres) AppendLog(ctx context.Context, runID string, stepIndex int, stream string, ts time.Time, line string) (int64, error) {
	const q = `
		INSERT INTO logs(run_id, step_index, stream, ts, line)
		SELECT $1::uuid, $2::int, $3::text, $4::timestamptz, $5::text
		WHERE NOT EXISTS (SELECT 1 FROM run_log_archives WHERE run_id = $1::uuid)
		RETURNING seq;
	`
	var seq int64
	err := p.pool.QueryRow(ctx, q, runID, stepIndex, stream, ts, line).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // sealed: dropped
	}
	if err != nil {
		return 0, err
	}
	// notify listeners of the new log entry (skipped for dropped lines, so
	// SSE clients stay consistent with what readers can see)
	_, _ = p.pool.Exec(ctx, "SELECT pg_notify($1, $2)", "log_appended:"+runID, fmt.Sprintf("%d", seq))
	return seq, nil
}

// AppendLogs stores many log lines with a constant number of round trips per
// run, instead of AppendLog's two per line. It is the hot path for an agent's
// bulk log upload; AppendLog remains for the callers that write a single line.
//
// The batch is grouped by run because SEALING IS A PROPERTY OF THE RUN, not of
// the line: run_log_archives does not change within one statement's snapshot,
// so every line of a run in a batch shares one verdict. That makes the row
// count returned by each INSERT unambiguous — zero (the run is sealed) or all
// of them — which is what lets the returned seqs be mapped back to the input
// positionally without relying on any ordering guarantee across runs.
//
// ORDER BY ord fixes the order rows are produced, and therefore the order the
// seq sequence is drawn in, so the ascending seqs pair with that run's lines in
// input order. postgres_log_batch_test.go proves this by reading the rows back
// rather than assuming it.
//
// ALL-OR-NOTHING PER RUN, BY DESIGN: because a run's lines share one INSERT
// statement, a statement-level failure — e.g. a line containing a byte
// Postgres's text type rejects, such as an embedded NUL — loses that run's
// entire share of the batch, not just the offending line. AppendLog, by
// contrast, loses only the one line that failed; lines before and after it in
// its per-line loop are independent statements and are unaffected.
//
// This is intentional, not an oversight, and is not "fixed" by falling back
// to a per-line loop on failure: the agent's own retry queue
// (LogPusher.flushPendingLocked, internal/agent/runner.go) already resends a
// failed batch oldest-first and stops at the first failure, to preserve
// emission order. Walk the two failure modes through that retry loop, not
// through a single request, to see why they land in the same place: today,
// per-line, the lines before the poison line land, the request still 500s on
// the poison line, the agent resends the same batch, those earlier lines land
// again, duplicating them on every retry, until drop-oldest eviction discards
// the batch and a "N lines dropped" marker appears. With this all-or-nothing
// batch, nothing lands, the request 500s, the same resend loop runs for the
// same duration, and eviction produces the same marker — without the
// duplicates. The poison line wedges that run's log until eviction either
// way; that wedge is pre-existing and this change does not worsen it. It is
// strictly better on the duplicate axis and neutral on the wedge axis — for a
// single-run batch, which is every batch today (LogPusher and every other
// caller send one run per call). That analysis does not extend to a batch
// spanning multiple runs: there, a run earlier in the batch can commit before
// a later run's poison line fails the call, and the resend loop above
// duplicates that earlier run's already-committed lines on every retry. See
// the AppendLogs interface doc (store.go) for that boundary. Actually fixing
// the wedge (where to sanitize, whether to mark an
// operator-visible altered line, whether AppendLog needs the same treatment)
// is a separate, tracked decision — do not reintroduce a per-line fallback
// here to work around it; that would bring back the N round trips this
// method exists to remove, exactly when the system is already struggling.
// TestPostgres_AppendLogs_NULByte_DiffersFromAppendLog documents the measured
// difference this comment describes.
func (p *Postgres) AppendLogs(ctx context.Context, lines []LogAppend) ([]int64, error) {
	if len(lines) == 0 {
		return nil, nil
	}

	// Group line positions by run, preserving input order within each run.
	order := make([]string, 0, 4)
	byRun := make(map[string][]int, 4)
	for i, l := range lines {
		if _, ok := byRun[l.RunID]; !ok {
			order = append(order, l.RunID)
		}
		byRun[l.RunID] = append(byRun[l.RunID], i)
	}

	const q = `
		INSERT INTO logs(run_id, step_index, stream, ts, line)
		SELECT $1::uuid, s.step_index, s.stream, s.ts, s.line
		FROM unnest($2::int[], $3::text[], $4::timestamptz[], $5::text[])
			WITH ORDINALITY AS s(step_index, stream, ts, line, ord)
		WHERE NOT EXISTS (SELECT 1 FROM run_log_archives WHERE run_id = $1::uuid)
		ORDER BY s.ord
		RETURNING seq;
	`

	out := make([]int64, len(lines))
	for _, runID := range order {
		idx := byRun[runID]
		steps := make([]int32, len(idx))
		streams := make([]string, len(idx))
		stamps := make([]time.Time, len(idx))
		texts := make([]string, len(idx))
		for j, i := range idx {
			steps[j] = int32(lines[i].StepIndex)
			streams[j] = lines[i].Stream
			stamps[j] = lines[i].Timestamp
			texts[j] = lines[i].Line
		}

		rows, err := p.pool.Query(ctx, q, runID, steps, streams, stamps, texts)
		if err != nil {
			return nil, fmt.Errorf("append logs for run %s: %w", runID, err)
		}
		seqs := make([]int64, 0, len(idx))
		for rows.Next() {
			var seq int64
			if err := rows.Scan(&seq); err != nil {
				rows.Close()
				return nil, fmt.Errorf("append logs for run %s: %w", runID, err)
			}
			seqs = append(seqs, seq)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("append logs for run %s: %w", runID, err)
		}

		// Zero rows means the run is sealed and every one of its lines was
		// dropped; out[i] is already 0 for those. Any other short count would
		// mean the per-run invariant above is wrong, so say so loudly rather
		// than returning a silently misaligned mapping.
		if len(seqs) == 0 {
			continue
		}
		if len(seqs) != len(idx) {
			return nil, fmt.Errorf("append logs for run %s: inserted %d of %d lines", runID, len(seqs), len(idx))
		}
		for j, i := range idx {
			out[i] = seqs[j]
		}

		// One notification per run that had a line written. The payload keeps
		// AppendLog's shape (the highest seq); no reader parses it — the SSE
		// handler uses its own lastSeq — but changing a wire format for no
		// gain is worse than leaving it. The error is discarded deliberately,
		// exactly as in AppendLog: the lines are written, which is what
		// matters, and a failed wake-up costs a delayed refresh, not data.
		_, _ = p.pool.Exec(ctx, "SELECT pg_notify($1, $2)",
			"log_appended:"+runID, fmt.Sprintf("%d", seqs[len(seqs)-1]))
	}
	return out, nil
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

// TailLogsRecent returns up to the last `limit` log lines for the run in
// ascending seq order (the tail of the log). It selects the newest `limit` rows
// (seq DESC) then re-sorts them ascending, so a bounded backfill keeps the end
// of a huge log rather than its beginning.
func (p *Postgres) TailLogsRecent(ctx context.Context, runID string, limit int) ([]api.LogLine, error) {
	const q = `
		SELECT seq, step_index, stream, ts, line FROM (
			SELECT seq, step_index, stream, ts, line
			FROM logs WHERE run_id = $1
			ORDER BY seq DESC LIMIT $2
		) recent
		ORDER BY seq ASC;
	`
	rows, err := p.pool.Query(ctx, q, runID, limit)
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

// logsStepFilter renders the optional step_index filter shared by the
// windowed-viewer queries. steps nil/empty = no filter.
// Returns the SQL fragment and the arg (or nil).
func logsStepFilter(steps []int) (string, any) {
	if len(steps) == 0 {
		return "", nil
	}
	return " AND step_index = ANY($2)", steps
}

func (p *Postgres) CountLogs(ctx context.Context, runID string, steps []int) (count, minSeq, maxSeq int64, err error) {
	frag, arg := logsStepFilter(steps)
	q := `SELECT COUNT(*), COALESCE(MIN(seq),0), COALESCE(MAX(seq),0) FROM logs WHERE run_id = $1` + frag
	args := []any{runID}
	if arg != nil {
		args = append(args, arg)
	}
	err = p.pool.QueryRow(ctx, q, args...).Scan(&count, &minSeq, &maxSeq)
	return count, minSeq, maxSeq, err
}

func (p *Postgres) ListLogsRange(ctx context.Context, runID string, steps []int, offset, limit int) ([]api.LogLine, error) {
	frag, arg := logsStepFilter(steps)
	args := []any{runID}
	n := 2
	if arg != nil {
		args = append(args, arg)
		n = 3
	}
	q := fmt.Sprintf(`SELECT seq, step_index, stream, ts, line FROM logs
		WHERE run_id = $1%s ORDER BY seq OFFSET $%d LIMIT $%d`, frag, n, n+1)
	args = append(args, offset, limit)
	rows, err := p.pool.Query(ctx, q, args...)
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

// escapeILIKE makes q a literal ILIKE substring pattern.
func escapeILIKE(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

func (p *Postgres) SearchLogs(ctx context.Context, runID string, steps []int, q string, capN int) (int64, []LogSearchMatch, error) {
	frag, arg := logsStepFilter(steps)
	args := []any{runID}
	n := 2
	if arg != nil {
		args = append(args, arg)
		n = 3
	}
	// Row numbers are computed over the VIEW (same ordering/filter as
	// ListLogsRange) BEFORE the match filter, so they are addressable rows.
	sql := fmt.Sprintf(`
		SELECT COUNT(*) OVER (), rn - 1, seq, step_index FROM (
			SELECT seq, step_index, line, ROW_NUMBER() OVER (ORDER BY seq) AS rn
			FROM logs WHERE run_id = $1%s
		) v WHERE line ILIKE $%d ESCAPE '\' ORDER BY seq LIMIT $%d`, frag, n, n+1)
	args = append(args, escapeILIKE(q), capN)
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	var total int64
	var out []LogSearchMatch
	for rows.Next() {
		var m LogSearchMatch
		if err := rows.Scan(&total, &m.Row, &m.Seq, &m.StepIndex); err != nil {
			return 0, nil, err
		}
		out = append(out, m)
	}
	return total, out, rows.Err()
}

// UpsertAgent is the REGISTRATION path. A registration is the authoritative
// statement of an agent's identity, so on conflict it REPLACES hostname/os/labels/
// version/env wholesale rather than merging. In particular, if a label present in a
// prior registration is absent here, it is dropped — this is required so removing a
// label from an agent's config and restarting actually takes effect (see TODO #23).
// Use UpsertAgentOnClaim for the lightweight, non-destructive claim-time upsert.
func (p *Postgres) UpsertAgent(ctx context.Context, agentID, hostname, os, version string, labels []string, capabilities []string, env map[string]string) error {
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
		INSERT INTO agents(id, hostname, os, labels, version, capabilities, env, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (id) DO UPDATE
		  SET hostname     = EXCLUDED.hostname,
		      os           = EXCLUDED.os,
		      labels       = EXCLUDED.labels,
		      version      = EXCLUDED.version,
		      capabilities = EXCLUDED.capabilities,
		      env          = EXCLUDED.env,
		      last_seen_at = NOW();
	`
	// capabilities is stored as-is: nil stays SQL NULL (legacy agent, cap check
	// skipped by ClaimNextRun); do NOT coerce to []string{} here.
	_, err = p.pool.Exec(ctx, q, agentID, hostname, os, labels, version, capabilities, envJSON)
	return err
}

// UpsertAgentOnClaim is the CLAIM path. It is a lightweight, non-destructive upsert
// used when an agent claims a run: it only knows the agent ID and its claim-time
// labels, so on conflict it only overwrites hostname/os/version/env when the caller
// supplied a non-empty value, and merges (rather than replaces) labels. This lets a
// claim refresh last_seen_at/labels without clobbering richer data recorded at full
// registration time (e.g. the register-only hostname:<h> label). See TODO #12/#23.
func (p *Postgres) UpsertAgentOnClaim(ctx context.Context, agentID, hostname, os, version string, labels []string, env map[string]string) error {
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
		  SET hostname     = COALESCE(NULLIF(EXCLUDED.hostname, ''), agents.hostname),
		      os           = COALESCE(NULLIF(EXCLUDED.os, ''), agents.os),
		      labels       = (SELECT ARRAY(SELECT DISTINCT unnest(agents.labels || EXCLUDED.labels))),
		      version      = COALESCE(NULLIF(EXCLUDED.version, ''), agents.version),
		      env          = CASE WHEN EXCLUDED.env = '{}'::jsonb THEN agents.env ELSE EXCLUDED.env END,
		      last_seen_at = NOW();
	`
	_, err = p.pool.Exec(ctx, q, agentID, hostname, os, labels, version, envJSON)
	return err
}

// TouchAgent refreshes an agent's last_seen_at from its heartbeat. It is an
// UPSERT, not a bare UPDATE: a heartbeat is the one write a live agent makes
// unconditionally on its own ticker, so it must be able to RECREATE a row that
// something else removed, not merely refresh one that happens to still exist.
//
// This matters because the agents row is not owned by the process that heartbeats
// it: DeleteAgent is an unfenced `DELETE FROM agents WHERE id = $1` reachable from
// any process holding that agent ID's credential (notably a duplicate-ID sibling
// deregistering at the end of its drain, i.e. any start-before-stop rollout). With
// the previous bare UPDATE the deletion matched zero rows, returned no error, and
// the row stayed absent until the next claim poll re-inserted it (~30s) — during
// which ListStuckRuns saw the row missing and reaped the healthy agent's run.
//
// recreated reports that the row was absent and this heartbeat re-inserted it, so
// the caller can make an otherwise invisible state loud. The re-inserted row is
// deliberately minimal (empty hostname/os/version, NULL capabilities): the next
// registration or claim poll fills the real values back in via UpsertAgent /
// UpsertAgentOnClaim. What matters here is only that liveness is not lost.
func (p *Postgres) TouchAgent(ctx context.Context, agentID string) (recreated bool, err error) {
	const q = `
		INSERT INTO agents(id, hostname, os, labels, version, env, last_seen_at)
		VALUES ($1, '', '', '{}', '', '{}'::jsonb, NOW())
		ON CONFLICT (id) DO UPDATE SET last_seen_at = NOW()
		RETURNING (xmax = 0);
	`
	// xmax = 0 is true only for a row this statement INSERTed; a row updated by
	// the ON CONFLICT clause carries the locking transaction's xid.
	err = p.pool.QueryRow(ctx, q, agentID).Scan(&recreated)
	return recreated, err
}

func (p *Postgres) DeleteAgent(ctx context.Context, agentID string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM agents WHERE id = $1`, agentID)
	return err
}

func (p *Postgres) ListAgents(ctx context.Context) ([]api.AgentInfo, error) {
	const q = `SELECT id, hostname, os, labels, version, env, last_seen_at, capabilities FROM agents ORDER BY last_seen_at DESC`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.AgentInfo
	for rows.Next() {
		var a api.AgentInfo
		var envJSON []byte
		if err := rows.Scan(&a.ID, &a.Hostname, &a.OS, &a.Labels, &a.Version, &envJSON, &a.LastSeenAt, &a.Capabilities); err != nil {
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
	const q = `SELECT id, hostname, os, labels, version, env, last_seen_at, capabilities FROM agents WHERE id = $1`
	var a api.AgentInfo
	var envJSON []byte
	err := p.pool.QueryRow(ctx, q, id).
		Scan(&a.ID, &a.Hostname, &a.OS, &a.Labels, &a.Version, &envJSON, &a.LastSeenAt, &a.Capabilities)
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
		SELECT id, job_name, status, params, created_at, updated_at, triggered_by, display_name
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
		var displayName *string
		if err := rows.Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy, &displayName); err != nil {
			return nil, err
		}
		r.Status = api.RunStatus(status)
		_ = json.Unmarshal(paramsOut, &r.Params)
		if r.Params == nil {
			r.Params = map[string]string{}
		}
		if displayName != nil {
			r.DisplayName = *displayName
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

// ListStuckRuns returns Running runs whose claiming agent is gone or has not sent
// a heartbeat within staleAfter, excluding runs claimed within the grace window
// (to avoid reaping a just-claimed run before its first heartbeat).
//
// AgentMissing distinguishes the two disjuncts, and the caller MUST treat them
// differently. A stale last_seen_at is positive evidence of death: the agent had a
// row, stopped writing to it, and staleAfter has elapsed — that is a measured
// silence. An absent row is NOT evidence of anything: it carries no timestamp, so
// this predicate has no time component for it at all, and a row that vanished one
// millisecond ago looks exactly like one that vanished an hour ago. Absence is
// naturally produced by events that say nothing about liveness (a duplicate-ID
// sibling's deregistration at end of drain, an operator DELETE /agents/{id}), so
// the stuck-run reaper confirms an AgentMissing observation across sweeps before
// acting on it — see runStuckRunReaperOnce.
func (p *Postgres) ListStuckRuns(ctx context.Context, staleAfter, grace time.Duration) ([]StuckRunRef, error) {
	const q = `
		SELECT r.id, (a.id IS NULL) AS agent_missing
		FROM runs r
		LEFT JOIN agents a ON r.claimed_by = a.id
		WHERE r.status = 'Running'
		  AND r.claimed_at IS NOT NULL
		  AND r.claimed_at < NOW() - make_interval(secs => $2)
		  AND (a.id IS NULL OR a.last_seen_at < NOW() - make_interval(secs => $1))
	`
	rows, err := p.pool.Query(ctx, q, staleAfter.Seconds(), grace.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list stuck runs: %w", err)
	}
	defer rows.Close()
	var out []StuckRunRef
	for rows.Next() {
		var ref StuckRunRef
		if err := rows.Scan(&ref.ID, &ref.AgentMissing); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// ListUnclaimableQueuedRuns returns Queued runs older than minAge that no live
// agent can claim: no agent with a heartbeat within staleAfter has labels that
// satisfy the run's agent_selector (empty selector matches any agent). This
// check is label-only by design and deliberately omits the capability clause
// that ClaimNextRun ANDs in (`a.capabilities IS NULL OR r.required_caps <@
// a.capabilities`): capabilities only make claiming stricter, so a
// label-unclaimable run is also cap-unclaimable, while a run that's
// label-claimable but capability-unschedulable (e.g. a native job when only a
// k8s agent is live) is intentionally left Queued rather than auto-failed
// here — it is surfaced instead via the JobDetail unschedulable banner
// (see serveJobSchedulability). Do not add a capability clause to this query.
//
// limit bounds the batch. The liveness answer this query returns is a snapshot,
// and every run the caller acts on is acted on with an answer that is as stale as
// the loop is long — so an unbounded batch is an unbounded staleness window (a
// 161-run backlog measured at 475ms end to end, ~2.93ms per run, against 4ms for a
// single run). The bound caps that window; FailQueuedRun re-validates per run on
// top of it. A limit <= 0 is treated as unbounded, for callers that want the whole
// set (tests).
func (p *Postgres) ListUnclaimableQueuedRuns(ctx context.Context, minAge, staleAfter time.Duration, limit int) ([]QueuedRunRef, error) {
	const q = `
		SELECT r.id, r.agent_selector
		FROM runs r
		WHERE r.status = 'Queued'
		  AND r.created_at < NOW() - make_interval(secs => $1)
		  AND NOT EXISTS (
		    SELECT 1 FROM agents a
		    WHERE a.last_seen_at >= NOW() - make_interval(secs => $2)
		      AND (r.agent_selector = '{}' OR r.agent_selector <@ a.labels)
		  )
		ORDER BY r.created_at
		LIMIT CASE WHEN $3::int > 0 THEN $3::int ELSE NULL END
	`
	rows, err := p.pool.Query(ctx, q, minAge.Seconds(), staleAfter.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("list unclaimable queued runs: %w", err)
	}
	defer rows.Close()
	var out []QueuedRunRef
	for rows.Next() {
		var ref QueuedRunRef
		if err := rows.Scan(&ref.ID, &ref.AgentSelector); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
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
func (p *Postgres) SetStepOutput(ctx context.Context, runID string, stepIndex int, variant, key, value string) error {
	const q = `
		INSERT INTO step_outputs(run_id, step_index, variant, key, value)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (run_id, step_index, variant, key) DO UPDATE SET value = EXCLUDED.value;
	`
	_, err := p.pool.Exec(ctx, q, runID, stepIndex, variant, key, value)
	return err
}

func (p *Postgres) GetStepOutputs(ctx context.Context, runID string, stepIndex int) (map[string]string, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT key, value FROM step_outputs WHERE run_id = $1 AND step_index = $2 ORDER BY variant`,
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
	conn, err := p.lockPool.Acquire(ctx)
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

func (p *Postgres) ListRunsNeedingArchival(ctx context.Context, limit int, excluded []string) ([]api.Run, error) {
	// Deliberately omits triggered_by (and display_name): this is an internal
	// archival-housekeeping projection, not a UI-facing api.Run projection.
	const q = `
		SELECT id, job_name, status, params, created_at, updated_at
		FROM runs
		WHERE status IN ('Succeeded', 'Failed', 'Cancelled')
		  AND id NOT IN (SELECT run_id FROM run_log_archives)
		  AND id != ALL($2::uuid[])
		ORDER BY updated_at
		LIMIT $1;
	`
	rows, err := p.pool.Query(ctx, q, limit, excluded)
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

// ListExpiredRuns returns IDs of terminal runs whose updated_at is older than
// cutoff, oldest first. A terminal run's updated_at no longer changes, so it
// is effectively the finish time. Used by the run-retention sweeper.
func (p *Postgres) ListExpiredRuns(ctx context.Context, cutoff time.Time, limit int, excluded []string) ([]string, error) {
	const q = `
		SELECT id FROM runs
		WHERE status IN ('Succeeded', 'Failed', 'Cancelled')
		  AND updated_at < $1
		  AND id != ALL($3::uuid[])
		ORDER BY updated_at
		LIMIT $2;
	`
	rows, err := p.pool.Query(ctx, q, cutoff, limit, excluded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (p *Postgres) CreateLogArchive(ctx context.Context, runID, objectKey string, sizeBytes, lineCount, maxSeq int64) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO run_log_archives(run_id, object_key, size_bytes, line_count, max_seq)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (run_id) DO UPDATE
		   SET object_key = EXCLUDED.object_key, size_bytes = EXCLUDED.size_bytes,
		       line_count = EXCLUDED.line_count, max_seq = EXCLUDED.max_seq, archived_at = NOW()`,
		runID, objectKey, sizeBytes, lineCount, maxSeq)
	return err
}

func (p *Postgres) GetLogArchive(ctx context.Context, runID string) (*LogArchive, error) {
	var a LogArchive
	err := p.pool.QueryRow(ctx,
		`SELECT run_id, object_key, size_bytes, line_count, max_seq, archived_at, trimmed_at FROM run_log_archives WHERE run_id = $1`,
		runID).Scan(&a.RunID, &a.ObjectKey, &a.SizeBytes, &a.LineCount, &a.MaxSeq, &a.ArchivedAt, &a.TrimmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListTrimCandidates returns run IDs whose logs are archived but not yet
// trimmed, with archived_at older than cutoff, oldest first. Archive records
// only exist for terminal runs, so no status filter is needed.
//
// The AND (...) clause excludes candidates that are permanently incomplete —
// without it, a run whose live logs will never be fully covered (>1,000,000-
// line archiver cap, or late appends after archival) would fail TrimRunLogs'
// coverage check forever, never leave the candidate set, and — being oldest
// — wedge the front of every batch, since runLogTrimOnce stops a batch as
// soon as it makes zero progress. Two arms:
//   - `line_count = 0 AND max_seq = 0`: legacy records written by migration
//     012's default, before this branch's archiver started recording real
//     coverage. Coverage is simply unknown, not known-incomplete, so these
//     stay in the set — the sweeper's healing path (runLogTrimOnce) deletes
//     them so the archiver re-creates them with real counts. Runs with no
//     live log rows also match this arm and trim normally (0 <= 0 passes
//     TrimRunLogs' coverage check), which is intentional.
//   - `NOT EXISTS (... l.seq > run_log_archives.max_seq ...)`: excludes
//     non-legacy records whose live logs already exceed the recorded
//     coverage — these would fail TrimRunLogs' check today and are not
//     legacy, so re-archiving won't fix them until the run stops growing.
//     Cheap via the logs (run_id, seq) index.
func (p *Postgres) ListTrimCandidates(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	const q = `
		SELECT run_id FROM run_log_archives
		WHERE trimmed_at IS NULL AND archived_at < $1
		  AND (
		    (line_count = 0 AND max_seq = 0)
		    OR NOT EXISTS (
		      SELECT 1 FROM logs l
		      WHERE l.run_id = run_log_archives.run_id AND l.seq > run_log_archives.max_seq
		    )
		  )
		ORDER BY archived_at
		LIMIT $2;
	`
	rows, err := p.pool.Query(ctx, q, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TrimRunLogs deletes a run's logs rows after marking its archive record
// trimmed, in one transaction. The mark goes FIRST and guards trimmed_at IS
// NULL: if there is no untrimmed archive record (never archived, already
// trimmed, or the run was deleted by retention) nothing is deleted and the
// call is a (0, nil) no-op.
//
// Before deleting anything, it re-checks the live logs table against the
// archive record's line_count/max_seq IN THIS SAME TRANSACTION (race-free
// vs. a late agent log flush landing between the caller's earlier checks
// and this call): if the run has more logs rows, or a higher seq, than the
// archive claims to cover, the archive is incomplete — trimming would
// destroy rows no archive has a copy of. In that case the whole transaction
// (including the trimmed_at mark) is rolled back and ErrArchiveIncomplete is
// returned so the sweeper skips the run rather than deleting its archive
// record (a >1,000,000-line run would otherwise loop re-archive forever).
func (p *Postgres) TrimRunLogs(ctx context.Context, runID string) (int64, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx,
		`UPDATE run_log_archives SET trimmed_at = NOW() WHERE run_id = $1 AND trimmed_at IS NULL`, runID)
	if err != nil {
		return 0, err
	}
	if ct.RowsAffected() == 0 {
		return 0, nil // no untrimmed archive record: never touch logs
	}

	var wantCount, wantMaxSeq int64
	if err := tx.QueryRow(ctx,
		`SELECT line_count, max_seq FROM run_log_archives WHERE run_id = $1`, runID,
	).Scan(&wantCount, &wantMaxSeq); err != nil {
		return 0, err
	}
	var haveCount, haveMaxSeq int64
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(MAX(seq), 0) FROM logs WHERE run_id = $1`, runID,
	).Scan(&haveCount, &haveMaxSeq); err != nil {
		return 0, err
	}
	if haveCount > wantCount || haveMaxSeq > wantMaxSeq {
		return 0, fmt.Errorf("%w: run %s", ErrArchiveIncomplete, runID)
	}

	// Bound the delete by wantMaxSeq (not an unqualified DELETE FROM logs)
	// to close a residual race: a row committed between the coverage SELECT
	// above and this DELETE — same transaction, but Postgres re-evaluates a
	// plain WHERE run_id = $1 against the table's current state at execution
	// time — would otherwise be destroyed even though it was never covered by
	// the coverage check. Restricting to seq <= wantMaxSeq guarantees only
	// rows the archive actually covers are deleted. For a legacy zero-
	// coverage record (wantMaxSeq = 0, no live rows) this deletes nothing,
	// which is correct: TrimRunLogs still marks trimmed_at, the caller
	// observes a (0, nil) delete, and ListTrimCandidates' NOT EXISTS arm sees
	// no seq > 0 to exclude on if a real archive record replaces it later.
	tag, err := tx.Exec(ctx, `DELETE FROM logs WHERE run_id = $1 AND seq <= $2`, runID, wantMaxSeq)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

func (p *Postgres) DeleteLogArchive(ctx context.Context, runID string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM run_log_archives WHERE run_id = $1`, runID)
	return err
}

// ErrListenPoolExhausted is returned by ListenForNotify when no listenPool
// connection became available within listenAcquireTimeout. It is distinct
// from an ordinary context-cancellation error (the caller's ctx ending first,
// e.g. an SSE viewer disconnecting) so callers can tell "the pool is full"
// apart from "the request ended" and respond to each differently, instead of
// pattern-matching an error string. Match it with errors.Is.
var ErrListenPoolExhausted = errors.New("store: listen pool exhausted")

func (p *Postgres) ListenForNotify(ctx context.Context, channel string, callback func(payload string)) error {
	acquireCtx, cancel := context.WithTimeout(ctx, listenAcquireTimeout)
	defer cancel()
	conn, err := p.listenPool.Acquire(acquireCtx)
	if err != nil {
		// Distinguish "our own bound fired" from "the caller's ctx ended
		// first" (an ordinary client disconnect racing the acquire — not
		// exhaustion, and should keep returning the ordinary context error)
		// by checking the ORIGINAL ctx, not acquireCtx: acquireCtx.Err() is
		// set in both cases, but ctx.Err() is only set when the caller's own
		// context is what actually ended.
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			stat := p.listenPool.Stat()
			slog.Error("listen pool exhausted: no connection acquired within timeout; raise listenMaxConns or reduce concurrent listeners",
				"pool", "listen",
				"maxConns", stat.MaxConns(),
				"acquiredConns", stat.AcquiredConns(),
				"timeout", listenAcquireTimeout,
				"channel", channel)
			return fmt.Errorf("acquire listen pool connection for channel %q: %w", channel, ErrListenPoolExhausted)
		}
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

func (p *Postgres) UpsertSecret(ctx context.Context, name string, encryptedDEK, ciphertext []byte) (*StoredSecret, error) {
	const q = `
		INSERT INTO secrets(name, encrypted_dek, ciphertext, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (name) DO UPDATE
		  SET encrypted_dek = EXCLUDED.encrypted_dek,
		      ciphertext     = EXCLUDED.ciphertext,
		      updated_at     = NOW()
		RETURNING id, name, encrypted_dek, ciphertext, created_at, updated_at;
	`
	var s StoredSecret
	err := p.pool.QueryRow(ctx, q, name, encryptedDEK, ciphertext).
		Scan(&s.ID, &s.Name, &s.EncryptedDEK, &s.Ciphertext, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("upsert secret: %w", err)
	}
	return &s, nil
}

func (p *Postgres) GetSecret(ctx context.Context, name string) (*StoredSecret, error) {
	const q = `SELECT id, name, encrypted_dek, ciphertext, created_at, updated_at
		FROM secrets WHERE name = $1`
	var s StoredSecret
	err := p.pool.QueryRow(ctx, q, name).
		Scan(&s.ID, &s.Name, &s.EncryptedDEK, &s.Ciphertext, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get secret %q: %w", name, err)
	}
	return &s, nil
}

func (p *Postgres) ListSecrets(ctx context.Context) ([]SecretMeta, error) {
	const q = `SELECT id, name, created_at FROM secrets ORDER BY name`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretMeta
	for rows.Next() {
		var m SecretMeta
		if err := rows.Scan(&m.ID, &m.Name, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteSecret(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM secrets WHERE name = $1`, name)
	return err
}

// CreatePAT creates a Personal Access Token and returns its metadata.
func (p *Postgres) CreatePAT(ctx context.Context, name, tokenHash, role string, expiresAt *time.Time) (*PAT, error) {
	const q = `INSERT INTO pats(name, token_hash, role, expires_at) VALUES ($1, $2, $3, $4)
		RETURNING id, name, role, created_at, expires_at, last_used_at;`
	var pat PAT
	err := p.pool.QueryRow(ctx, q, name, tokenHash, role, expiresAt).
		Scan(&pat.ID, &pat.Name, &pat.Role, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt)
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
	const updateQ = `UPDATE pats SET token_hash = $2, role = 'admin' WHERE name = $1
		RETURNING id, name, role, created_at, expires_at, last_used_at;`
	var pat PAT
	err := p.pool.QueryRow(ctx, updateQ, name, tokenHash).
		Scan(&pat.ID, &pat.Name, &pat.Role, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt)
	if err == nil {
		return &pat, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("rotate bootstrap pat: %w", err)
	}

	const insertQ = `INSERT INTO pats(name, token_hash, role) VALUES ($1, $2, 'admin')
		RETURNING id, name, role, created_at, expires_at, last_used_at;`
	if err := p.pool.QueryRow(ctx, insertQ, name, tokenHash).
		Scan(&pat.ID, &pat.Name, &pat.Role, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt); err != nil {
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
	const q = `SELECT id, name, role, created_at, expires_at, last_used_at FROM pats
		WHERE token_hash = $1 AND (expires_at IS NULL OR expires_at > NOW())`
	var pat PAT
	err := p.pool.QueryRow(ctx, q, tokenHash).
		Scan(&pat.ID, &pat.Name, &pat.Role, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt)
	if err != nil {
		return nil, fmt.Errorf("get pat: %w", err)
	}
	return &pat, nil
}

// ListPATs returns all PATs ordered by creation date ascending.
func (p *Postgres) ListPATs(ctx context.Context) ([]PAT, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, name, role, created_at, expires_at, last_used_at FROM pats ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PAT
	for rows.Next() {
		var pat PAT
		if err := rows.Scan(&pat.ID, &pat.Name, &pat.Role, &pat.CreatedAt, &pat.ExpiresAt, &pat.LastUsedAt); err != nil {
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

// UpsertVars creates or updates a Vars manifest and returns its name.
func (p *Postgres) UpsertVars(ctx context.Context, name string, specJSON []byte) (string, error) {
	const q = `INSERT INTO vars(name, spec) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET spec = EXCLUDED.spec, updated_at = NOW()
		RETURNING name;`
	var out string
	err := p.pool.QueryRow(ctx, q, name, specJSON).Scan(&out)
	if err != nil {
		return "", fmt.Errorf("upsert vars %q: %w", name, err)
	}
	return out, nil
}

// ListVars returns all Vars manifests ordered by name ascending.
func (p *Postgres) ListVars(ctx context.Context) ([]VarsRecord, error) {
	rows, err := p.pool.Query(ctx, `SELECT name, spec FROM vars ORDER BY name;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VarsRecord
	for rows.Next() {
		var v VarsRecord
		if err := rows.Scan(&v.Name, &v.Spec); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteVars deletes a Vars manifest by name.
func (p *Postgres) DeleteVars(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM vars WHERE name = $1;`, name)
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
func (p *Postgres) CreateSession(ctx context.Context, tokenHash, sub, email, role string, refreshDEK, refreshCT []byte, expiresAt time.Time) (*Session, error) {
	const q = `INSERT INTO sessions(token_hash, sub, email, role, refresh_token_dek, refresh_token_ct, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, token_hash, sub, email, role, refresh_token_dek, refresh_token_ct, expires_at, last_used_at, created_at`
	var s Session
	err := p.pool.QueryRow(ctx, q, tokenHash, sub, email, role, refreshDEK, refreshCT, expiresAt).
		Scan(&s.ID, &s.TokenHash, &s.Sub, &s.Email, &s.Role, &s.RefreshTokenDEK, &s.RefreshTokenCT, &s.ExpiresAt, &s.LastUsedAt, &s.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &s, nil
}

// GetSessionByHash retrieves a session by token_hash.
func (p *Postgres) GetSessionByHash(ctx context.Context, tokenHash string) (*Session, error) {
	const q = `SELECT id, token_hash, sub, email, role, refresh_token_dek, refresh_token_ct, expires_at, last_used_at, created_at
		FROM sessions WHERE token_hash = $1`
	var s Session
	err := p.pool.QueryRow(ctx, q, tokenHash).
		Scan(&s.ID, &s.TokenHash, &s.Sub, &s.Email, &s.Role, &s.RefreshTokenDEK, &s.RefreshTokenCT, &s.ExpiresAt, &s.LastUsedAt, &s.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get session: %w", err)
	}
	return &s, nil
}

// UpdateSessionExpiry updates the expiry time and refresh token of a session.
func (p *Postgres) UpdateSessionExpiry(ctx context.Context, id string, refreshDEK, refreshCT []byte, expiresAt time.Time) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE sessions SET expires_at = $1, refresh_token_dek = $2, refresh_token_ct = $3 WHERE id = $4`,
		expiresAt, refreshDEK, refreshCT, id)
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

// InsertAuditLog records a single state-changing API operation.
func (p *Postgres) InsertAuditLog(ctx context.Context, actor, method, path, action, resource string, status int) error {
	const q = `
		INSERT INTO audit_logs(actor, method, path, action, resource, status)
		VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := p.pool.Exec(ctx, q, actor, method, path, action, resource, status)
	return err
}

// ListAuditLogs returns audit log entries newest-first, with limit/offset pagination.
func (p *Postgres) ListAuditLogs(ctx context.Context, limit, offset int) ([]api.AuditLog, error) {
	const q = `
		SELECT id, occurred_at, actor, method, path, action, resource, status
		FROM audit_logs
		ORDER BY occurred_at DESC, id DESC
		LIMIT $1 OFFSET $2`
	rows, err := p.pool.Query(ctx, q, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := make([]api.AuditLog, 0)
	for rows.Next() {
		var a api.AuditLog
		if err := rows.Scan(&a.ID, &a.OccurredAt, &a.Actor, &a.Method, &a.Path, &a.Action, &a.Resource, &a.Status); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// DeleteAuditLogsOlderThan deletes audit log rows with occurred_at before the given time.
// Returns the number of rows deleted.
func (p *Postgres) DeleteAuditLogsOlderThan(ctx context.Context, before time.Time) (int, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM audit_logs WHERE occurred_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ListPendingRuns returns Pending runs (oldest first, up to limit) that are
// not in excluded — the git resolver's backoff exclusion set for candidates
// whose resolution has been transiently failing (see failureBackoff).
func (p *Postgres) ListPendingRuns(ctx context.Context, limit int, excluded []string) ([]PendingRun, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, spec, created_at FROM runs
		 WHERE status = 'Pending' AND id != ALL($2::uuid[])
		 ORDER BY created_at LIMIT $1`,
		limit, excluded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []PendingRun
	for rows.Next() {
		var r PendingRun
		if err := rows.Scan(&r.ID, &r.Spec, &r.CreatedAt); err != nil {
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

// RenameJob re-keys a job from oldName to newName and repoints its run history,
// in a single transaction. It is used by the AppSource reconciler's legacy
// re-keying guard (bug #25) to turn a bare-named legacy Job (e.g. "build") into
// its qualified form (e.g. "team-a/build") without losing run history or leaving
// an orphan row.
//
// Two cases are handled, driven by whether newName already exists:
//
//   - newName does NOT exist yet: copy the row to newName, repoint run history,
//     then drop the old row. The FK runs.job_name -> jobs.name has ON DELETE
//     CASCADE but no ON UPDATE CASCADE and is not deferrable, so a plain UPDATE of
//     jobs.name would transiently violate the FK for existing runs. We instead
//     create the newName parent first, move the runs onto it, then remove the old
//     parent. See the ordering below.
//
//   - newName ALREADY exists (the common reconciler path: applyResource has already
//     UpsertJob'd the qualified row before prune runs): the oldName row is an
//     orphan. We repoint run history to newName and DELETE the orphan, never
//     clobbering the existing qualified row's spec.
//
// Ordering (FK-safe, no ON UPDATE CASCADE):
//  1. Ensure the newName row exists. If it does not, copy oldName's row to newName.
//  2. Repoint runs.job_name from oldName -> newName (now a valid FK target).
//  3. Delete the oldName row (safe: no runs reference it anymore).
//
// Idempotent: a missing oldName is a no-op. Never leaves zero rows for a job that
// should exist; never touches unrelated jobs.
func (p *Postgres) RenameJob(ctx context.Context, oldName, newName string) error {
	if oldName == newName {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("rename job: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Ensure the newName row exists. INSERT ... SELECT from the old row copies
	// its api_version/spec so a plain rename preserves the job definition. If
	// newName already exists (reconciler path), ON CONFLICT DO NOTHING leaves the
	// already-applied qualified row untouched. If oldName does not exist and
	// newName does not exist, this inserts nothing (SELECT is empty) — handled by
	// the no-op guard below.
	if _, err := tx.Exec(ctx, `
		INSERT INTO jobs(name, api_version, spec, updated_at)
		SELECT $2, api_version, spec, NOW() FROM jobs WHERE name = $1
		ON CONFLICT (name) DO NOTHING`, oldName, newName); err != nil {
		return fmt.Errorf("rename job: ensure new row: %w", err)
	}

	// If neither row exists we cannot proceed meaningfully; treat as no-op.
	var newExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE name = $1)`, newName).
		Scan(&newExists); err != nil {
		return fmt.Errorf("rename job: check new row: %w", err)
	}
	if !newExists {
		// oldName didn't exist either — nothing to do.
		return tx.Commit(ctx)
	}

	// 2. Repoint run history now that newName is a valid FK target.
	if _, err := tx.Exec(ctx,
		`UPDATE runs SET job_name = $2 WHERE job_name = $1`, oldName, newName); err != nil {
		return fmt.Errorf("rename job: repoint runs: %w", err)
	}

	// 3. Remove the old (bare) row. No runs reference it anymore, so the ON DELETE
	// CASCADE FK does not cascade any history away.
	if _, err := tx.Exec(ctx, `DELETE FROM jobs WHERE name = $1`, oldName); err != nil {
		return fmt.Errorf("rename job: delete old row: %w", err)
	}

	return tx.Commit(ctx)
}

// UpsertAppSource creates or updates an AppSource. On update, last_commit is
// reset to "" (forcing a fresh sync) only when the spec actually changed; an
// upsert with an identical spec preserves last_commit. This avoids a nested
// AppSource re-fetching its whole directory on every parent reconcile tick,
// where the parent re-applies the child's unchanged spec each time.
func (p *Postgres) UpsertAppSource(ctx context.Context, name string, spec []byte) (*AppSource, error) {
	const q = `
		INSERT INTO app_sources(name, spec)
		VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE
		  SET spec = EXCLUDED.spec,
		      last_commit = CASE
		        WHEN app_sources.spec IS DISTINCT FROM EXCLUDED.spec THEN ''
		        ELSE app_sources.last_commit
		      END,
		      updated_at = NOW()
		RETURNING name, spec, last_synced_at, last_commit, managed_resources, updated_at, sync_status, last_error`
	var a AppSource
	var mr []byte
	if err := p.pool.QueryRow(ctx, q, name, spec).
		Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &mr, &a.UpdatedAt, &a.SyncStatus, &a.LastError); err != nil {
		return nil, fmt.Errorf("upsert AppSource: %w", err)
	}
	if err := unmarshalManagedResources(mr, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAppSource retrieves an AppSource by name.
func (p *Postgres) GetAppSource(ctx context.Context, name string) (*AppSource, error) {
	const q = `SELECT name, spec, last_synced_at, last_commit, managed_resources, updated_at, sync_status, last_error FROM app_sources WHERE name = $1`
	var a AppSource
	var mr []byte
	if err := p.pool.QueryRow(ctx, q, name).
		Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &mr, &a.UpdatedAt, &a.SyncStatus, &a.LastError); err != nil {
		return nil, fmt.Errorf("get AppSource name=%s: %w", name, err)
	}
	if err := unmarshalManagedResources(mr, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// ListAppSources returns all AppSources ordered by name.
func (p *Postgres) ListAppSources(ctx context.Context) ([]AppSource, error) {
	const q = `SELECT name, spec, last_synced_at, last_commit, managed_resources, updated_at, sync_status, last_error FROM app_sources ORDER BY name`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppSource
	for rows.Next() {
		var a AppSource
		var mr []byte
		if err := rows.Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &mr, &a.UpdatedAt, &a.SyncStatus, &a.LastError); err != nil {
			return nil, err
		}
		if err := unmarshalManagedResources(mr, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAppSource deletes an AppSource by name.
func (p *Postgres) DeleteAppSource(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM app_sources WHERE name = $1`, name)
	return err
}

// UpdateAppSourceSyncState updates the sync state of an AppSource (last commit, sync time, and managed resource list).
// A successful sync also marks the AppSource as Synced and clears any recorded error.
func (p *Postgres) UpdateAppSourceSyncState(ctx context.Context, name, lastCommit string, syncedAt time.Time, managed []ResourceRef) error {
	if managed == nil {
		managed = []ResourceRef{}
	}
	data, err := json.Marshal(managed)
	if err != nil {
		return fmt.Errorf("marshal managed resources: %w", err)
	}
	_, err = p.pool.Exec(ctx,
		`UPDATE app_sources SET last_commit = $1, last_synced_at = $2, managed_resources = $3, sync_status = 'Synced', last_error = '', updated_at = NOW() WHERE name = $4`,
		lastCommit, syncedAt, data, name)
	return err
}

// unmarshalManagedResources decodes the managed_resources jsonb column into a.ManagedResources.
func unmarshalManagedResources(raw []byte, a *AppSource) error {
	if len(raw) == 0 {
		a.ManagedResources = nil
		return nil
	}
	if err := json.Unmarshal(raw, &a.ManagedResources); err != nil {
		return fmt.Errorf("unmarshal managed_resources for %q: %w", a.Name, err)
	}
	return nil
}

// ResetAppSourceCommit resets the last_commit of an AppSource to an empty string.
func (p *Postgres) ResetAppSourceCommit(ctx context.Context, name string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE app_sources SET last_commit = '', updated_at = NOW() WHERE name = $1`,
		name)
	return err
}

// SetAppSourceSyncStatus sets the sync_status and last_error of an AppSource.
func (p *Postgres) SetAppSourceSyncStatus(ctx context.Context, name, status, lastError string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE app_sources SET sync_status = $1, last_error = $2, updated_at = NOW() WHERE name = $3`,
		status, lastError, name)
	return err
}

// FindManagingAppSource returns the AppSource whose managed_resources contains
// {kind,name}, or nil when the resource is not managed by any AppSource.
func (p *Postgres) FindManagingAppSource(ctx context.Context, kind, name string) (*AppSource, error) {
	ref, err := json.Marshal([]ResourceRef{{Kind: kind, Name: name}})
	if err != nil {
		return nil, err
	}
	const q = `SELECT name, spec, last_synced_at, last_commit, managed_resources, updated_at, sync_status, last_error
FROM app_sources WHERE managed_resources @> $1::jsonb LIMIT 1`
	var a AppSource
	var mr []byte
	err = p.pool.QueryRow(ctx, q, ref).Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &mr, &a.UpdatedAt, &a.SyncStatus, &a.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := unmarshalManagedResources(mr, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// stuckSyncingResetError is the last_error recorded on an AppSource that the
// sync reaper reset after it was stuck in "Syncing" past the timeout.
const stuckSyncingResetError = "sync reset by reaper: stuck in Syncing past timeout (previous sync did not complete — reconciler crash, process restart, or leadership change)"

// ResetStuckSyncingAppSources finds AppSources stuck in sync_status='Syncing'
// whose updated_at is older than olderThan and resets them to a retryable state,
// returning the number reset.
//
// The manual sync-trigger API sets sync_status='Syncing' synchronously, decoupled
// from the actual reconcile that happens on the next ticker cycle (see
// handleSyncAppSource). If the reconciler panics, the process dies, or leadership
// changes mid-sync, the row can stay 'Syncing' forever. This reaper bounds that.
//
// Recovered state: we set sync_status='Failed' (so the UI/API surfaces that the
// prior attempt did not finish) and clear last_commit. Clearing last_commit is
// what actually makes the next reconcile tick re-sync: shouldSync returns true
// when last_commit is empty (see appsource_reconciler.shouldSync), and
// syncAppSource force-fetches when last_commit is empty. A last_error explains the
// reset. We deliberately do NOT set status back to 'Syncing' (that would be
// re-armed as stuck next cycle); the reconciler will flip it to Synced/Failed on
// its next real attempt.
func (p *Postgres) ResetStuckSyncingAppSources(ctx context.Context, olderThan time.Duration) (int, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE app_sources
		SET sync_status = 'Failed',
		    last_error  = $2,
		    last_commit = '',
		    updated_at  = NOW()
		WHERE sync_status = 'Syncing'
		  AND updated_at < NOW() - make_interval(secs => $1)`,
		olderThan.Seconds(), stuckSyncingResetError)
	if err != nil {
		return 0, fmt.Errorf("reset stuck syncing app_sources: %w", err)
	}
	return int(tag.RowsAffected()), nil
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

// DecideApproval records a human decision on a pending approval gate. It is
// first-writer-wins on the row's own status AND conditional on the gate still
// being decidable at all:
//
//   - the run must not be terminal. run_approvals is an audit record (docs/user-guide/writing-jobs/approval-and-finally.md,
//     docs/operator-manual/audit.md), and without this clause an ordinary `unified-cli approve`
//     wrote Approved plus a named principal onto runs that had already Failed or
//     been Cancelled, returned 204, and left an audit_logs row reading as success.
//     Worse than the false record: when the decision commits inside the agent's 5s
//     cancel-detection window, WaitForApproval sees Approved before the agent sees
//     the cancel, so the post-gate step body actually executes on a Cancelled run.
//   - the gate must not have passed its timeout_at. Past it the agent has already
//     failed the step locally, so a decision can no longer have any effect on
//     execution and only falsifies the record. (The approval reaper reconciles such
//     rows to TimedOut/system within ~1 minute; this closes the window before it.)
//
// Both clauses live in the SQL rather than in the handler on purpose: a handler-side
// GetRun check races the run's own terminalization landing between the read and the
// UPDATE, which is the exact shape of bug this fixes. The handler classifies the
// refusal for the HTTP response afterwards, which is safe because the write has
// already been decided by then.
func (p *Postgres) DecideApproval(ctx context.Context, runID string, stepIndex int, status, decidedBy, comment string) (bool, error) {
	const q = `
		UPDATE run_approvals a
		SET status = $3, decided_by = $4, comment = $5, decided_at = now()
		WHERE a.run_id = $1 AND a.step_index = $2 AND a.status = 'Pending'
		  AND (a.timeout_at IS NULL OR a.timeout_at > now())
		  AND EXISTS (
		    SELECT 1 FROM runs r
		    WHERE r.id = a.run_id
		      AND r.status NOT IN ('Succeeded', 'Failed', 'Cancelled')
		  );
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

func (p *Postgres) MarkExpiredApprovalsTimedOut(ctx context.Context) (int, error) {
	const q = `
		UPDATE run_approvals
		SET status = 'TimedOut', decided_by = 'system', decided_at = now()
		WHERE status = 'Pending' AND timeout_at IS NOT NULL AND timeout_at < now();
	`
	tag, err := p.pool.Exec(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("mark expired approvals timed out: %w", err)
	}
	return int(tag.RowsAffected()), nil
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
	out := []api.RunApproval{}
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

// CountRunsByStatus returns the number of non-terminal runs per status.
// Terminal statuses are excluded so the result stays a bounded gauge input.
func (p *Postgres) CountRunsByStatus(ctx context.Context) (map[api.RunStatus]int, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT status, COUNT(*) FROM runs
		 WHERE status IN ('Pending','Queued','Running')
		 GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[api.RunStatus]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[api.RunStatus(status)] = n
	}
	return out, rows.Err()
}

// CountAgentsByLiveness partitions registered agents by heartbeat freshness:
// alive = last_seen_at within staleAfter, stale = older than staleAfter.
func (p *Postgres) CountAgentsByLiveness(ctx context.Context, staleAfter time.Duration) (alive, stale int, err error) {
	err = p.pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE last_seen_at >= NOW() - make_interval(secs => $1)),
		        COUNT(*) FILTER (WHERE last_seen_at <  NOW() - make_interval(secs => $1))
		 FROM agents`, staleAfter.Seconds()).Scan(&alive, &stale)
	return alive, stale, err
}
