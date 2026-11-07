#!/usr/bin/env bash
# Builds the controller image for both architectures into an OCI layout, with
# provenance and an SBOM, and prints digest=<digest>. The digest is fixed here,
# so a release pushes these bytes and never builds again. The Dockerfile
# cross-compiles, so one host makes both. It needs a docker-container builder.
# Paths are from the root of the repository.
#
#   hack/release/build-image.sh <layout> <version>
set -euo pipefail

cd "$(dirname "$0")/../.."

# shellcheck source-path=SCRIPTDIR source=lib.sh
source hack/release/lib.sh

readonly USAGE="usage: build-image.sh <layout> <version>"
layout=${1:?${USAGE}}
version=${2:?${USAGE}}
[[ ! -e "${layout}" ]] || die "${layout} already exists"

docker buildx build \
  --no-cache \
  --platform linux/amd64,linux/arm64 \
  --provenance=mode=max \
  --attest "type=sbom,generator=${SBOM_SCANNER}" \
  --build-arg SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)" \
  --label "org.opencontainers.image.version=${version}" \
  --label "org.opencontainers.image.revision=$(git rev-parse HEAD)" \
  --output "type=oci,dest=${layout},tar=false,rewrite-timestamp=true" \
  --file dockerfiles/controller/Dockerfile \
  . >&2

printf 'digest=%s\n' "$(jq -er '.manifests[0].digest' "${layout}/index.json")"
