#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP_DIR="${SCRIPT_DIR}/app"

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "[ERROR] Required command not found: $1" >&2
    exit 1
  }
}

for cmd in kustomize helm argocd find; do
  require_cmd "$cmd"
done

tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

cp "${APP_DIR}/kustomization.yaml" "${tmp_dir}/kustomization.yaml"

pushd "${tmp_dir}" >/dev/null
echo "[INFO] Running kustomize build --enable-helm to download charts/"
kustomize build --enable-helm . >/dev/null

echo
echo "[INFO] Downloaded files under charts/:"
find charts -type f | sed 's#^./##' | sort

echo
echo "[INFO] Simulating argocd default --local-include patterns: *.yaml,*.yml,*.json"
mapfile -t helper_files < <(find . -type f \( -name '*.tpl' -o -name '_*.tpl' \) | sed 's#^./##' | sort)
mapfile -t default_included < <(find . -type f \( -name '*.yaml' -o -name '*.yml' -o -name '*.json' \) | sed 's#^./##' | sort)

printf 'Included (%s files):\n' "${#default_included[@]}"
printf '  %s\n' "${default_included[@]}"

missing=()
for f in "${helper_files[@]}"; do
  if ! printf '%s\n' "${default_included[@]}" | grep -Fxq "$f"; then
    missing+=("$f")
  fi
done

echo
if [[ "${#missing[@]}" -gt 0 ]]; then
  echo "[BUG CONFIRMED] Default --local-include excludes Helm template helpers:"
  printf '  %s\n' "${missing[@]}"
else
  echo "[UNEXPECTED] No helper templates were excluded by default include patterns."
fi

echo
echo "[INFO] Workaround simulation: --local-include '*.*'"
mapfile -t wide_included < <(find . -type f -name '*.*' | sed 's#^./##' | sort)
printf 'Included with *.* (%s files):\n' "${#wide_included[@]}"
printf '  %s\n' "${wide_included[@]}"

still_missing=()
for f in "${helper_files[@]}"; do
  if ! printf '%s\n' "${wide_included[@]}" | grep -Fxq "$f"; then
    still_missing+=("$f")
  fi
done

if [[ "${#still_missing[@]}" -eq 0 ]]; then
  echo "[WORKAROUND CONFIRMED] Helm helper templates are included with --local-include '*.*'."
else
  echo "[ERROR] Helpers still missing with *.* include pattern:"
  printf '  %s\n' "${still_missing[@]}"
  exit 1
fi
popd >/dev/null
