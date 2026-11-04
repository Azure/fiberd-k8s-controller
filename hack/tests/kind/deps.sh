#!/usr/bin/env bash
# Installs cert-manager, its csi-driver and the fiberd CA into the cluster of
# the current kubectl context. Needs kubectl and helm. Safe to run again.
set -euo pipefail

cd "$(dirname "$0")/../../.."

log() {
  echo "deps: $*" >&2
}

die() {
  log "$*"
  exit 1
}

# shellcheck source-path=SCRIPTDIR source=versions.env
source hack/tests/kind/versions.env

# config/certs puts the CA in cert-manager's namespace.
readonly CERTS_DIR=config/certs
readonly CERT_MANAGER_NAMESPACE=cert-manager
readonly CA_NAME=fiberd-ca

# Every wait is bounded by this many seconds. Pulling images on a cold kind
# node is the slow part.
readonly TIMEOUT=300

readonly CERT_MANAGER_MANIFEST="https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"

for tool in kubectl helm; do
  command -v "${tool}" >/dev/null 2>&1 || die "${tool} is not on PATH"
done

# Without a cluster the first apply fails with kubectl's openapi download
# error, which names neither the cause nor the remedy.
context="$(kubectl config current-context 2>/dev/null)" ||
  die "kubectl has no current context, create a cluster with kind first"
kubectl version --request-timeout=10s >/dev/null 2>&1 ||
  die "the cluster of context ${context} is not reachable, create one with kind first"
log "installing into the cluster of context ${context}"

log "installing cert-manager ${CERT_MANAGER_VERSION}"
kubectl apply -f "${CERT_MANAGER_MANIFEST}"
kubectl wait deployment --all --namespace "${CERT_MANAGER_NAMESPACE}" \
  --for=condition=Available --timeout="${TIMEOUT}s"

log "installing cert-manager-csi-driver ${CERT_MANAGER_CSI_DRIVER_VERSION}"
helm upgrade cert-manager-csi-driver oci://quay.io/jetstack/charts/cert-manager-csi-driver \
  --install \
  --namespace "${CERT_MANAGER_NAMESPACE}" \
  --version "${CERT_MANAGER_CSI_DRIVER_VERSION}" \
  --wait \
  --timeout "${TIMEOUT}s"

log "applying the fiberd CA from ${CERTS_DIR}"
# cert-manager's Deployments turn Available before its webhook serves, and the
# API server refuses a Certificate until it does.
deadline=$((SECONDS + TIMEOUT))
# A hung webhook call would otherwise outlast the deadline.
until output="$(kubectl apply -k "${CERTS_DIR}" --request-timeout=30s 2>&1)"; do
  if ((SECONDS >= deadline)); then
    printf '%s\n' "${output}" >&2
    die "${CERTS_DIR} still refused after ${TIMEOUT}s"
  fi
  log "${CERTS_DIR} refused, retrying in 5s"
  sleep 5
done
printf '%s\n' "${output}"

log "waiting for the fiberd CA to be Ready"
# Qualified, so another CRD named certificate cannot match.
kubectl wait "certificates.cert-manager.io/${CA_NAME}" --namespace "${CERT_MANAGER_NAMESPACE}" \
  --for=condition=Ready --timeout="${TIMEOUT}s"
kubectl wait "clusterissuers.cert-manager.io/${CA_NAME}" \
  --for=condition=Ready --timeout="${TIMEOUT}s"

log "done"
