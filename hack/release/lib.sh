# shellcheck shell=bash
# What a release publishes, and what its scripts share. The names are literal
# and lowercase, since a registry refuses a capital and the repository's name
# holds one. hack/release validate checks them on every run.

readonly IMAGE=ghcr.io/azure/fiberd-k8s-controller
readonly CHART=ghcr.io/azure/charts/fiberd-k8s-controller
readonly CHART_NAME=fiberd-k8s-controller

die() {
  echo "release: $*" >&2
  exit 1
}

check_digest() {
  [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]] || die "$1 is not an image digest"
}
