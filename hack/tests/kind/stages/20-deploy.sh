# shellcheck shell=bash
# The deps and deploy stages. deploy proves the chart, the CA, the admission
# policy and a grant Pod's whole first start.

STAGES+=(deps deploy)

readonly RELEASE=fiberd-k8s-controller
readonly RELEASE_NAMESPACE=fiberd-system

stage_deps() {
  hack/tests/kind/deps.sh
}

apply_grant() {
  sed "s|__DIGEST__|$2|" "hack/tests/kind/grants/$1.yaml" | kubectl apply --filename -
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
  local grant
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

  # A deploy on a kept cluster starts the Pods over, so the restart check below
  # judges this start and not the conformance suite's restart. Foreground, so
  # the old Pods are gone before those checks look for one.
  kubectl delete capacitygrants.fiberd.io --all --namespace "${GRANT_NAMESPACE}" \
    --cascade=foreground --timeout="${TIMEOUT}s"
  apply_grant plain "${PLAIN_DIGEST}"
  apply_grant conform "${CONFORM_DIGEST}"

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
