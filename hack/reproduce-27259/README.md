# Reproduction scripts for issue #27259

This directory reproduces two related problems described in [argoproj/argo-cd#27259](https://github.com/argoproj/argo-cd/issues/27259):

1. **CLI `--local --server-side-generate` upload bug**
   - `argocd app diff --local` defaults to `--local-include '*.yaml,*.yml,*.json'`.
   - Helm helper files (for example `_helpers.tpl`) inside Kustomize-downloaded `charts/` are excluded from upload.
   - Repo-server receives a partial chart and `kustomize build --enable-helm` can fail with template/include errors.

2. **Server-side diff persistence bug**
   - Repo-server temporary worktrees can retain downloaded `charts/` artifacts between invocations when cleanup is skipped (`cleanState=false`).
   - Stale chart artifacts can pollute later diffs.

## Files

- `app/kustomization.yaml`: minimal Kustomize + Helm app using public `podinfo` chart
- `01-reproduce-local-cli-bug.sh`: local-only reproduction of include-filter bug (no cluster required)
- `02-setup-k3d-argocd.sh`: creates k3d cluster and installs Argo CD v2.13.0
- `03-reproduce-server-side-diff-bug.sh`: reproduces server-side behavior on the k3d cluster

## Prerequisites

Install these CLIs first:

- `k3d`
- `kubectl`
- `helm`
- `kustomize`
- `argocd`

> Script `01` only needs: `kustomize`, `helm`, `argocd`.

## Run

From repository root (`/tmp/workspace/ppapapetrou76/argo-cd`):

```bash
chmod +x hack/reproduce-27259/*.sh

./hack/reproduce-27259/01-reproduce-local-cli-bug.sh
./hack/reproduce-27259/02-setup-k3d-argocd.sh
./hack/reproduce-27259/03-reproduce-server-side-diff-bug.sh
```

## Expected output

### Script 01

- Lists downloaded chart files under `charts/`, including `_helpers.tpl`
- Shows that default include patterns (`*.yaml,*.yml,*.json`) do **not** include `_helpers.tpl`
- Prints:
  - `[BUG CONFIRMED]` for missing helper templates
  - `[WORKAROUND CONFIRMED]` for `--local-include '*.*'`

### Script 03

- **Part A** should show server-side generate behavior with default include filters and then with workaround.
- For affected versions, default filtering may produce template/include errors similar to:
  - `error calling include`
  - `no template ... associated with template "gotpl"`
- Workaround run with `--local-include '*.*'` should avoid that include-template failure mode.
- **Part B** prints repo-server `charts/` footprint before/after repeated diffs and flags potential stale-artifact persistence.

## Cleanup

```bash
# delete k3d cluster
k3d cluster delete argocd-bug-27259

# stop existing port-forward started by script 02
if [[ -f hack/reproduce-27259/argocd-pf.pid ]]; then
  kill "$(cat hack/reproduce-27259/argocd-pf.pid)" 2>/dev/null || true
  rm -f hack/reproduce-27259/argocd-pf.pid
fi
```
