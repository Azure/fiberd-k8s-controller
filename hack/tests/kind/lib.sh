# shellcheck shell=bash
# What conform.sh, fiberd-src.sh and hooks.sh share. Each runs from the
# repository root before it sources this file.

# shellcheck source-path=SCRIPTDIR source=versions.env
source hack/tests/kind/versions.env

readonly CLUSTER=fiberd-e2e
readonly E2E_DIR=bin/e2e
readonly FIBERD_ENV_FILE="${E2E_DIR}/fiberd.env"

# What must never reach the artifacts, the signing key and the Pod's grant.
readonly PRIVATE_DIR="${E2E_DIR}/private"
readonly ARTIFACTS_DIR="${E2E_DIR}/artifacts"

readonly GRANT_NAMESPACE=conform
readonly STATE_DIR=/var/lib/fiberd
readonly AUDIT_SPOOL="${STATE_DIR}/private/audit.jsonl"

# Every wait on the cluster is bounded by this many seconds. Pulling images on
# a cold kind node is the slow part.
readonly TIMEOUT=300

# A private kubeconfig, so no command here can touch another cluster or move
# the current context.
export KUBECONFIG="${PWD}/${E2E_DIR}/kubeconfig"

log() {
  echo "e2e: $*" >&2
}

die() {
  log "$*"
  exit 1
}

# poll <seconds> <what> <command...> runs the command until it succeeds. A
# timeout ends the run, and so does a die inside the command.
poll() {
  local seconds=$1 what=$2 deadline
  shift 2
  deadline=$((SECONDS + seconds))
  until "$@"; do
    ((SECONDS < deadline)) || die "timed out after ${seconds}s waiting for ${what}"
    sleep 1
  done
}

# grep reads to the end here. grep -q would stop at the match, and under
# pipefail the signal that kills kind would read as no cluster.
cluster_exists() {
  kind get clusters 2>/dev/null | grep -x "${CLUSTER}" >/dev/null
}

# grant_pod names the one Pod of a grant.
grant_pod() {
  kubectl get pods --namespace "${GRANT_NAMESPACE}" --selector "fiberd.io/grant=$1" \
    --output 'jsonpath={.items[0].metadata.name}'
}

pod_field() {
  kubectl get pod "$1" --namespace "${GRANT_NAMESPACE}" --output "jsonpath=$2" 2>/dev/null
}

container_started() {
  pod_field "$1" '{.status.containerStatuses[0].started}'
}

# pod_up reads one status, so its two fields agree.
pod_up() {
  [[ "$(pod_field "$1" '{.status.containerStatuses[0].started} {.status.conditions[?(@.type=="Ready")].status}')" == "true True" ]]
}

pod_exec() {
  local pod=$1
  shift
  kubectl exec "${pod}" --namespace "${GRANT_NAMESPACE}" --container fiberd -- "$@"
}

# fiberd_exec is pod_exec behind the rule that keeps a run from flaking. A
# process that enters the container's cgroup while fiberd starts can cost a
# container restart, so this waits on the API until the Pod is Ready.
fiberd_exec() {
  poll 60 "pod $1 to be Ready" pod_up "$1"
  pod_exec "$@"
}
