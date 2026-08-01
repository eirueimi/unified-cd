#!/usr/bin/env bash
# W4-2 (reuse swallow): mint the kubeconfig that gives the HOST-RUN Kubernetes
# agent a credential WITHOUT `pods update` / `pods patch`.
#
# The output carries a live ServiceAccount bearer token, so it is gitignored
# (see .gitignore). THIS SCRIPT is the committed artifact; its output is not.
# Re-run it whenever the token expires (default TTL 24h).
#
# Why this works at all, and why no in-cluster Deployment is needed:
# cmd/k8s-agent/main.go:96-105 buildRestConfig passes cfg.Kubeconfig
# (internal/k8sagent/config.go:36) straight to clientcmd.BuildConfigFromFlags,
# and Config.Validate (config.go:166-226) never touches the field — no default,
# no existence check, no format check. A `users: [{user: {token: ...}}]`
# kubeconfig is an ordinary clientcmd credential. main.go:66-68 then hands the
# resulting client to NewPodManager, NewExecutor AND NewPodPool, so the pool's
# Update calls go through exactly this credential.
#
# Prerequisites:
#   kubectl apply -f test/edgecase/k8s/w4-2-reuse-denied-rbac.yaml
#
# Server URL: Docker Desktop publishes the apiserver on a DYNAMIC host port
# (127.0.0.1:<port>), so the default below is read out of the developer's own
# current kubeconfig rather than hardcoded. The node serving certificate's SANs
# include DNS:localhost and IP:127.0.0.1, so no insecure-skip-tls-verify is
# needed. Do NOT copy make-spike-kubeconfig.sh's
# `https://desktop-control-plane:6443` here: that name resolves from a
# container on the kind bridge, not from a host process.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
out="${W4_2_KUBECONFIG:-${repo_root}/test/edgecase/k8s/.w4-2-restricted.kubeconfig}"
node="${W4_KIND_NODE:-desktop-control-plane}"
server="${W4_2_SERVER:-$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')}"
sa_ns="${W4_2_SA_NAMESPACE:-ci}"
sa="${W4_2_SA:-w4-2-reuse-denied}"
ttl="${W4_TOKEN_TTL:-24h}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT

# The cluster CA certificate is public material; it is copied out of the node
# rather than out of the developer's kubeconfig so this script never reads the
# admin client certificate. MSYS_NO_PATHCONV + cygpath: the standing rule for
# this rig is to disable Git-Bash path mangling on colon-bearing docker paths,
# which then requires the HOST side of `docker cp` to be a Windows path.
if command -v cygpath >/dev/null 2>&1; then
  MSYS_NO_PATHCONV=1 docker cp "${node}:/etc/kubernetes/pki/ca.crt" "$(cygpath -w "${tmpdir}/ca.crt")" >/dev/null
else
  docker cp "${node}:/etc/kubernetes/pki/ca.crt" "${tmpdir}/ca.crt" >/dev/null
fi

rm -f "${out}"
export KUBECONFIG="${out}"
kubectl config set-cluster w4-2-restricted \
  --server="${server}" \
  --certificate-authority="${tmpdir}/ca.crt" \
  --embed-certs=true >/dev/null
kubectl config set-credentials w4-2-restricted \
  --token="$(KUBECONFIG= kubectl create token "${sa}" -n "${sa_ns}" --duration="${ttl}")" >/dev/null
kubectl config set-context w4-2-restricted \
  --cluster=w4-2-restricted --user=w4-2-restricted --namespace="${sa_ns}" >/dev/null
kubectl config use-context w4-2-restricted >/dev/null
chmod 600 "${out}" 2>/dev/null || true

echo "wrote ${out} (server=${server}, sa=${sa_ns}/${sa}, ttl=${ttl})"
echo "-- verb check, as the API server sees this identity --"
for v in get list create delete update patch watch; do
  printf '   %-7s %s\n' "${v}" \
    "$(KUBECONFIG= kubectl auth can-i "${v}" pods -n "${sa_ns}" --as="system:serviceaccount:${sa_ns}:${sa}" 2>&1)"
done
