#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_TEMPLATE_DIR="${SCRIPT_DIR}/app"
CLUSTER_NAME="argocd-bug-27259"
APP_NAME="repro-27259"

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "[ERROR] Required command not found: $1" >&2
    exit 1
  }
}

for cmd in k3d kubectl argocd kustomize helm find grep sed sort; do
  require_cmd "$cmd"
done

if ! k3d cluster get "${CLUSTER_NAME}" >/dev/null 2>&1; then
  echo "[ERROR] k3d cluster ${CLUSTER_NAME} not found. Run 02-setup-k3d-argocd.sh first." >&2
  exit 1
fi

if ! kubectl get ns argocd >/dev/null 2>&1; then
  echo "[ERROR] argocd namespace not found. Run 02-setup-k3d-argocd.sh first." >&2
  exit 1
fi

if ! argocd account get-user-info >/dev/null 2>&1; then
  echo "[ERROR] argocd CLI is not logged in. Run 02-setup-k3d-argocd.sh first." >&2
  exit 1
fi

echo "[INFO] Ensuring demo app exists"
argocd app create "${APP_NAME}" \
  --repo https://github.com/argoproj/argocd-example-apps.git \
  --path guestbook \
  --dest-server https://kubernetes.default.svc \
  --dest-namespace default \
  --upsert >/dev/null

work_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

cp "${APP_TEMPLATE_DIR}/kustomization.yaml" "${work_dir}/kustomization.yaml"
pushd "${work_dir}" >/dev/null

echo "[INFO] Pre-building local source to force kustomize chart download"
kustomize build --enable-helm . >/dev/null

helper_file="$(find charts -type f -name '_helpers.tpl' | head -n1 || true)"
if [[ -z "${helper_file}" ]]; then
  echo "[ERROR] Could not find downloaded _helpers.tpl in charts/." >&2
  exit 1
fi
echo "[INFO] Downloaded helper file: ${helper_file}"

echo
echo "=== Part A: --local + --server-side-generate with default include filters ==="
set +e
argocd app diff "${APP_NAME}" \
  --server-side-generate \
  --local "${work_dir}" \
  --refresh 2>&1 | tee "${work_dir}/part-a-default.log"
part_a_rc=${PIPESTATUS[0]}
set -e

if grep -Eiq '(error calling include|no template .* associated with template|kustomize build .* --enable-helm failed)' "${work_dir}/part-a-default.log"; then
  echo "[BUG CONFIRMED] Server-side generate failed with default local include filters."
else
  echo "[WARN] Expected template/include failure not observed (exit code=${part_a_rc})."
fi

echo
echo "=== Part A workaround: explicit --local-include '*.*' ==="
set +e
argocd app diff "${APP_NAME}" \
  --server-side-generate \
  --local "${work_dir}" \
  --local-include '*.*' \
  --refresh 2>&1 | tee "${work_dir}/part-a-workaround.log"
part_a_workaround_rc=${PIPESTATUS[0]}
set -e

if grep -Eiq '(error calling include|no template .* associated with template|kustomize build .* --enable-helm failed)' "${work_dir}/part-a-workaround.log"; then
  echo "[WARN] Workaround run still contains template/include errors (exit code=${part_a_workaround_rc})."
else
  echo "[WORKAROUND CONFIRMED] No include/template error with --local-include '*.*'."
fi

echo
echo "=== Part B: server-side stale charts persistence check ==="
repo_pod="$(kubectl -n argocd get pod -l app.kubernetes.io/name=argocd-repo-server -o jsonpath='{.items[0].metadata.name}')"

before_file="${work_dir}/repo-before.txt"
after_file="${work_dir}/repo-after.txt"

kubectl -n argocd exec "${repo_pod}" -- sh -c "find /tmp -type d -name charts 2>/dev/null | sort" >"${before_file}" || true
echo "[INFO] charts directories on repo-server before second diff: $(wc -l < "${before_file}")"

set +e
argocd app diff "${APP_NAME}" \
  --server-side-generate \
  --local "${work_dir}" \
  --local-include '*.*' \
  --refresh >/dev/null 2>&1
set -e

kubectl -n argocd exec "${repo_pod}" -- sh -c "find /tmp -type d -name charts 2>/dev/null | sort" >"${after_file}" || true
echo "[INFO] charts directories on repo-server after second diff: $(wc -l < "${after_file}")"

if ! diff -u "${before_file}" "${after_file}" >/dev/null 2>&1; then
  echo "[BUG CONFIRMED] repo-server charts/ footprint changed between runs; stale artifacts may persist."
  echo "[INFO] Inspect files: ${before_file} and ${after_file}"
else
  echo "[INFO] No repo-server charts/ footprint delta observed in this run."
  echo "       If needed, rerun this script to catch nondeterministic persistence behavior."
fi

popd >/dev/null
