# Kubernetes Install Manifests

The Kubernetes install manifests are generated from the Helm chart in
[`charts/unified-cd`](../charts/unified-cd/README.md). This directory holds no
manifests of its own: the chart is the single source for `helm install` and for
the three release bundles.

## Release bundles

Each GitHub Release attaches three pre-rendered bundles for `kubectl apply -f`.
Every bundle starts with the `unified-cd` Namespace, followed by the chart
rendered into that namespace with the matching values file in
[`charts/unified-cd/bundles/`](../charts/unified-cd/bundles/).

| File | Values file | Contents | Prerequisites |
|------|-------------|----------|---------------|
| `install.yaml` | `install.values.yaml` | controller + k8s-agent + in-cluster PostgreSQL and Garage with development-default credentials | None. For evaluation only; **do not use in production.** |
| `core-install.yaml` | `core-install.values.yaml` | controller + k8s-agent | External PostgreSQL and S3-compatible store, a TLS terminator, and the `unified-cd-controller` Secret. Replace `https://controller.example.invalid` with your HTTPS endpoint. |
| `agent-only.yaml` | `agent-only.values.yaml` | k8s-agent only | A controller running elsewhere with the matching Kubernetes enrollment policy. Replace `https://controller.example.invalid` with its HTTPS endpoint. |

```bash
kubectl apply -f https://github.com/eirueimi/unified-cd/releases/latest/download/install.yaml
```

The bundles run the secret-encryption-key (KEK) Job as a plain Job, because
`kubectl apply` has no hooks. A Helm install runs it as a `pre-install` hook
instead. Both create the `unified-cd-controller-kek` Secret once and never
replace it.

For production, prefer `helm install` over the bundles. The chart README covers
external stores, `controller.existingSecret`, the KEK Secret, ingress, and the
agent-only layout.

## Building the bundles locally

```bash
make manifests                                       # writes dist/manifests/ (needs helm)
scripts/build-manifests.sh dist/manifests v0.0.0-test  # optional image tag
```

`scripts/build-manifests.sh <output-dir> [image-tag]` renders the three files
with `helm template`. With an image tag, both first-party images are pinned to
it; this is what the release workflow does. Without one, the images follow the
chart's `appVersion` (`latest` in a checkout). Nothing built here is committed.

## Related documentation

- [Chart README](../charts/unified-cd/README.md): values, install modes and render-time errors
- [Kubernetes Integration Guide](../docs/operator-manual/kubernetes-integration.md): k8s-agent podTemplate configuration
- [High Availability (HA) Guide](../docs/operator-manual/high-availability.md): controller scale-out and leader election
- [Secrets Management Guide](../docs/user-guide/secrets.md): the KEK, Vault and OpenBao
