#!/usr/bin/env bash
# Picks the fiberd a run tests, checks it out and records the choice. The first
# of these that is set wins.
#
#   FIBERD_REF          an input, main, a commit, a tag, or pin for the go.mod pin
#   FIBERD_VERSION      the repository variable, a fiberd release tag
#   FIBERD_DEFAULT_REF  the default in versions.env
#   go.mod              the pin
#
# FIBERD_SRC names a directory to use instead. Its images and suite come from
# that tree, and the controller keeps the pin.
#
# A choice other than the pin moves a private copy of go.mod, so the controller,
# the images and the suite build from one fiberd and the tree's go.mod stays as
# committed. The result lands in bin/e2e/fiberd.env for the later stages.
set -euo pipefail

cd "$(dirname "$0")/../../.."

# shellcheck source-path=SCRIPTDIR source=lib.sh
source hack/tests/kind/lib.sh

readonly FIBERD_MODULE=github.com/Azure/fiberd
readonly FIBERD_URL=https://github.com/Azure/fiberd.git
readonly CLONE_DIR=bin/fiberd-src
readonly MODFILE="${E2E_DIR}/fiberd.mod"

# pin_ref prints the go.mod pin as a git ref. A pseudo-version ends in the
# commit's first twelve characters, and a release version is its own tag. A dot
# comes before the timestamp when the version follows a tag, and a dash when
# none does.
pin_ref() {
  local version
  version="$(go list -m -f '{{.Version}}' "${FIBERD_MODULE}")"
  if [[ "${version}" =~ [-.][0-9]{14}-([0-9a-f]{12})$ ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
  else
    printf '%s\n' "${version}"
  fi
}

# A clone rather than the module, since module zips drop the exec bits fiberd's
# Dockerfile relies on.
sync_clone() {
  if [[ -d "${CLONE_DIR}/.git" ]]; then
    git -C "${CLONE_DIR}" fetch --quiet --tags --force origin
  else
    git clone --quiet --filter=blob:none "${FIBERD_URL}" "${CLONE_DIR}"
  fi
}

commit_of() {
  git -C "${CLONE_DIR}" rev-parse --verify --quiet "origin/$1^{commit}" ||
    git -C "${CLONE_DIR}" rev-parse --verify --quiet "$1^{commit}" ||
    die "fiberd has no ref $1"
}

# warn_mismatch stays quiet while versions.env decides, since a warning on every
# run teaches people to ignore it.
warn_mismatch() {
  local spec=$1 source=$2 commit=$3 default_commit=$4 pin_commit=$5
  if [[ "${source}" == input || "${source}" == variable ]] && [[ "${commit}" != "${default_commit}" ]]; then
    warn "fiberd ${spec} (${commit}) from the ${source} differs from the default (${default_commit})"
  fi
  if [[ "${spec}" == v[0-9]* && "${commit}" != "${pin_commit}" ]]; then
    warn "fiberd ${spec} (${commit}) differs from the go.mod pin (${pin_commit})"
  fi
}

warn() {
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    echo "::warning::$*"
  else
    log "warning: $*"
  fi
}

# write_modfile moves the private go.mod. go.sum sits beside it under the name
# -modfile derives.
write_modfile() {
  cp go.mod "${MODFILE}"
  cp go.sum "${MODFILE%.mod}.sum"
  go get -modfile="${MODFILE}" "${FIBERD_MODULE}@$1"
  go mod tidy -modfile="${MODFILE}"
}

record() {
  local src=$1 commit=$2 spec=$3 source=$4 modfile=$5
  {
    printf 'FIBERD_SRC_DIR=%q\n' "${src}"
    printf 'FIBERD_COMMIT=%q\n' "${commit}"
    printf 'FIBERD_SPEC=%q\n' "${spec}"
    printf 'FIBERD_SOURCE=%q\n' "${source}"
    printf 'FIBERD_MODFILE=%q\n' "${modfile}"
  } >"${FIBERD_ENV_FILE}"
  log "testing fiberd ${commit} from the ${source} (${spec})"
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    echo "fiberd \`${commit}\` from the ${source} (${spec})" >>"${GITHUB_STEP_SUMMARY}"
  fi
}

main() {
  local spec source pin commit default_commit pin_commit modfile=""
  mkdir -p "${E2E_DIR}"

  if [[ -n "${FIBERD_SRC:-}" ]]; then
    [[ -d "${FIBERD_SRC}" ]] || die "FIBERD_SRC ${FIBERD_SRC} is not a directory"
    commit="$(git -C "${FIBERD_SRC}" rev-parse HEAD 2>/dev/null || echo unknown)"
    record "${FIBERD_SRC}" "${commit}" "${FIBERD_SRC}" local ""
    return
  fi

  if [[ -n "${FIBERD_REF:-}" ]]; then
    spec=${FIBERD_REF} source=input
  elif [[ -n "${FIBERD_VERSION:-}" ]]; then
    spec=${FIBERD_VERSION} source=variable
  elif [[ -n "${FIBERD_DEFAULT_REF:-}" ]]; then
    spec=${FIBERD_DEFAULT_REF} source=default
  else
    spec=pin source=pin
  fi

  sync_clone
  pin="$(pin_ref)"
  pin_commit="$(commit_of "${pin}")"
  default_commit="$(commit_of "${FIBERD_DEFAULT_REF:-${pin}}")"
  if [[ "${spec}" == pin ]]; then
    commit=${pin_commit}
  else
    commit="$(commit_of "${spec}")"
  fi
  git -C "${CLONE_DIR}" checkout --quiet --detach "${commit}"

  warn_mismatch "${spec}" "${source}" "${commit}" "${default_commit}" "${pin_commit}"
  if [[ "${commit}" != "${pin_commit}" ]]; then
    write_modfile "${commit}"
    modfile=${MODFILE}
  fi
  record "${CLONE_DIR}" "${commit}" "${spec}" "${source}" "${modfile}"
}

main
