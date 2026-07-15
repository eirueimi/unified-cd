package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

// Test databases are cloned from a single, already-migrated Postgres container that
// is shared across every test in the test binary. Provisioning a fresh Docker
// container per test made the full package suite spin up ~130 containers
// sequentially (there is no t.Parallel), taking many minutes and blowing past any
// reasonable test timeout. Instead we start ONE container lazily, migrate a template
// database once, and hand each test its own database via CREATE DATABASE ... TEMPLATE
// (tens of milliseconds) that is dropped on cleanup.

const (
	pgUser       = "unified"
	pgPassword   = "unified"
	pgAdminDB    = "unified"          // default DB from the container env; maintenance connection for CREATE/DROP DATABASE
	pgTemplateDB = "unified_template" // migrated once, cloned per test
)

var (
	sharedPGOnce sync.Once
	sharedPG     sharedPostgres
	sharedPGErr  error

	testDBSeq  atomic.Int64
	createDBMu sync.Mutex // serializes CREATE/DROP DATABASE against the shared template
)

type sharedPostgres struct {
	host  string  // "localhost:PORT"
	admin *sql.DB // maintenance connection to pgAdminDB; lives for the whole test binary
}

func pgDSN(host, dbname string) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", pgUser, pgPassword, host, dbname)
}

// startSharedPostgres boots the single shared container and migrates the template DB.
// It records any failure in sharedPGErr so every waiting test fails with the same cause.
func startSharedPostgres() {
	pool, err := dockertest.NewPool("")
	if err != nil {
		sharedPGErr = fmt.Errorf("dockertest pool: %w", err)
		return
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "postgres",
		Tag:        "16-alpine",
		Env: []string{
			"POSTGRES_USER=" + pgUser,
			"POSTGRES_PASSWORD=" + pgPassword,
			"POSTGRES_DB=" + pgAdminDB,
			"listen_addresses=*",
		},
	}, func(c *docker.HostConfig) {
		c.AutoRemove = true
		c.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		sharedPGErr = fmt.Errorf("dockertest run: %w", err)
		return
	}
	// The container must outlive individual tests, so it is deliberately not purged
	// per test. There is no package TestMain to tear it down at the end, so Expire
	// tells the Docker daemon to kill it after a generous bound even if the test
	// process dies; AutoRemove then removes it. This is the safety net against leaks.
	_ = resource.Expire(900)

	host := "localhost:" + resource.GetPort("5432/tcp")
	adminDSN := pgDSN(host, pgAdminDB)

	pool.MaxWait = 60 * time.Second
	if err := pool.Retry(func() error {
		db, e := sql.Open("pgx", adminDSN)
		if e != nil {
			return e
		}
		defer db.Close() // close EVERY attempt's handle (the previous version leaked a *sql.DB + connectionOpener goroutine per failed retry)
		return db.Ping()
	}); err != nil {
		sharedPGErr = fmt.Errorf("postgres not ready: %w", err)
		return
	}

	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		sharedPGErr = fmt.Errorf("open admin connection: %w", err)
		return
	}
	if _, err := admin.Exec("CREATE DATABASE " + pgTemplateDB); err != nil {
		admin.Close()
		sharedPGErr = fmt.Errorf("create template db: %w", err)
		return
	}
	if err := (&Postgres{}).Migrate(pgDSN(host, pgTemplateDB)); err != nil {
		admin.Close()
		sharedPGErr = fmt.Errorf("migrate template db: %w", err)
		return
	}
	// golang-migrate's postgres driver keeps a checked-out session connection for its
	// advisory lock that Migrate's db.Close() does not release, so the template still
	// has a live connection. CREATE DATABASE ... TEMPLATE requires zero connections to
	// the source, so evict any stragglers and wait until the template is idle.
	if err := waitTemplateIdle(admin); err != nil {
		admin.Close()
		sharedPGErr = fmt.Errorf("drain template db: %w", err)
		return
	}

	sharedPG = sharedPostgres{host: host, admin: admin}
}

// waitTemplateIdle terminates every backend connected to the template database and
// polls until none remain, so the template can be safely used as a CREATE DATABASE source.
func waitTemplateIdle(admin *sql.DB) error {
	for i := 0; i < 100; i++ {
		if _, err := admin.Exec(
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
			pgTemplateDB); err != nil {
			return err
		}
		var n int
		if err := admin.QueryRow(
			`SELECT count(*) FROM pg_stat_activity WHERE datname = $1`, pgTemplateDB).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("template db still has active connections after eviction")
}

// NewTestPostgres returns a Postgres backed by a fresh, fully-migrated database
// cloned from the shared container's template. The database is dropped on cleanup.
func NewTestPostgres(t *testing.T) *Postgres {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	sharedPGOnce.Do(startSharedPostgres)
	if sharedPGErr != nil {
		t.Fatalf("shared postgres: %v", sharedPGErr)
	}

	dbName := fmt.Sprintf("test_%d", testDBSeq.Add(1))

	createDBMu.Lock()
	_, err := sharedPG.admin.ExecContext(context.Background(),
		fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", dbName, pgTemplateDB))
	createDBMu.Unlock()
	if err != nil {
		t.Fatalf("create test db %s: %v", dbName, err)
	}

	pg, err := NewPostgres(context.Background(), pgDSN(sharedPG.host, dbName))
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() {
		pg.Close() // release pooled connections before dropping
		createDBMu.Lock()
		// WITH (FORCE) terminates any stragglers (Postgres 13+); the container is 16.
		_, _ = sharedPG.admin.ExecContext(context.Background(),
			fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", dbName))
		createDBMu.Unlock()
	})
	return pg
}

// BackdateRunCreatedAt sets a run's created_at to age before now. Test-only
// helper (Postgres.pool is unexported, so packages other than store — e.g.
// the git-resolver deadline tests in internal/controller — need this to
// simulate an old Pending run without a store.Store method for it).
func (p *Postgres) BackdateRunCreatedAt(ctx context.Context, runID string, age time.Duration) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE runs SET created_at = NOW() - ($1 * interval '1 second') WHERE id = $2`,
		age.Seconds(), runID)
	return err
}
