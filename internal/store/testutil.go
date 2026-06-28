package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
)

func NewTestPostgres(t *testing.T) *Postgres {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Fatalf("dockertest pool: %v", err)
	}
	resource, err := pool.RunWithOptions(&dockertest.RunOptions{
		Repository: "postgres",
		Tag:        "16-alpine",
		Env: []string{
			"POSTGRES_USER=unified",
			"POSTGRES_PASSWORD=unified",
			"POSTGRES_DB=unified",
			"listen_addresses=*",
		},
	}, func(c *docker.HostConfig) {
		c.AutoRemove = true
		c.RestartPolicy = docker.RestartPolicy{Name: "no"}
	})
	if err != nil {
		t.Fatalf("dockertest run: %v", err)
	}
	t.Cleanup(func() { _ = pool.Purge(resource) })

	dsn := fmt.Sprintf("postgres://unified:unified@localhost:%s/unified?sslmode=disable",
		resource.GetPort("5432/tcp"))

	pool.MaxWait = 60 * time.Second
	var db *sql.DB
	if err := pool.Retry(func() error {
		var e error
		db, e = sql.Open("pgx", dsn)
		if e != nil {
			return e
		}
		return db.Ping()
	}); err != nil {
		t.Fatalf("postgres not ready: %v", err)
	}
	_ = db.Close()

	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	if err := pg.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}
