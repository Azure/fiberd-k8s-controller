# shellcheck shell=bash
# What a release publishes, and what its scripts share. The names are literal
# and lowercase, since a registry refuses a capital and the repository's name
# holds one. hack/release validate checks them on every run.

readonly IMAGE=ghcr.io/azure/fiberd-k8s-controller
readonly CHART=ghcr.io/azure/charts/fiberd-k8s-controller
readonly CHART_NAME=fiberd-k8s-controller

# Stands in the draft's notes for the chart's digest, which exists only after
# the push that comes after the draft.
readonly CHART_DIGEST_MARKER=CHART-DIGEST-AT-PUBLISH

# The SBOM generator runs inside the build, so it is pinned like a base image.
readonly SBOM_SCANNER=docker.io/docker/buildkit-syft-scanner:1.12.0@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9

die() {
  echo "release: $*" >&2
  exit 1
}

check_digest() {
  [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]] || die "$1 is not an image digest"
}

# crane runs the version pinned in hack/tools/go.mod.
crane() {
  local -a tool
  read -r -a tool <<<"${CRANE:-go tool -modfile=hack/tools/go.mod crane}"
  "${tool[@]}" "$@"
}
