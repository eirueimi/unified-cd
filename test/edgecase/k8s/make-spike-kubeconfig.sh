#!/usr/bin/env bash
# W4-0 enrollment spike: generate the kubeconfig the compose controllers use to
# reach the local kind cluster.
#
# The output carries a live ServiceAccount bearer token, so it is gitignored
# (see .gitignore). This script is the committed artifact; the kubeconfig it
# produces is not. Re-run it whenever the token expires.
#
# Prerequisites:
#   kubectl apply -f test/edgecase/k8s/w4-spike-controller-rbac.yaml
#
# Why https://desktop-control-plane:6443 and not host.docker.internal:
# the apiserver serving certificate's SANs are
#   DNS:desktop-control-plane, DNS:kubernetes[...], DNS:localhost,
#   IP:10.96.0.1, IP:172.18.0.4, IP:127.0.0.1
# so the node's own container name verifies cleanly from any container attached
# to the `kind` bridge network. No insecure-skip-tls-verify is required.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
out="${repo_root}/test/edgecase/compose/kubeconfig-k8senroll.yaml"
node="${W4_KIND_NODE:-desktop-control-plane}"
server="${W4_KIND_SERVER:-https://${node}:6443}"
sa_ns="${W4_CONTROLLER_SA_NAMESPACE:-unified-cd}"
sa="${W4_CONTROLLER_SA:-w4-spike-controller}"
ttl="${W4_TOKEN_TTL:-24h}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT

# The cluster CA certificate is public material; it is copied out of the node
# rather than out of the developer's kubeconfig so this script never reads the
# admin client certificate.
docker cp "${node}:/etc/kubernetes/pki/ca.crt" "${tmpdir}/ca.crt" >/dev/null

rm -f "${out}"
export KUBECONFIG="${out}"
kubectl config set-cluster w4-spike \
  --server="${server}" \
  --certificate-authority="${tmpdir}/ca.crt" \
  --embed-certs=true >/dev/null
kubectl config set-credentials w4-spike \
  --token="$(KUBECONFIG= kubectl create token "${sa}" -n "${sa_ns}" --duration="${ttl}")" >/dev/null
kubectl config set-context w4-spike --cluster=w4-spike --user=w4-spike >/dev/null
kubectl config use-context w4-spike >/dev/null

echo "wrote ${out} (server=${server}, sa=${sa_ns}/${sa}, ttl=${ttl})"
