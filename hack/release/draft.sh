#!/usr/bin/env bash
# Creates a release's draft with its assets and notes, or refreshes the draft
# hack/release guard found. A draft makes no tag and nothing here is public. The
# notes hold the image digest, which is how a later job knows the draft is its
# own. It needs gh, GH_TOKEN, GITHUB_REPOSITORY and GITHUB_SHA.
#
#   hack/release/draft.sh <assets> <tag> <version> <digest> <prerelease> [<draft-id>]
set -euo pipefail

cd "$(dirname "$0")/../.."

# shellcheck source-path=SCRIPTDIR source=lib.sh
source hack/release/lib.sh

readonly USAGE="usage: draft.sh <assets> <tag> <version> <digest> <prerelease> [<draft-id>]"
assets=${1:?${USAGE}}
tag=${2:?${USAGE}}
version=${3:?${USAGE}}
digest=${4:?${USAGE}}
prerelease=${5:?${USAGE}}
draft_id=${6:-}
check_digest "${digest}"

readonly REPO_API="repos/${GITHUB_REPOSITORY}/releases"
readonly INSTALL_URL="https://github.com/${GITHUB_REPOSITORY}/releases/download/${tag}/install.yaml"
export GH_REPO="${GITHUB_REPOSITORY}"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
notes="${work}/notes.md"

write_notes() {
  cat <<EOF
## Install

cert-manager and its csi-driver must be installed first.

\`\`\`bash
kubectl apply -f ${INSTALL_URL}
\`\`\`

\`\`\`bash
helm install ${CHART_NAME} oci://${CHART} --version ${version} --namespace fiberd-system --create-namespace
\`\`\`

## Artifacts

- Image: \`${IMAGE}@${digest}\`
- Chart: \`${CHART}@${CHART_DIGEST_MARKER}\`

## Verify

\`\`\`bash
sha256sum -c SHA256SUMS
gh attestation verify install.yaml --repo ${GITHUB_REPOSITORY}
gh attestation verify oci://${IMAGE}@${digest} --repo ${GITHUB_REPOSITORY}
\`\`\`

EOF
  gh api --method POST "${REPO_API}/generate-notes" \
    -f tag_name="${tag}" -f target_commitish="${GITHUB_SHA}" --jq .body
}

write_release() {
  local method=$1 path=$2
  gh api --method "${method}" "${path}" --silent \
    -f tag_name="${tag}" -f target_commitish="${GITHUB_SHA}" -f name="${tag}" \
    -F draft=true -F prerelease="${prerelease}" -F body=@"${notes}"
}

write_notes >"${notes}"
if [[ -n "${draft_id}" ]]; then
  write_release PATCH "${REPO_API}/${draft_id}"
else
  write_release POST "${REPO_API}"
fi
gh release upload "${tag}" --clobber \
  "${assets}/${CHART_NAME}-${version}.tgz" "${assets}/install.yaml" "${assets}/SHA256SUMS"
