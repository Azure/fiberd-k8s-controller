#!/usr/bin/env bash
# The kind end-to-end run. Each file under stages adds its stages to STAGES, in
# the order its name sorts.
#
#   hack/tests/kind/conform.sh all       every stage, on a new cluster
#   hack/tests/kind/conform.sh <stage>   one stage, on the cluster that is there
#   hack/tests/kind/conform.sh down      delete the cluster
#
# It needs docker, git, go, helm, jq, kind, kubectl and make.
set -euo pipefail

cd "$(dirname "$0")/../../.."

# shellcheck source-path=SCRIPTDIR source=lib.sh
source hack/tests/kind/lib.sh

STAGES=()
for stage_file in hack/tests/kind/stages/*.sh; do
  # shellcheck source=/dev/null
  source "${stage_file}"
done

usage() {
  trap - EXIT
  echo "usage: $0 all|down|<stage>, where the stages are ${STAGES[*]}" >&2
  exit 2
}

run_all() {
  local stage
  stage_down
  rm -rf "${ARTIFACTS_DIR}"
  mkdir -p "${ARTIFACTS_DIR}"
  for stage in "${STAGES[@]}"; do
    log "stage ${stage}"
    "stage_${stage}"
  done
}

main() {
  local stage=${1:-} tool
  for tool in docker git go helm jq kind kubectl make; do
    command -v "${tool}" >/dev/null 2>&1 || die "${tool} is not on PATH"
  done
  mkdir -p "${ARTIFACTS_DIR}"
  if [[ "${stage}" == all ]]; then
    run_all
  elif declare -F "stage_${stage}" >/dev/null; then
    "stage_${stage}"
  else
    usage
  fi
}

main "$@"
