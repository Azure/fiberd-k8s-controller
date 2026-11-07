#!/usr/bin/env bash
# Publishes the draft hack/release finish named. One PATCH sets the notes and
# flips the draft, which creates the tag at the commit the run tested, so it
# either happens or leaves the draft. It needs gh, GH_TOKEN and
# GITHUB_REPOSITORY.
#
#   hack/release/publish.sh <draft-id> <chart-digest> <prerelease>
set -euo pipefail

cd "$(dirname "$0")/../.."

# shellcheck source-path=SCRIPTDIR source=lib.sh
source hack/release/lib.sh

readonly USAGE="usage: publish.sh <draft-id> <chart-digest> <prerelease>"
draft_id=${1:?${USAGE}}
chart_digest=${2:?${USAGE}}
prerelease=${3:?${USAGE}}
[[ "${draft_id}" =~ ^[0-9]+$ ]] || die "${draft_id} is not a release ID"
check_digest "${chart_digest}"

readonly RELEASE="repos/${GITHUB_REPOSITORY}/releases/${draft_id}"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
notes="${work}/notes.md"

gh api "${RELEASE}" --jq .body >"${notes}.draft"
grep -Fq "${CHART_DIGEST_MARKER}" "${notes}.draft" || die "the draft's notes have no place for the chart digest"
sed "s|${CHART_DIGEST_MARKER}|${chart_digest}|" "${notes}.draft" >"${notes}"

# A pre-release never becomes the latest release. legacy lets GitHub compare
# versions, so an older version published later does not take the place.
latest=legacy
if [[ "${prerelease}" == true ]]; then
  latest=false
fi
gh api --method PATCH "${RELEASE}" --silent -F draft=false -f make_latest="${latest}" -F body=@"${notes}"
