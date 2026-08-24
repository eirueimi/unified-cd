# Troubleshooting

Find the symptom you saw. Every entry links straight to the section that
covers it.

**Runs and Scheduling**

- [Run stays `Queued` forever](runs-and-scheduling.md#run-stays-queued-forever)
- [Run failed with "no eligible agent available to claim it"](runs-and-scheduling.md#run-failed-with-no-eligible-agent-available-to-claim-it)
- [Job stays Queued / unschedulable warning](runs-and-scheduling.md#job-stays-queued-unschedulable-warning)
- [Run marked `Failed` with "agent lost"](runs-and-scheduling.md#run-marked-failed-with-agent-lost)
- [Run marked `Failed` by heartbeat reconcile after a lost claim](runs-and-scheduling.md#run-marked-failed-by-heartbeat-reconcile-after-a-lost-claim)
- [Approve/reject returns 409 `run is already terminal` or `approval window has expired`](runs-and-scheduling.md#approvereject-returns-409-run-is-already-terminal-or-approval-window-has-expired)

**Steps and Execution**

- [Run fails with `dynamic secret name must be resolved from a parameter before execution`](steps-and-execution.md#run-fails-with-dynamic-secret-name-must-be-resolved-from-a-parameter-before-execution)
- [Job isolation](steps-and-execution.md#job-isolation)
- [Conditional step ran when it shouldn't](steps-and-execution.md#conditional-step-ran-when-it-shouldnt)
- [A step's log shows `step panicked: ...`](steps-and-execution.md#a-steps-log-shows-step-panicked)

**Artifacts and Storage**

- [k8s pod `ImagePullBackOff` on `unified-artifact`](artifacts-and-storage.md#k8s-pod-imagepullbackoff-on-unified-artifact)
- [A sidecar failed to start](artifacts-and-storage.md#a-sidecar-failed-to-start)
- [Artifact step fails `no such file`](artifacts-and-storage.md#artifact-step-fails-no-such-file)
- [`artifact download` fails](artifacts-and-storage.md#artifact-download-fails)
- [Step fails with `artifact/cache path ... escapes the workspace`](artifacts-and-storage.md#step-fails-with-artifactcache-path-escapes-the-workspace)

**Templates and `uses`**

- [`uses:` run fails with `uses: targets must be kind: JobTemplate`](templates-and-uses.md#uses-run-fails-with-uses-targets-must-be-kind-jobtemplate)
- [Scoped `uses` step can't find workspace files](templates-and-uses.md#scoped-uses-step-cant-find-workspace-files)
- [Job fails apply with a dangling `container:` reference](templates-and-uses.md#job-fails-apply-with-a-dangling-container-reference)
- [`podTemplate` container/volume name rejected as an invalid DNS-1123 label](templates-and-uses.md#podtemplate-containervolume-name-rejected-as-an-invalid-dns-1123-label)
- [`uses: git://...` job fails to resolve with invalid characters](templates-and-uses.md#uses-git-job-fails-to-resolve-with-invalid-characters)
- [Run fails with log line `git template resolution failed for more than 1h0m0s`](templates-and-uses.md#run-fails-with-log-line-git-template-resolution-failed-for-more-than-1h0m0s)

**Webhooks**

- [Webhook returns 401](webhooks.md#webhook-returns-401)
- [Webhook returns 400 `invalid JSON payload`](webhooks.md#webhook-returns-400-invalid-json-payload)
- [Webhook returns 400 `missing required param`](webhooks.md#webhook-returns-400-missing-required-param)

**Agents and Enrollment**

- [Agent requests fail with 403 `run <id> is claimed by another agent`](agents-and-enrollment.md#agent-requests-fail-with-403-run-id-is-claimed-by-another-agent)
- [Every agent reports `"version": "dev"`](agents-and-enrollment.md#every-agent-reports-version-dev)
- [Agent enrollment and credentials](agents-and-enrollment.md#agent-enrollment-and-credentials)

**Controller and Database**

- [Controller logs `dropping log line for sealed run`](controller-and-database.md#controller-logs-dropping-log-line-for-sealed-run)
- [A run's log shows `[N log line(s) dropped: controller unreachable]`](controller-and-database.md#a-runs-log-shows-n-log-lines-dropped-controller-unreachable)
- [Controller `/readyz` returns `503 db unavailable`](controller-and-database.md#controller-readyz-returns-503-db-unavailable)
- [Controller fails at startup with `schema drift: ... does not exist`](controller-and-database.md#controller-fails-at-startup-with-schema-drift-does-not-exist)
- [Dev stack: controller container unhealthy, `vendor/modules.txt` errors](controller-and-database.md#dev-stack-controller-container-unhealthy-vendormodulestxt-errors)
- [Local Kubernetes won't start (`kubelet is not healthy`)](controller-and-database.md#local-kubernetes-wont-start-kubelet-is-not-healthy)
- [Schema drift (migration renumbering)](controller-and-database.md#schema-drift-migration-renumbering)
