# shellcheck shell=bash
# The deps and deploy stages. deploy proves the chart, the CA and its issuance
# policies, the admission policy and a grant Pod's whole first start.

STAGES+=(deps deploy)

readonly RELEASE=fiberd-k8s-controller
readonly RELEASE_NAMESPACE=fiberd-system

# The issuer's Service, the name its serving certificate carries.
readonly ISSUER_HOST="${RELEASE}.${RELEASE_NAMESPACE}.svc"
readonly ISSUER_PORT=8443

# hack/tests/kind/caller.yaml declares these.
readonly CALLER_CERTIFICATE=conform-caller
readonly CALLER_SECRET=conform-caller-tls

stage_deps() {
  hack/tests/kind/deps.sh
}

# apply_grant <name> <digest> <thumbprint>
apply_grant() {
  sed -e "s|__DIGEST__|$2|" -e "s|__THUMBPRINT__|$3|" "hack/tests/kind/grants/$1.yaml" |
    kubectl apply --filename -
}

# write_caller keeps the caller's certificate on the host, where the suite and
# the smoke test run. It stays out of the artifacts, and stage_down removes it.
write_caller() {
  local file secret
  mkdir -p -m 0700 "${PRIVATE_DIR}" "${CALLER_DIR}"
  secret="$(kubectl get secret "${CALLER_SECRET}" --namespace "${GRANT_NAMESPACE}" --output json)"
  for file in ca.crt tls.crt tls.key; do
    jq --exit-status --raw-output --arg file "${file}" '.data[$file]' <<<"${secret}" |
      base64 --decode >"${CALLER_DIR}/${file}"
  done
}

# grant_up reports whether the grant has its one Pod, Ready. The readiness and
# liveness probes are exec probes, so a Pod with any restart behind it fails the
# run here and never gets a retry. A later hook would only hide it.
grant_up() {
  local grant=$1 names restarts
  local -a pods
  names="$(kubectl get pods --namespace "${GRANT_NAMESPACE}" --selector "fiberd.io/grant=${grant}" \
    --output 'jsonpath={.items[*].metadata.name}' 2>/dev/null)" || return 1
  read -r -a pods <<<"${names}"
  ((${#pods[@]} == 1)) || return 1
  restarts="$(pod_field "${pods[0]}" '{.status.containerStatuses[0].restartCount}')"
  [[ "${restarts:-0}" == 0 ]] || die "pod ${pods[0]} of grant ${grant} restarted ${restarts} times on its first start"
  pod_up "${pods[0]}"
}

stage_deploy() {
  local grant thumbprint
  fiberd_env
  # shellcheck source=/dev/null
  source "${DIGESTS_FILE}"

  kubectl apply --filename hack/tests/kind/grants/namespace.yaml
  helm upgrade "${RELEASE}" helm/fiberd-k8s-controller \
    --install \
    --namespace "${RELEASE_NAMESPACE}" \
    --create-namespace \
    --values hack/tests/kind/values.yaml \
    --set "image.digest=${CONTROLLER_DIGEST}" \
    --wait \
    --timeout "${TIMEOUT}s"

  kubectl apply --filename hack/tests/kind/caller.yaml
  kubectl wait "certificates.cert-manager.io/${CALLER_CERTIFICATE}" --namespace "${GRANT_NAMESPACE}" \
    --for=condition=Ready --timeout="${TIMEOUT}s"
  write_caller
  thumbprint="$(fiberd_go run ./test/e2e/thumbprint "${CALLER_DIR}/tls.crt")"

  # A deploy on a kept cluster starts the Pods over, so the restart check below
  # judges this start and not the conformance suite's restart. Foreground, so
  # the old Pods are gone before those checks look for one.
  kubectl delete capacitygrants.fiberd.io --all --namespace "${GRANT_NAMESPACE}" \
    --cascade=foreground --timeout="${TIMEOUT}s"
  apply_grant plain "${PLAIN_DIGEST}" "${thumbprint}"
  apply_grant conform "${CONFORM_DIGEST}" "${thumbprint}"

  for grant in plain conform; do
    poll "${TIMEOUT}" "grant ${grant} to be Ready" grant_up "${grant}"
    kubectl wait "capacitygrants.fiberd.io/${grant}" --namespace "${GRANT_NAMESPACE}" \
      --for=condition=Ready --timeout=60s
  done

  # The first Pod could have restarted while the second came up.
  for grant in plain conform; do
    grant_up "${grant}" || die "grant ${grant} is no longer Ready"
  done
}
