package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MetricGauge is one persisted custom gauge series.
type MetricGauge struct {
	Name      string
	Help      string
	Labels    map[string]string
	Value     float64
	UpdatedAt time.Time
}

// MetricRealtime is one running run/step timer exported as now - StartedAt at
// scrape time. StepKey is "" for run-level rows.
type MetricRealtime struct {
	RunID     string
	StepKey   string
	Name      string
	Help      string
	Labels    map[string]string
	StartedAt time.Time
}

// ErrSeriesCap is returned when a new labels_hash would exceed maxSeries for that name.
var ErrSeriesCap = errors.New("custom metric series cap reached")

func marshalLabels(l map[string]string) ([]byte, error) {
	if l == nil {
		l = map[string]string{}
	}
	return json.Marshal(l)
}

func unmarshalLabels(b []byte) map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal(b, &out)
	return out
}

// ApplyMetricGauge applies op ("Set", "Add" or "Sub") to the series identified
// by (g.Name, labelsHash). When maxSeries > 0 and the series does not exist yet,
// it is refused with ErrSeriesCap once the name already has maxSeries series;
// updating an existing series always works. The count-then-insert runs in one
// transaction holding a transaction-scoped advisory lock keyed on the name, so
// concurrent replicas cannot both pass the cap check.
func (p *Postgres) ApplyMetricGauge(ctx context.Context, g MetricGauge, labelsHash, op string, maxSeries int) error {
	var onConflict string
	delta := g.Value
	switch op {
	case "Set":
		onConflict = "value = EXCLUDED.value"
	case "Add":
		onConflict = "value = custom_metric_gauges.value + EXCLUDED.value"
	case "Sub":
		delta = -g.Value
		onConflict = "value = custom_metric_gauges.value + EXCLUDED.value"
	default:
		return fmt.Errorf("unknown gauge op %q", op)
	}
	labels, err := marshalLabels(g.Labels)
	if err != nil {
		return err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if maxSeries > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('custom_metric_gauges/' || $1::text, 0))`, g.Name); err != nil {
			return err
		}
		var exists bool
		var n int
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM custom_metric_gauges WHERE name = $1 AND labels_hash = $2),
			       (SELECT count(*) FROM custom_metric_gauges WHERE name = $1)`,
			g.Name, labelsHash).Scan(&exists, &n); err != nil {
			return err
		}
		if !exists && n >= maxSeries {
			return ErrSeriesCap
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO custom_metric_gauges(name, labels_hash, labels, help, value, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (name, labels_hash) DO UPDATE
		  SET `+onConflict+`,
		      labels     = EXCLUDED.labels,
		      help       = EXCLUDED.help,
		      updated_at = now()`,
		g.Name, labelsHash, labels, g.Help, delta); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListMetricGauges returns the gauges updated at or after since.
func (p *Postgres) ListMetricGauges(ctx context.Context, since time.Time) ([]MetricGauge, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT name, help, labels, value, updated_at
		  FROM custom_metric_gauges
		 WHERE updated_at >= $1
		 ORDER BY name, labels_hash`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricGauge
	for rows.Next() {
		var g MetricGauge
		var labels []byte
		if err := rows.Scan(&g.Name, &g.Help, &labels, &g.Value, &g.UpdatedAt); err != nil {
			return nil, err
		}
		g.Labels = unmarshalLabels(labels)
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteMetricGaugesBefore removes gauges last updated before the cutoff.
func (p *Postgres) DeleteMetricGaugesBefore(ctx context.Context, before time.Time) (int64, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM custom_metric_gauges WHERE updated_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PutMetricRealtime upserts a realtime row. It is a no-op when the run no
// longer exists.
func (p *Postgres) PutMetricRealtime(ctx context.Context, r MetricRealtime) error {
	labels, err := marshalLabels(r.Labels)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO custom_metric_realtime(run_id, step_key, name, labels, help, started_at)
		SELECT r.id, $2, $3, $4, $5, $6 FROM runs r WHERE r.id = $1
		ON CONFLICT (run_id, step_key, name) DO UPDATE
		  SET labels = EXCLUDED.labels, help = EXCLUDED.help, started_at = EXCLUDED.started_at`,
		r.RunID, r.StepKey, r.Name, labels, r.Help, r.StartedAt)
	return err
}

// DeleteMetricRealtime removes the rows of one step of a run. stepKey "" deletes
// the run-level rows only.
func (p *Postgres) DeleteMetricRealtime(ctx context.Context, runID, stepKey string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM custom_metric_realtime WHERE run_id = $1 AND step_key = $2`, runID, stepKey)
	return err
}

// ListMetricRealtime returns every realtime row.
func (p *Postgres) ListMetricRealtime(ctx context.Context) ([]MetricRealtime, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT run_id::text, step_key, name, help, labels, started_at
		  FROM custom_metric_realtime
		 ORDER BY run_id, step_key, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricRealtime
	for rows.Next() {
		var r MetricRealtime
		var labels []byte
		if err := rows.Scan(&r.RunID, &r.StepKey, &r.Name, &r.Help, &labels, &r.StartedAt); err != nil {
			return nil, err
		}
		r.Labels = unmarshalLabels(labels)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteMetricRealtimeForTerminalRuns removes rows whose run is terminal (backstop cleanup).
func (p *Postgres) DeleteMetricRealtimeForTerminalRuns(ctx context.Context) (int64, error) {
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM custom_metric_realtime m
		 USING runs r
		 WHERE r.id = m.run_id AND r.status IN ('Succeeded', 'Failed', 'Cancelled')`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// GetStepVariantOutputs returns only the given variant's outputs of a step
// (variant "" is the non-matrix step).
func (p *Postgres) GetStepVariantOutputs(ctx context.Context, runID string, stepIndex int, variant string) (map[string]string, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT key, value FROM step_outputs WHERE run_id = $1 AND step_index = $2 AND variant = $3`,
		runID, stepIndex, variant)
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
