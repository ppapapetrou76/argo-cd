#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLUSTER_NAME="argocd-bug-27259"
PF_PID_FILE="${SCRIPT_DIR}/argocd-pf.pid"
PF_LOG_FILE="${SCRIPT_DIR}/argocd-pf.log"

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "[ERROR] Required command not found: $1" >&2
    exit 1
  }
}

for cmd in k3d kubectl helm kustomize argocd base64; do
  require_cmd "$cmd"
done

if ! k3d cluster get "${CLUSTER_NAME}" >/dev/null 2>&1; then
  echo "[INFO] Creating k3d cluster ${CLUSTER_NAME}"
  k3d cluster create "${CLUSTER_NAME}" --wait
else
  echo "[INFO] k3d cluster ${CLUSTER_NAME} already exists"
fi

echo "[INFO] Ensuring namespace argocd exists"
kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f -

echo "[INFO] Installing Argo CD v2.13.0"
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/v2.13.0/manifests/install.yaml

echo "[INFO] Waiting for Argo CD workloads to be ready"
for deploy in argocd-applicationset-controller argocd-dex-server argocd-notifications-controller argocd-redis argocd-repo-server argocd-server; do
  kubectl -n argocd rollout status "deployment/${deploy}" --timeout=300s
done
kubectl -n argocd rollout status statefulset/argocd-application-controller --timeout=300s

password="$(kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d)"
echo "[INFO] Initial admin password: ${password}"

if [[ -f "${PF_PID_FILE}" ]] && kill -0 "$(cat "${PF_PID_FILE}")" >/dev/null 2>&1; then
  echo "[INFO] Existing port-forward process detected; stopping it"
  kill "$(cat "${PF_PID_FILE}")"
  rm -f "${PF_PID_FILE}"
fi

echo "[INFO] Starting port-forward on localhost:8080 -> argocd-server:443"
kubectl -n argocd port-forward svc/argocd-server 8080:443 >"${PF_LOG_FILE}" 2>&1 &
PF_PID=$!
echo "${PF_PID}" > "${PF_PID_FILE}"
sleep 3

if ! kill -0 "${PF_PID}" >/dev/null 2>&1; then
  echo "[ERROR] Port-forward failed to start. See ${PF_LOG_FILE}" >&2
  exit 1
fi

echo "[INFO] Logging in via argocd CLI"
argocd login localhost:8080 --username admin --password "${password}" --insecure --grpc-web

echo
echo "[SUCCESS] Argo CD setup complete"
echo "  Cluster: ${CLUSTER_NAME}"
echo "  Namespace: argocd"
echo "  Port-forward PID file: ${PF_PID_FILE}"
echo "  Next step: ${SCRIPT_DIR}/03-reproduce-server-side-diff-bug.sh"
