# shellcheck shell=bash
# The smoke stage. It reads the grant the controller gave the plain Pod and
# clones with it from the host, the only coverage of the path a real tenant
# takes.

STAGES+=(smoke)

readonly SMOKE_TARGET=127.0.0.1:30085
readonly GRANTS_DIR=/var/run/fiberd/grants

stage_smoke() {
  # Absolute, since go test runs the test in its package's directory.
  local pod uid grant_file="${PWD}/${PRIVATE_DIR}/plain.jwt"
  fiberd_env
  pod="$(grant_pod plain)"
  uid="$(pod_field "${pod}" '{.metadata.uid}')"
  mkdir -p -m 0700 "${PRIVATE_DIR}"
  fiberd_exec "${pod}" cat "${GRANTS_DIR}/${uid}.jwt" >"${grant_file}"

  FIBERD_E2E_TARGET="${SMOKE_TARGET}" FIBERD_E2E_GRANT_FILE="${grant_file}" \
    GOFLAGS="${FIBERD_MODFILE:+-modfile=${FIBERD_MODFILE}}" \
    go test -tags e2e -count=1 -v ./test/e2e/...
}
