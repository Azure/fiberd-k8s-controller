#!/usr/bin/env bash
# Packages a release around one image digest. It reaches no registry, and the
# tree stays as committed, since no file holds a version or a digest. Paths are
# from the root of the repository. It needs helm and go.
#
#   hack/release/package.sh <out> <version> <digest>
#
# <out> gets the chart, install.yaml, and SHA256SUMS over both.
set -euo pipefail

cd "$(dirname "$0")/../.."

# shellcheck source-path=SCRIPTDIR source=lib.sh
source hack/release/lib.sh

readonly USAGE="usage: package.sh <out> <version> <digest>"
out=${1:?${USAGE}}
version=${2:?${USAGE}}
digest=${3:?${USAGE}}
check_digest "${digest}"

read -r -a kustomize <<<"${KUSTOMIZE:-go tool -modfile=hack/tools/go.mod kustomize}"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# package_chart writes the digest into a copy. A chart needs its directory to
# carry its name.
package_chart() {
  local chart="${work}/${CHART_NAME}" values
  values="${chart}/values.yaml"
  cp -R "helm/${CHART_NAME}" "${chart}"
  grep -Fxq "  repository: ${IMAGE}" "${values}" || die "the chart does not name ${IMAGE}"
  [[ "$(grep -Fxc '  digest: ""' "${values}")" == 1 ]] || die "the chart must hold one empty image digest"
  sed "s|^  digest: \"\"\$|  digest: \"${digest}\"|" "${values}" >"${work}/values.yaml"
  mv "${work}/values.yaml" "${values}"
  helm package "${chart}" --version "${version}" --app-version "${version}" --destination "${out}"
}

# render_install overlays a copy of config, since kustomize takes a relative
# path to a base and the overlay needs a place of its own.
render_install() {
  local overlay="${work}/install"
  cp -R config "${work}/config"
  mkdir "${overlay}"
  cat >"${overlay}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../config/default
images:
  - name: controller
    newName: ${IMAGE}
    digest: ${digest}
EOF
  "${kustomize[@]}" build "${overlay}" >"${out}/install.yaml"
}

mkdir -p "${out}"
package_chart
render_install
(cd "${out}" && shasum -a 256 "${CHART_NAME}-${version}.tgz" install.yaml >SHA256SUMS)
