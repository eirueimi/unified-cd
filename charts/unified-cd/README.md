# unified-cd Helm chart

Installs unified-cd on Kubernetes: the controller, the Kubernetes agent, an
optional job namespace, and (for trials) a bundled PostgreSQL and Garage.

The chart is published at `oci://ghcr.io/eirueimi/charts/unified-cd` (from the
next release on). From a checkout of this repository, use `charts/unified-cd`
instead of the OCI reference in every command below.

Requires Helm 3 and Kubernetes 1.25 or newer.

## Install

### Trial (zero configuration)

```bash
helm install unified-cd oci://ghcr.io/eirueimi/charts/unified-cd \
  --version <version> -n unified-cd --create-namespace

kubectl -n unified-cd port-forward svc/unified-cd-controller 8080:80
```

Open http://localhost:8080 and sign in with the token `dev-token-change-me`.

**The trial is not for production.** It runs a single PostgreSQL and a single
Garage on PersistentVolumeClaims with fixed development credentials. The chart
refuses to render an Ingress while these credentials are in use
(`ingress.enabled with the bundled trial credentials would expose
dev-token-change-me ...`). A Secret you create by hand with development values
cannot be detected, so do not expose one either.

### Production

Production needs your own PostgreSQL and S3-compatible object store, two
Secrets, and HTTPS in front of the controller.

1. Create the controller Secret. It holds the controller's environment and is
   projected whole into the container (`envFrom`):

   ```bash
   kubectl create namespace unified-cd

   kubectl create secret generic unified-cd-controller -n unified-cd \
     --from-literal=UNIFIED_DB_DSN='postgres://...' \
     --from-literal=UNIFIED_TOKEN='...' \
     --from-literal=UNIFIED_S3_ENDPOINT='...' \
     --from-literal=UNIFIED_S3_BUCKET='...' \
     --from-literal=UNIFIED_S3_KEY='...' \
     --from-literal=UNIFIED_S3_SECRET='...'
   ```

   Omitting the four `UNIFIED_S3_*` keys does not stop the controller, but log
   archival and artifacts are then off.

2. Create the key-encryption key (KEK) in its own Secret. It is deliberately
   not a key of the controller Secret: `envFrom` would put it into the process
   environment (`/proc/<pid>/environ`, crash dumps, child processes such as the
   git subprocess). The chart mounts it as a file instead.

   ```bash
   unified-cli keygen --out ./kek
   kubectl create secret generic unified-cd-controller-kek -n unified-cd \
     --from-file=kek=./kek
   ```

   Back `./kek` up somewhere durable, then delete the local file. If the
   Secret is lost, every secret the controller stored becomes permanently
   unreadable. Use a different Secret name with `controller.kek.secretName`.

3. Install, pointing at your Secret and an Ingress with TLS (or put another TLS
   terminator in front of the controller and set `agent.server` to its URL):

   ```bash
   helm install unified-cd oci://ghcr.io/eirueimi/charts/unified-cd \
     --version <version> -n unified-cd \
     --set bundledStores.enabled=false \
     --set controller.existingSecret=unified-cd-controller \
     --set ingress.enabled=true \
     --set ingress.className=nginx \
     --set 'ingress.hosts[0].host=cd.example.com' \
     --set 'ingress.hosts[0].paths[0].path=/' \
     --set 'ingress.hosts[0].paths[0].pathType=Prefix' \
     --set 'ingress.tls[0].secretName=cd-tls' \
     --set 'ingress.tls[0].hosts[0]=cd.example.com' \
     --set agent.server=https://cd.example.com
   ```

   With `bundledStores.enabled=false` the chart renders no Secret and runs no
   KEK Job. Missing Secrets surface as Pod start failures (the chart does not
   use `lookup`). `agent.server`, when set, must be an `https://` URL; with the
   default empty value the agent uses the in-cluster Service over plain HTTP.

   The Ingress does not create certificates. For cert-manager, add its
   annotation through `ingress.annotations`.

### Agent only

For a controller running elsewhere (for example Docker Compose on a host),
install only the agent and the job namespace:

```bash
helm install unified-cd-agent oci://ghcr.io/eirueimi/charts/unified-cd \
  --version <version> -n unified-cd --create-namespace \
  --set controller.enabled=false \
  --set bundledStores.enabled=false \
  --set agent.server=https://controller.example.com
```

Configure the external controller's in-cluster verifier and enrollment policy
first; see `docs/operator-manual/kubernetes-integration.md`.

## Behavior to know

- **One controller release per cluster.** Resource names are fixed
  (`unified-cd-controller`, `unified-cd-k8s-agent`, `postgres`, `garage`, ...),
  not prefixed with the release name, and the ClusterRole and ClusterRoleBinding
  `unified-cd-controller-kubernetes-enrollment` are cluster-scoped, so a second
  controller release collides with the first even in another namespace. The
  default job namespace `ci` is cluster-wide as well: a second release (for
  example an agent-only one) needs a different `jobNamespace.name`.
- **A failed KEK hook leaves its objects behind.** If the `unified-cd-kek-init`
  hook fails, its Job, ServiceAccount, Role and RoleBinding stay in the namespace
  (the `before-hook-creation` policy only replaces them on the next install or
  upgrade) and `helm uninstall` does not remove them. Delete them with
  `kubectl -n <ns> delete job,serviceaccount,role,rolebinding unified-cd-kek-init`.
- **Configuration merges.** `controller.config` and `agent.config` are
  deep-merged over the chart-computed defaults. Maps merge key by key and your
  value always wins, including `false`, `0` and `""`. A list you supply
  replaces the computed list entirely (for example
  `agentAuth.kubernetesEnrollmentPolicies`, or the agent `labels`).
- **Config changes restart Pods.** The Deployments carry `checksum/config`
  annotations, so editing `controller.config` or `agent.config` rolls the Pods.
- **KEK Job.** With the bundled stores (or `controller.kek.generate=true`) a
  Job generates the KEK once. It is a `pre-install,pre-upgrade` Helm hook, so
  `helm install --wait` works, and it uses `kubectl create`, which never
  replaces an existing Secret: reinstalling and `helm upgrade` keep the key.
  Set `controller.kek.job.helmHook=false` to render it as a plain resource.
- **The KEK survives `helm uninstall`.** The Secret is created by the Job, not
  by Helm, so Helm does not own or delete it. This keeps stored secrets
  readable across a reinstall. To remove it deliberately:

  ```bash
  kubectl -n <ns> delete secret unified-cd-controller-kek
  ```

  After that, secrets encrypted with the old key cannot be read again.
- **The job namespace is kept on uninstall.** It is annotated
  `helm.sh/resource-policy: keep`, so uninstalling never deletes running job
  Pods or their volumes. It is rendered whenever `jobNamespace.create` is
  `true`, even with `agent.enabled=false`, because the controller's
  store-credential policy references it. The release namespace is never
  created by the chart: use `--create-namespace`.
- **Trial data goes with the release.** The bundled stores' PersistentVolumeClaims
  (`postgres-data`, `garage-data`) are Helm-owned and are deleted by
  `helm uninstall`, together with the data. Only the KEK Secret and the job
  namespace are kept.

## Values

| Key | Default | Description |
| --- | --- | --- |
| `controller.enabled` | `true` | Install the controller Deployment, Service and RBAC. |
| `controller.image.repository` | `ghcr.io/eirueimi/unified-cd-controller` | Controller image. |
| `controller.image.tag` | `""` | Image tag; empty uses the chart `appVersion`. |
| `controller.image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `controller.replicas` | `1` | Controller replicas (stateless; leader election via PostgreSQL). |
| `controller.existingSecret` | `""` | Secret with the controller environment. Empty with `bundledStores.enabled` renders the trial Secret; otherwise this name (default `unified-cd-controller`) is referenced, never created. |
| `controller.kek.secretName` | `unified-cd-controller-kek` | Secret holding the KEK, mounted as a file. |
| `controller.kek.generate` | `null` | Run the KEK Job. `null` follows `bundledStores.enabled`; `true` or `false` overrides. |
| `controller.kek.job.helmHook` | `true` | Run the KEK Job as a `pre-install,pre-upgrade` hook. |
| `controller.kek.job.image` | `alpine/k8s:1.31.2` | Image providing `kubectl` for the KEK Job. |
| `controller.config` | `{}` | Deep-merged over the computed controller config (see above). |
| `controller.service.type` | `ClusterIP` | Controller Service type. HTTP only; terminate TLS at an Ingress. |
| `controller.service.port` | `80` | Controller Service port. |
| `controller.resources` | requests 100m / 256Mi, memory limit 512Mi | Controller container resources. |
| `controller.nodeSelector` | `{}` | Controller Pod node selector. |
| `controller.tolerations` | `[]` | Controller Pod tolerations. |
| `controller.affinity` | `{}` | Controller Pod affinity. |
| `controller.podAnnotations` | `{}` | Extra controller Pod annotations. |
| `agent.enabled` | `true` | Install the Kubernetes agent Deployment and RBAC. |
| `agent.image.repository` | `ghcr.io/eirueimi/unified-cd-k8s-agent` | Agent image. |
| `agent.image.tag` | `""` | Image tag; empty uses the chart `appVersion`. |
| `agent.image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `agent.replicas` | `2` | Agent replicas. |
| `agent.server` | `""` | Controller URL. Empty uses the in-cluster Service over plain HTTP with `allowInsecureHTTP: true` whenever the controller is installed (not only in trial mode; NOTES warns when `bundledStores.enabled=false`); otherwise it must start with `https://`. |
| `agent.config` | `{}` | Deep-merged over the computed agent config (see above). |
| `agent.resources` | requests 50m / 128Mi, memory limit 256Mi | Agent container resources. |
| `agent.nodeSelector` | `{}` | Agent Pod node selector. |
| `agent.tolerations` | `[]` | Agent Pod tolerations. |
| `agent.affinity` | `{}` | Agent Pod affinity. |
| `agent.podAnnotations` | `{}` | Extra agent Pod annotations. |
| `jobNamespace.create` | `true` | Create the namespace where job Pods run. |
| `jobNamespace.name` | `ci` | Job namespace name. |
| `jobNamespace.podSecurity.enforce` | `baseline` | Pod Security Admission `enforce` level (`privileged`, `baseline` or `restricted`). |
| `jobNamespace.podSecurity.audit` | `restricted` | Pod Security Admission `audit` level. |
| `bundledStores.enabled` | `true` | Install the trial PostgreSQL, Garage and trial Secret. Requires `controller.enabled`. Not for production. |
| `bundledStores.postgres.image` | `postgres:16-alpine` | PostgreSQL image. |
| `bundledStores.postgres.storage` | `5Gi` | PostgreSQL PVC size. |
| `bundledStores.postgres.storageClassName` | `""` | PostgreSQL StorageClass; empty uses the cluster default. |
| `bundledStores.garage.image` | `dxflrs/garage:v2.3.0` | Garage image. |
| `bundledStores.garage.storage` | `10Gi` | Garage PVC size. |
| `bundledStores.garage.storageClassName` | `""` | Garage StorageClass; empty uses the cluster default. |
| `ingress.enabled` | `false` | Create an Ingress for the controller. Refused with the trial credentials. |
| `ingress.className` | `""` | `ingressClassName`; empty uses the cluster default. |
| `ingress.annotations` | `{}` | Ingress annotations. |
| `ingress.hosts` | `[]` | Hosts and paths: `[{host, paths: [{path, pathType}]}]`. |
| `ingress.tls` | `[]` | TLS entries: `[{secretName, hosts}]`. |

`values.schema.json` validates types and the Pod Security levels at install
time. Image tags are typed as strings, so a numeric-looking tag such as `1.10`
needs `--set-string controller.image.tag=1.10` (plain `--set` makes it a number
and the schema rejects it).

## Render-time errors

The chart fails to render, naming the value to change, when:

- both `controller.enabled` and `agent.enabled` are `false`
  (`nothing to install: ...`);
- `bundledStores.enabled` is `true` with `controller.enabled=false`;
- `agent.server` is empty with `controller.enabled=false`;
- `agent.server` is set and is not an `https://` URL;
- `ingress.enabled` is `true` with `controller.enabled=false` (the Ingress routes
  to the controller Service; the message names `controller.enabled`);
- `ingress.enabled` is `true` while the trial Secret would be rendered;
- `ingress.enabled` is `true` with an empty `ingress.hosts`.
