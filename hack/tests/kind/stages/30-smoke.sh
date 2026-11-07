# shellcheck shell=bash
# The smoke stage. It fetches the plain Pod's grant from the caller route, as a
# tenant's caller does, and clones with it from the host, the only coverage of
# the controller's minting and of the path a real tenant takes. The route runs
# on the controller's Service, which the host reaches through a port-forward.

STAGES+=(smoke)

readonly SMOKE_TARGET=127.0.0.1:30085
readonly SMOKE_ISSUER_PORT=38443
readonly FORWARD_LOG="${ARTIFACTS_DIR}/port-forward.log"

forward_ready() {
  (: <"/dev/tcp/127.0.0.1/${SMOKE_ISSUER_PORT}") 2>/dev/null
}

# The subshell keeps a failure here from leaving the forward running.
stage_smoke() {
  local grant=plain pod forward status=0
  fiberd_env
  pod="$(grant_pod "${grant}")"
  poll 60 "pod ${pod} to be Ready" pod_up "${pod}"

  kubectl port-forward --namespace "${RELEASE_NAMESPACE}" "service/${RELEASE}" \
    "${SMOKE_ISSUER_PORT}:${ISSUER_PORT}" >"${FORWARD_LOG}" 2>&1 &
  forward=$!
  (
    poll 30 "the port-forward to the issuer" forward_ready
    FIBERD_E2E_TARGET="${SMOKE_TARGET}" \
      FIBERD_E2E_ISSUER="127.0.0.1:${SMOKE_ISSUER_PORT}" \
      FIBERD_E2E_ISSUER_NAME="${ISSUER_HOST}" \
      FIBERD_E2E_CALLER_DIR="${PWD}/${CALLER_DIR}" \
      FIBERD_E2E_NAMESPACE="${GRANT_NAMESPACE}" \
      FIBERD_E2E_GRANT="${grant}" \
      FIBERD_E2E_POD="${pod}" \
      FIBERD_E2E_POD_UID="$(pod_field "${pod}" '{.metadata.uid}')" \
      fiberd_go test -tags e2e -count=1 -v ./test/e2e/...
  ) || status=$?
  kill "${forward}" 2>/dev/null || true
  wait "${forward}" 2>/dev/null || true
  return "${status}"
}
