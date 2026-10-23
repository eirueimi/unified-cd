# Troubleshooting

Find the symptom you saw. Every entry links straight to the section that
covers it.

**Runs and Scheduling**

- [Run stays `Queued` forever](runs-and-scheduling.md#run-stays-queued-forever)
- [Run failed with "no eligible agent available to claim it"](runs-and-scheduling.md#run-failed-with-no-eligible-agent-available-to-claim-it)
- [Job stays Queued / unschedulable warning](runs-and-scheduling.md#job-stays-queued-unschedulable-warning)
- [Job stays Queued: no agent declares volume](runs-and-scheduling.md#job-stays-queued-no-agent-declares-volume)
- [Run stays Queued: the volume it mounts is below `minFreeDisk`](runs-and-scheduling.md#run-stays-queued-the-volume-it-mounts-is-below-minfreedisk)
- [Run marked `Failed` with "agent lost"](runs-and-scheduling.md#run-marked-failed-with-agent-lost)
- [Run marked `Failed` by heartbeat reconcile after a lost claim](runs-and-scheduling.md#run-marked-failed-by-heartbeat-reconcile-after-a-lost-claim)
- [Approve/reject returns 409 `run is already terminal` or `approval window has expired`](runs-and-scheduling.md#approvereject-returns-409-run-is-already-terminal-or-approval-window-has-expired)

**Steps and Execution**

- [Run fails with `dynamic secret name must be resolved from a parameter before execution`](steps-and-execution.md#run-fails-with-dynamic-secret-name-must-be-resolved-from-a-parameter-before-execution)
- [Job isolation](steps-and-execution.md#job-isolation)
- [Step failed because its `if:` could not be evaluated](steps-and-execution.md#step-failed-because-its-if-could-not-be-evaluated)
- [A step's log shows `step panicked: ...`](steps-and-execution.md#a-steps-log-shows-step-panicked)
- [A `finally` step fails with `context deadline exceeded` and the run is `Failed`](steps-and-execution.md#a-finally-step-fails-with-context-deadline-exceeded-and-the-run-is-failed)
- [A cancelled run finished `Failed` instead of `Cancelled`](steps-and-execution.md#a-cancelled-run-finished-failed-instead-of-cancelled)
- [Run fails with `prepare workspace failed: clean workspace ...`](steps-and-execution.md#run-fails-with-prepare-workspace-failed-clean-workspace)
- [A run waits for a volume another run holds](steps-and-execution.md#a-run-waits-for-a-volume-another-run-holds)
- [A volume stays held after its run crashed](steps-and-execution.md#a-volume-stays-held-after-its-run-crashed)
- [Run fails: volume was last used by a native/isolated run](steps-and-execution.md#run-fails-volume-was-last-used-by-a-nativeisolated-run)
- [Run fails: `already exists and was not created by this agent as a volume link`](steps-and-execution.md#run-fails-already-exists-and-was-not-created-by-this-agent-as-a-volume-link)
- [Run fails: `cannot link ... on Windows`](steps-and-execution.md#run-fails-cannot-link-on-windows)
- [Run fails: volume is shared and cannot be mounted at the workspace root](steps-and-execution.md#run-fails-volume-is-shared-and-cannot-be-mounted-at-the-workspace-root)
- [A shared volume never binds (RWX unavailable)](steps-and-execution.md#a-shared-volume-never-binds-rwx-unavailable)
- [Permission denied writing to a volume as a non-root user](steps-and-execution.md#permission-denied-writing-to-a-volume-as-a-non-root-user)
- [Run fails: `is not mapped by this agent's rootless container runtime`](steps-and-execution.md#run-fails-is-not-mapped-by-this-agents-rootless-container-runtime)
- [Run fails: `container has runAsNonRoot and image will run as root`](steps-and-execution.md#run-fails-container-has-runasnonroot-and-image-will-run-as-root)
- [A securityContext job stays Queued: `container runtime cannot honour securityContext`](steps-and-execution.md#a-securitycontext-job-stays-queued-container-runtime-cannot-honour-securitycontext)
- [Agent log: `400 unknown capability: securitycontext`](steps-and-execution.md#agent-log-400-unknown-capability-securitycontext)

**Artifacts and Storage**

- [Step fails with `requires S3 configuration (UNIFIED_S3_*)`](artifacts-and-storage.md#step-fails-with-requires-s3-configuration-unified_s3_)
- [Job pods never start with `CreateContainerConfigError` (`secret "…" not found`)](artifacts-and-storage.md#job-pods-never-start-with-createcontainerconfigerror)
- [Kubernetes run fails: pod was evicted: Usage of EmptyDir volume "workspace" exceeds the limit](artifacts-and-storage.md#kubernetes-run-fails-pod-was-evicted-usage-of-emptydir-volume-workspace-exceeds-the-limit)
- [k8s pod `ImagePullBackOff` on `unified-artifact`](artifacts-and-storage.md#k8s-pod-imagepullbackoff-on-unified-artifact)
- [A sidecar failed to start](artifacts-and-storage.md#a-sidecar-failed-to-start)
- [Artifact step fails `no such file`](artifacts-and-storage.md#artifact-step-fails-no-such-file)
- [`artifact download` fails](artifacts-and-storage.md#artifact-download-fails)
- [Step fails with `artifact/cache path ... escapes the workspace`](artifacts-and-storage.md#step-fails-with-artifactcache-path-escapes-the-workspace)
- [An artifact or cache path leaves a volume's contents out](artifacts-and-storage.md#an-artifact-or-cache-path-leaves-a-volumes-contents-out)
- [Artifact upload fails or a cache is never restored: `flag provided but not defined: -exclude`](artifacts-and-storage.md#artifact-upload-fails-or-a-cache-is-never-restored-flag-provided-but-not-defined-exclude)

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
- [An agent fails at startup with a `volume ...` configuration error](agents-and-enrollment.md#an-agent-fails-at-startup-with-a-volume-configuration-error)
- [A Kubernetes agent fails at startup with `workspace.pvc was removed`](agents-and-enrollment.md#a-kubernetes-agent-fails-at-startup-with-workspacepvc-was-removed)
- [A host agent is not claiming: `free disk space below minimum, skipping claim`](agents-and-enrollment.md#a-host-agent-is-not-claiming-free-disk-space-below-minimum-skipping-claim)

**Controller and Database**

- [Controller logs `dropping log line for sealed run`](controller-and-database.md#controller-logs-dropping-log-line-for-sealed-run)
- [A run's log shows `[N log line(s) dropped: controller unreachable]`](controller-and-database.md#a-runs-log-shows-n-log-lines-dropped-controller-unreachable)
- [Controller `/readyz` returns `503 db unavailable`](controller-and-database.md#controller-readyz-returns-503-db-unavailable)
- [Controller fails at startup with `schema drift: ... does not exist`](controller-and-database.md#controller-fails-at-startup-with-schema-drift-does-not-exist)
- [Dev stack: controller container unhealthy, `vendor/modules.txt` errors](controller-and-database.md#dev-stack-controller-container-unhealthy-vendormodulestxt-errors)
- [Local Kubernetes won't start (`kubelet is not healthy`)](controller-and-database.md#local-kubernetes-wont-start-kubelet-is-not-healthy)
- [Schema drift (migration renumbering)](controller-and-database.md#schema-drift-migration-renumbering)
