#!/usr/bin/env bash
# Checks the layout build-image.sh wrote. Its one entry must be an index of this
# digest, and each architecture must be an image that runs as non-root, with
# its own attestation holding provenance and an SBOM. Paths are from the root of
# the repository.
#
#   hack/release/check-image.sh <layout> <digest>
set -euo pipefail

cd "$(dirname "$0")/../.."

# shellcheck source-path=SCRIPTDIR source=lib.sh
source hack/release/lib.sh

readonly USAGE="usage: check-image.sh <layout> <digest>"
layout=${1:?${USAGE}}
digest=${2:?${USAGE}}
check_digest "${digest}"

readonly INDEX_TYPE=application/vnd.oci.image.index.v1+json
# One image and one attestation for each architecture.
readonly MANIFESTS=4

blob() {
  printf '%s/blobs/sha256/%s' "${layout}" "${1#sha256:}"
}

# only_digest prints the digest of the one entry a filter leaves, and fails on
# any other count.
only_digest() {
  jq -er --arg want "$1" "[.manifests[] | $2] | if length == 1 then .[0].digest else empty end" "$3"
}

check_platform() {
  local arch=$1 index=$2 image attestation config
  image="$(only_digest "${arch}" 'select(.platform.os == "linux" and .platform.architecture == $want)' "${index}")" ||
    die "the index has no one image for ${arch}"
  attestation="$(only_digest "${image}" \
    'select(.annotations["vnd.docker.reference.type"] == "attestation-manifest" and .annotations["vnd.docker.reference.digest"] == $want)' \
    "${index}")" || die "the ${arch} image has no one attestation"
  config="$(jq -er .config.digest "$(blob "${image}")")"
  jq -e --arg arch "${arch}" '.architecture == $arch and .config.User == "65532:65532"' \
    "$(blob "${config}")" >/dev/null || die "the ${arch} image is not ${arch} running as 65532:65532"
  jq -e '[.layers[].annotations["in-toto.io/predicate-type"] // ""] |
    any(startswith("https://slsa.dev/provenance/")) and any(startswith("https://spdx.dev/Document"))' \
    "$(blob "${attestation}")" >/dev/null || die "the ${arch} attestation lacks provenance or an SBOM"
}

jq -e --arg digest "${digest}" --arg type "${INDEX_TYPE}" \
  '.manifests | length == 1 and .[0].digest == $digest and .[0].mediaType == $type' \
  "${layout}/index.json" >/dev/null || die "the layout's one entry is not the index ${digest}"

index="$(blob "${digest}")"
jq -e --argjson count "${MANIFESTS}" '.manifests | length == $count' "${index}" >/dev/null ||
  die "the index does not hold ${MANIFESTS} manifests"
for arch in amd64 arm64; do
  check_platform "${arch}" "${index}"
done
