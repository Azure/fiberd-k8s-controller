#!/usr/bin/env bash
# Installs helm from its release archive, checked against a pinned checksum, for
# the jobs whose output ships or that hold a write token. Runs on linux/amd64.
#
#   hack/release/install-helm.sh <dir>
set -euo pipefail

readonly VERSION=v4.3.1
readonly SHA256=0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b
readonly ARCHIVE="helm-${VERSION}-linux-amd64.tar.gz"

dir=${1:?usage: install-helm.sh <dir>}

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

curl -fsSL --retry 3 --output "${work}/${ARCHIVE}" "https://get.helm.sh/${ARCHIVE}"
(cd "${work}" && shasum -a 256 --check --status <<<"${SHA256}  ${ARCHIVE}")
tar -xzf "${work}/${ARCHIVE}" -C "${work}" linux-amd64/helm
mkdir -p "${dir}"
mv "${work}/linux-amd64/helm" "${dir}/helm"
