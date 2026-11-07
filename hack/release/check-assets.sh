#!/usr/bin/env bash
# Checks what package.sh wrote. Every image an install or the chart can start
# must be the release's digest. Paths are from the root of the repository. It
# needs helm.
#
#   hack/release/check-assets.sh <out> <version> <digest>
set -euo pipefail

cd "$(dirname "$0")/../.."

# shellcheck source-path=SCRIPTDIR source=lib.sh
source hack/release/lib.sh

readonly USAGE="usage: check-assets.sh <out> <version> <digest>"
out=${1:?${USAGE}}
version=${2:?${USAGE}}
digest=${3:?${USAGE}}
check_digest "${digest}"

readonly WANT_IMAGE="${IMAGE}@${digest}"
readonly TGZ="${out}/${CHART_NAME}-${version}.tgz"

# images_of prints each image the manifests on stdin name, once.
images_of() {
  sed -n -E "s/^[[:space:]]*(- )?image:[[:space:]]+['\"]?([^'\"[:space:]]+)['\"]?[[:space:]]*\$/\2/p" | sort -u
}

chart_field() {
  sed -n -E "s/^$1:[[:space:]]+['\"]?([^'\"]+)['\"]?\$/\1/p"
}

check_sums() {
  (cd "${out}" && shasum -a 256 --check --status SHA256SUMS) || die "SHA256SUMS does not match"
}

# One apply must be enough, so the install holds its Namespace and CRDs and no
# object of those CRDs.
check_install() {
  local install="${out}/install.yaml" images
  images="$(images_of <"${install}")"
  [[ "${images}" == "${WANT_IMAGE}" ]] || die "install.yaml names ${images:-no image}, want ${WANT_IMAGE}"
  grep -Fxq 'kind: Namespace' "${install}" || die "install.yaml holds no Namespace"
  grep -Fxq 'kind: CustomResourceDefinition' "${install}" || die "install.yaml holds no CRD"
  if grep -Fxq 'kind: GrantPolicy' "${install}"; then
    die "install.yaml holds a GrantPolicy, which one apply cannot map beside its CRD"
  fi
}

check_chart() {
  local metadata images
  metadata="$(helm show chart "${TGZ}")"
  [[ "$(chart_field version <<<"${metadata}")" == "${version}" ]] || die "the chart's version is not ${version}"
  [[ "$(chart_field appVersion <<<"${metadata}")" == "${version}" ]] || die "the chart's appVersion is not ${version}"
  images="$(helm template "${CHART_NAME}" "${TGZ}" --namespace fiberd-system --api-versions cert-manager.io/v1 | images_of)"
  [[ "${images}" == "${WANT_IMAGE}" ]] || die "the chart renders ${images:-no image}, want ${WANT_IMAGE}"
}

check_sums
check_install
check_chart
