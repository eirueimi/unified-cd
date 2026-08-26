package store

import "github.com/jackc/pgx/v5/pgxpool"

// PoolStat is one connection pool's saturation, in the terms an operator
// needs to answer "is the controller waiting on Postgres?".
//
// The four pools are separately bounded so background work cannot starve the
// API (see DefaultPostgresPoolConfig). That isolation was the point of the
// split, and until these numbers are exported there is no way to tell whether
// it is holding: a BOUNDED pool under pressure does not error, it queues, so
// the only symptom is latency with no accompanying signal.
type PoolStat struct {
	// AcquiredConns is connections currently checked out.
	AcquiredConns int32
	// IdleConns is established connections sitting unused.
	IdleConns int32
	// TotalConns is AcquiredConns + IdleConns + those still being constructed.
	TotalConns int32
	// MaxConns is the configured ceiling. AcquiredConns/MaxConns is the
	// saturation ratio to alert on.
	MaxConns int32
	// EmptyAcquireCount is the cumulative number of acquires that found no
	// free connection and had to wait. A rising rate here is the pool telling
	// you it is too small, and it is the signal that a bounded pool otherwise
	// never gives.
	EmptyAcquireCount int64
	// CanceledAcquireCount is acquires abandoned because the caller's context
	// ended first — a request that gave up waiting for a connection.
	CanceledAcquireCount int64
}

// PoolStats reports per-pool saturation, keyed by the pool names used in
// NewPostgresWithPoolConfig ("api", "background", "lock", "listen").
//
// A pool that is nil is omitted rather than reported as zero: a Postgres
// built to share another's pools has no numbers of its own to give, and a
// zeroed entry would read as a perfectly idle pool instead of an absent one.
func (p *Postgres) PoolStats() map[string]PoolStat {
	out := make(map[string]PoolStat, 4)
	for name, pool := range map[string]*pgxpool.Pool{
		"api":        p.pool,
		"background": p.backgroundPool,
		"lock":       p.lockPool,
		"listen":     p.listenPool,
	} {
		if pool == nil {
			continue
		}
		s := pool.Stat()
		out[name] = PoolStat{
			AcquiredConns:        s.AcquiredConns(),
			IdleConns:            s.IdleConns(),
			TotalConns:           s.TotalConns(),
			MaxConns:             s.MaxConns(),
			EmptyAcquireCount:    s.EmptyAcquireCount(),
			CanceledAcquireCount: s.CanceledAcquireCount(),
		}
	}
	return out
}
