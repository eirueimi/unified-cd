# Migrating to operator-created Kubernetes Secrets

`core-install.yaml` no longer ships the `unified-cd-controller` and
`unified-cd-controller-kek` Secrets. Previously the bundle carried both,
pre-filled with `REPLACE_WITH_*` placeholder values that an operator edited
in place before applying — which invites live credentials into whatever
version control tracks the edited manifest. The bundle now references both
Secrets by name (`envFrom` for the first, a volume mount for the second) but
creates neither; an operator creates them with `kubectl create secret`.

**An existing installation needs no action.** `kubectl apply -f` does not
prune resources that disappear from a manifest, so the Secrets already in
your cluster are untouched by upgrading to a bundle that no longer contains
them. The Secret names are unchanged, so the Deployment's `envFrom` and
volume-mount references still resolve to the same objects. Re-applying the
new `core-install.yaml` is safe.

| Before | After |
|---|---|
| `core-install.yaml` contained both Secrets, pre-filled with `REPLACE_WITH_*` placeholders. | `core-install.yaml` contains no Secret resources. |
| An operator edited the placeholders in the manifest and applied it. | An operator runs `kubectl create secret` before applying the manifest. |
| Editing the manifest in place risked committing live credentials to version control. | Credentials never appear in the manifest. |
| Consumption mechanism: `envFrom` for `unified-cd-controller`, a `defaultMode: 0400` volume mount for `unified-cd-controller-kek`. | Unchanged. |

## Fresh install

Create the namespace and both Secrets before applying `core-install.yaml`:

```bash
kubectl create namespace unified-cd

kubectl create secret generic unified-cd-controller -n unified-cd \
  --from-literal=UNIFIED_DB_DSN='postgres://...' \
  --from-literal=UNIFIED_TOKEN='...' \
  --from-literal=UNIFIED_S3_ENDPOINT='...' \
  --from-literal=UNIFIED_S3_BUCKET='...' \
  --from-literal=UNIFIED_S3_KEY='...' \
  --from-literal=UNIFIED_S3_SECRET='...'

unified-cli keygen --out ./kek
kubectl create secret generic unified-cd-controller-kek -n unified-cd \
  --from-file=kek=./kek
```

See [Kubernetes Install Manifests: Creating the controller Secrets](https://github.com/eirueimi/unified-cd/blob/main/manifests/README.md#creating-the-controller-secrets)
for why the KEK uses `--from-file` instead of `--from-literal`, and for the
Vault/OpenBao and SSO variants of this command.

## Expected error if you skip it

If `core-install.yaml` is applied without first creating `unified-cd-controller`,
the controller Pod stays in `CreateContainerConfigError`, with an event reading:

```
secret "unified-cd-controller" not found
```

Create the Secret and the Pod will start on its next reconcile; no restart of
the Deployment is needed.
