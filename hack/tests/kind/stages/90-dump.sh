# shellcheck shell=bash
# The dump stage, which also runs when any stage fails, before anything is
# deleted. CI uploads what it leaves in bin/e2e/artifacts, and anyone can read
# the artifacts of a public repository, so it never reads a Secret, a grant file
# or the signing key.

DUMP_NAMESPACES=("${GRANT_NAMESPACE}" "${RELEASE_NAMESPACE}" cert-manager)
DUMP_KINDS=(capacitygrants.fiberd.io deployments pods services)
DUMP_CLUSTER_KINDS=(grantpolicies.fiberd.io validatingadmissionpolicies validatingadmissionpolicybindings)

# Run inside the Pod. Each grant's zygote log sits in its run directory.
readonly ZYGOTE_LOGS='
for log in /var/lib/fiberd/run/*/zygote.log; do
  [ -f "$log" ] || continue
  echo "--- $log"
  tail -c 65536 "$log"
done
'

# save <file> <command...> keeps what a command printed and never fails, so one
# missing object cannot cost the rest of the evidence.
save() {
  local file=$1
  shift
  "$@" >"${file}" 2>&1 || log "$1 failed, see ${file}"
}

dump_pod_logs() {
  local namespace=$1 dir=$2 pod
  for pod in $(kubectl get pods --namespace "${namespace}" --output name 2>/dev/null); do
    pod=${pod#pod/}
    save "${dir}/${pod}.log" kubectl logs "${pod}" --namespace "${namespace}" --all-containers
    kubectl logs "${pod}" --namespace "${namespace}" --all-containers --previous \
      >"${dir}/${pod}.previous.log" 2>/dev/null || rm -f "${dir}/${pod}.previous.log"
  done
}

dump_namespace() {
  local namespace=$1 dir="${ARTIFACTS_DIR}/$1" kind
  mkdir -p "${dir}"
  for kind in "${DUMP_KINDS[@]}"; do
    save "${dir}/${kind}.yaml" kubectl get "${kind}" --namespace "${namespace}" --output yaml
  done
  save "${dir}/events.txt" kubectl get events --namespace "${namespace}" --sort-by=.lastTimestamp
  if [[ "${namespace}" != cert-manager ]]; then
    save "${dir}/describe.txt" kubectl describe pods --namespace "${namespace}"
  fi
  dump_pod_logs "${namespace}" "${dir}"
}

# dump_grant_pod keeps only what the API gave for a Pod whose container has not
# started, since an exec could cost it a restart.
dump_grant_pod() {
  local pod=$1 dir="${ARTIFACTS_DIR}/${GRANT_NAMESPACE}"
  if [[ "$(container_started "${pod}")" != true ]]; then
    log "skipping the exec in pod ${pod}, whose container has not started"
    return
  fi
  save "${dir}/${pod}.zygote.log" pod_exec "${pod}" sh -c "${ZYGOTE_LOGS}"
  save "${dir}/${pod}.audit.jsonl" pod_exec "${pod}" cat "${AUDIT_SPOOL}"
}

stage_dump() {
  local namespace kind pod
  mkdir -p "${ARTIFACTS_DIR}"
  if [[ -f "${FIBERD_ENV_FILE}" ]]; then
    cp "${FIBERD_ENV_FILE}" "${ARTIFACTS_DIR}/fiberd.env"
  fi
  for kind in "${DUMP_CLUSTER_KINDS[@]}"; do
    save "${ARTIFACTS_DIR}/${kind}.yaml" kubectl get "${kind}" --output yaml
  done
  for namespace in "${DUMP_NAMESPACES[@]}"; do
    dump_namespace "${namespace}"
  done
  for pod in $(kubectl get pods --namespace "${GRANT_NAMESPACE}" --output name 2>/dev/null); do
    dump_grant_pod "${pod#pod/}"
  done
  kind export logs "${ARTIFACTS_DIR}/kind" --name "${CLUSTER}" || log "kind export logs failed"
  log "the dump is in ${ARTIFACTS_DIR}"
}

dump_on_failure() {
  local status=$?
  trap - EXIT
  if ((status != 0)) && cluster_exists; then
    log "exit status ${status}, dumping the cluster"
    stage_dump
  fi
  exit "${status}"
}

trap dump_on_failure EXIT
