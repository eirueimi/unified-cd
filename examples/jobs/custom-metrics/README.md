# Custom metrics example

`job.yaml` declares job-defined Prometheus metrics in `metrics:` blocks:

- `demo_runs` is a counter labelled by run status, exposed as `unifiedcd_job_demo_runs_total`;
- `demo_duration_seconds` is a histogram of the run duration with explicit buckets;
- `demo_running_seconds` is a realtime gauge: it shows the elapsed time of each running run and disappears when the run finishes;
- `demo_test_coverage_ratio` is a step-level gauge set from the `coverage` output of the `test` step, only when that step succeeded.

```bash
unified-cd apply -f examples/jobs/custom-metrics/job.yaml
unified-cd run trigger custom-metrics-demo                      # status="Succeeded"
unified-cd run trigger custom-metrics-demo --param fail=true    # status="Failed"
curl -s http://localhost:8080/metrics | grep unifiedcd_job_
```

Run-level samples appear when the run reaches a terminal status; step-level samples appear when the step does. Every exposed name starts with `unifiedcd_job_`. Controllers with several replicas must be scraped one by one and queried with `sum(rate(...))` for counters and histograms and `max by (...)` for gauges. The rules, template fields, limits and cardinality advice are in
[Custom Metrics](../../../docs/user-guide/writing-jobs/custom-metrics.md).
