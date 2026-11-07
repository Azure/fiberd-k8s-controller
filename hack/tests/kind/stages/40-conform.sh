# shellcheck shell=bash
# The conform stage. It runs fiberd's own suite from the host against a Pod the
# controller made. The suite mints its own grants, so it needs the controller's
# key to pass the Pod's key set check and the image's test hooks to admit its
# UIDs. It binds each grant it mints to the key of -tls-cert. It tests fiberd's
# contract, and the smoke stage tests the controller's minting.

STAGES+=(conform)

readonly CONFORM_TARGET=127.0.0.1:30084
readonly CONFORM_BINARY="${PWD}/${E2E_DIR}/grant-conform"
readonly CONFORM_LOG="${ARTIFACTS_DIR}/conform.log"

# The name the Pod's certificate carries, since the NodePort is not on it.
readonly CONFORM_SERVER_NAME="fiberd-conform.${GRANT_NAMESPACE}.svc"

# The issuer every grant Pod verifies against, config/manager's --issuer-url.
readonly ISSUER_URL="https://${ISSUER_HOST}:${ISSUER_PORT}"
readonly KEY_SECRET=grant-issuer-key

# The suite skips a case with no hook and still passes, so a green run can hide
# a lost hook. Each of these must report PASS. C11 and C14 need a second home,
# and C12 joins once -workspaces has been tried on proc.
REQUIRED_CASES=(
  C1_Idempotency
  C2_FenceMonotonic
  C3_EpochBump
  C4_RevocationByTTL
  C5_AdmissionCompleteness
  C6_WBudget
  C7_TierFloor
  C8_DeviceBudget
  C9_EngineLoss
  C10_ScopeRevocation
  C13_Discard
)

# The binary comes from fiberd's own module, so go.mod gains nothing. go -C moves
# before it reads -o, so that path is absolute.
build_suite() {
  rm -f "${CONFORM_BINARY}"
  go -C "${FIBERD_SRC_DIR}" test -c -tags integration -o "${CONFORM_BINARY}" ./tests/e2e/conform
}

# write_signing_key takes the key that signs today from the controller's ring.
# It stays out of the artifacts, and stage_down removes it.
write_signing_key() {
  local key=$1
  mkdir -p -m 0700 "${PRIVATE_DIR}"
  kubectl get secret "${KEY_SECRET}" --namespace "${RELEASE_NAMESPACE}" \
    --output 'jsonpath={.data.keys\.json}' | base64 --decode | jq --exit-status '.[0].jwk' >"${key}"
}

check_required_cases() {
  local name missing=""
  for name in "${REQUIRED_CASES[@]}"; do
    grep -Fq -- "--- PASS: TestConformance/${name} " "${CONFORM_LOG}" || missing+=" ${name}"
  done
  [[ -z "${missing}" ]] || die "these cases did not report PASS:${missing}"
}

stage_conform() {
  local pod template key="${PRIVATE_DIR}/issuer-key.json" hooks=hack/tests/kind/hooks.sh
  fiberd_env
  build_suite
  write_signing_key "${key}"
  pod="$(grant_pod conform)"
  template="$(kubectl get capacitygrants.fiberd.io/conform --namespace "${GRANT_NAMESPACE}" \
    --output 'jsonpath={.spec.zygote.digest}')"

  "${CONFORM_BINARY}" -test.v -test.timeout=20m \
    -target "${CONFORM_TARGET}" \
    -tls-ca "${CALLER_DIR}/ca.crt" \
    -tls-cert "${CALLER_DIR}/tls.crt" \
    -tls-key "${CALLER_DIR}/tls.key" \
    -tls-server-name "${CONFORM_SERVER_NAME}" \
    -target-tier FIBER_CHECKPOINT \
    -node-id "${pod}" \
    -template "${template}" \
    -isolation TRUSTED \
    -case-timeout 60s \
    -mint jwt -issuer "${ISSUER_URL}" -issuer-key "${key}" \
    -restart-cmd "${hooks} restart" \
    -cp-health-cmd "${hooks} issuer-reachable \$1" \
    -audit-cmd "${hooks} audit \$1 \$2" \
    -engine-kill-cmd "${hooks} engine-kill \$1" \
    -scope-cmd "${hooks} scope-lost" 2>&1 | tee "${CONFORM_LOG}"
  check_required_cases
}
