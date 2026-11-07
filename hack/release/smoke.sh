#!/usr/bin/env bash
# Sends a release through a throwaway registry on this machine and proves what
# comes back, so a dry run reaches everything but the real registry. The image
# must keep its digest and start on this architecture, and the chart must keep
# its bytes. SMOKE_REGISTRY names the registry. It needs docker, go and helm.
# Paths are from the root of the repository.
#
#   hack/release/smoke.sh <layout> <assets> <version> <digest>
set -euo pipefail

cd "$(dirname "$0")/../.."

# shellcheck source-path=SCRIPTDIR source=lib.sh
source hack/release/lib.sh

readonly USAGE="usage: smoke.sh <layout> <assets> <version> <digest>"
layout=${1:?${USAGE}}
assets=${2:?${USAGE}}
version=${3:?${USAGE}}
digest=${4:?${USAGE}}
check_digest "${digest}"

readonly REGISTRY=${SMOKE_REGISTRY:-localhost:5000}

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

smoke_image() {
  local ref="${REGISTRY}/${IMAGE##*/}@${digest}" pushed
  pushed="$(crane push "${layout}" "${ref}")"
  [[ "${pushed}" == "${ref}" ]] || die "pushed ${pushed}, want ${ref}"
  docker pull "${ref}"
  [[ "$(docker image inspect --format '{{.Architecture}}' "${ref}")" == "$(docker version --format '{{.Server.Arch}}')" ]] ||
    die "the pulled image is not for this architecture"
  [[ "$(docker image inspect --format '{{.Config.User}}' "${ref}")" == "65532:65532" ]] ||
    die "the pulled image does not run as 65532:65532"
  docker run --rm "${ref}" --help >/dev/null
}

smoke_chart() {
  local name="${CHART_NAME}-${version}.tgz"
  helm push "${assets}/${name}" "oci://${REGISTRY}/charts" --plain-http
  helm pull "oci://${REGISTRY}/charts/${CHART_NAME}" --version "${version}" --plain-http --destination "${work}"
  cmp "${assets}/${name}" "${work}/${name}" || die "the chart came back changed"
  crane digest "${REGISTRY}/charts/${CHART_NAME}:${version}"
}

smoke_image
smoke_chart
