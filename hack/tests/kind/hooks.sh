#!/usr/bin/env bash
# The hooks fiberd's conformance suite calls, each reaching the conform grant's
# Pod through kubectl exec. The suite runs on the host, so these are its only
# way into the Pod.
#
#   hooks.sh restart                     restart fiberd in place, keeping its state
#   hooks.sh issuer-reachable up|down    flip whether fiberd sees its issuer
#   hooks.sh audit <event> <fence>       exit 0 if the audit spool holds the record
#   hooks.sh engine-kill <grant uid>     kill the grant's zygote as a crash would
#   hooks.sh scope-lost                  make fiberd lose its scope while it runs
set -euo pipefail

cd "$(dirname "$0")/../../.."

# shellcheck source-path=SCRIPTDIR source=lib.sh
source hack/tests/kind/lib.sh

readonly ADMIN_SOCKET="${STATE_DIR}/private/admin.sock"

# C3 gives this hook three of its 60 second case timeouts and needs some back.
readonly RESTART_TIMEOUT=150

# Run inside the Pod. A private cgroup namespace shows the container's cgroup at
# the mount root, and the host's shows it at the path /proc/1/cgroup names. PID 1
# has moved into the fiberd leaf by then, so the leaf comes off the path. The
# zygote's cgroup sits under the grants subtree fiberd carves.
readonly ENGINE_KILL='
own=/sys/fs/cgroup$(cut -d: -f3 /proc/1/cgroup | head -n 1)
own=${own%/fiberd}
[ -d "$own" ] || own=/sys/fs/cgroup
procs=$own/grants/$1/fibers/zygote/cgroup.procs
[ -r "$procs" ] || { echo "no zygote cgroup for $1" >&2; exit 1; }
for pid in $(cat "$procs"); do kill -9 "$pid"; done
'

admin_post() {
  local path=$1
  shift
  fiberd_exec "${POD}" curl --silent --show-error --fail --unix-socket "${ADMIN_SOCKET}" \
    --output /dev/null --request POST "$@" "http://x/${path}"
}

restarted_past() {
  local count started ready
  read -r count started ready <<<"$(pod_field "${POD}" \
    '{.status.containerStatuses[0].restartCount} {.status.containerStatuses[0].started} {.status.conditions[?(@.type=="Ready")].status}')"
  [[ "${count:-0}" -gt "$1" && "${started:-}" == true && "${ready:-}" == True ]]
}

# restart signals PID 1 and then waits on the API alone. tini forwards the signal
# to fiberd, and the kubelet starts the container again on the same state. The
# image has no kill binary, so the shell's builtin sends it. The exec may end
# with the container, and the wait below is what decides.
restart() {
  local before
  before="$(pod_field "${POD}" '{.status.containerStatuses[0].restartCount}')"
  fiberd_exec "${POD}" sh -c 'kill -TERM 1' || log "the exec ended with the container"
  poll "${RESTART_TIMEOUT}" "pod ${POD} to restart and turn Ready" restarted_past "${before}"
}

issuer_reachable() {
  case "$1" in
    up) admin_post issuer --data '{"reachable":true}' ;;
    down) admin_post issuer --data '{"reachable":false}' ;;
    *) die "issuer-reachable takes up or down" ;;
  esac
}

# audit greps for a record as core.AuditRecord writes it. A fence is
# grant/epoch/seq, and core.Fence carries no JSON tags. The suite reads a failed
# hook as a record not there yet.
audit() {
  local event=$1 uid epoch seq
  IFS=/ read -r uid epoch seq <<<"$2"
  fiberd_exec "${POD}" grep -q \
    "\"event\":\"${event}\".*\"fence\":{\"GrantUID\":\"${uid}\",\"Epoch\":${epoch},\"Seq\":${seq}}" \
    "${AUDIT_SPOOL}"
}

engine_kill() {
  fiberd_exec "${POD}" sh -c "${ENGINE_KILL}" hook "$1"
}

scope_lost() {
  admin_post scope-lost
}

main() {
  POD="$(grant_pod conform)"
  readonly POD
  case "${1:-}" in
    restart) restart ;;
    issuer-reachable) issuer_reachable "$2" ;;
    audit) audit "$2" "$3" ;;
    engine-kill) engine_kill "$2" ;;
    scope-lost) scope_lost ;;
    *) die "unknown hook ${1:-}" ;;
  esac
}

main "$@"
