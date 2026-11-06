# shellcheck shell=bash
# The up, down and images stages.

STAGES+=(up images)

readonly CONTROLLER_IMAGE=localhost/fiberd-k8s-controller:kind
readonly FIBERD_BUILD_IMAGE=localhost/fiberd-build:kind
readonly FIBERD_IMAGE=localhost/fiberd:kind
readonly PLAIN_IMAGE=localhost/fiberd-plain:kind
readonly CONFORM_IMAGE=localhost/fiberd-conform:kind
readonly DIGESTS_FILE="${E2E_DIR}/digests.env"

stage_up() {
  [[ "$(kind version)" == "kind ${KIND_VERSION} "* ]] ||
    die "this harness pins kind ${KIND_VERSION}, and PATH has $(kind version)"
  if cluster_exists; then
    kind export kubeconfig --name "${CLUSTER}" --kubeconfig "${KUBECONFIG}"
    if kubectl wait node --all --for=condition=Ready --timeout=30s >/dev/null 2>&1; then
      log "keeping cluster ${CLUSTER}, whose node is Ready"
      return
    fi
    log "replacing cluster ${CLUSTER}, whose node is not Ready"
    stage_down
  fi
  kind create cluster --name "${CLUSTER}" --config hack/tests/kind/kind.yaml \
    --image "${KIND_NODE_IMAGE}" --kubeconfig "${KUBECONFIG}" --wait 120s
}

stage_down() {
  kind delete cluster --name "${CLUSTER}" --kubeconfig "${KUBECONFIG}"
  rm -rf "${PRIVATE_DIR}"
}

fiberd_env() {
  [[ -f "${FIBERD_ENV_FILE}" ]] || die "${FIBERD_ENV_FILE} is missing, run the images stage first"
  # shellcheck source=/dev/null
  source "${FIBERD_ENV_FILE}"
}

# docker_build leaves out the attestation manifest. kind cannot load an image
# that carries one cleanly, and the two grant images start from the other two.
docker_build() {
  docker build --provenance=false "$@"
}

# build_fiberd_images builds from the fiberd under test. FIBERD_IMAGE_CACHE names
# a docker save tar. A tar that exists skips the build, and CI restores it from a
# cache keyed on the fiberd commit.
build_fiberd_images() {
  local cache=${FIBERD_IMAGE_CACHE:-} dockerfile="${FIBERD_SRC_DIR}/docker/fiberd/Dockerfile" target
  if [[ -n "${cache}" && -f "${cache}" ]]; then
    log "loading the fiberd images from ${cache}"
    docker load --input "${cache}"
    return
  fi
  docker_build --target build --tag "${FIBERD_BUILD_IMAGE}" --file "${dockerfile}" "${FIBERD_SRC_DIR}"
  docker_build --target fiberd --tag "${FIBERD_IMAGE}" --file "${dockerfile}" "${FIBERD_SRC_DIR}"
  for target in plain conform; do
    docker_build --target "${target}" --tag "localhost/fiberd-${target}:kind" \
      --file hack/tests/kind/grant.Dockerfile hack/tests/kind
  done
  if [[ -n "${cache}" ]]; then
    mkdir -p "$(dirname "${cache}")"
    docker save --output "${cache}" "${PLAIN_IMAGE}" "${CONFORM_IMAGE}"
  fi
}

# build_controller_image gives the build a context of its own when the fiberd
# under test is not the pin, since the Dockerfile copies go.mod by name.
build_controller_image() {
  local context=.
  if [[ -n "${FIBERD_MODFILE}" ]]; then
    context="${E2E_DIR}/controller-src"
    rm -rf "${context}"
    mkdir -p "${context}"
    cp -R api cmd pkg .dockerignore "${context}"
    cp "${FIBERD_MODFILE}" "${context}/go.mod"
    cp "${FIBERD_MODFILE%.mod}.sum" "${context}/go.sum"
  fi
  make image CONTROLLER_IMAGE="${CONTROLLER_IMAGE}" CONTROLLER_CONTEXT="${context}"
}

# digest_in_node names a loaded image by digest inside the node and prints the
# digest. The chart and the admission policy both want a digest, and kind leaves
# the image with no reference of that form. This is one function so a local
# registry can replace it.
digest_in_node() {
  local image=$1 node="${CLUSTER}-control-plane" digest
  digest="$(docker exec "${node}" ctr --namespace=k8s.io images ls "name==${image}" | awk 'NR == 2 { print $3 }')"
  [[ "${digest}" == sha256:* ]] || die "${node} shows no digest for ${image}"
  docker exec "${node}" ctr --namespace=k8s.io images tag --force "${image}" "${image%:*}@${digest}" >/dev/null
  printf '%s\n' "${digest}"
}

stage_images() {
  local image controller plain conform
  [[ "${FIBERD_RESOLVED:-}" == 1 ]] || hack/tests/kind/fiberd-src.sh
  fiberd_env
  build_fiberd_images
  build_controller_image
  for image in "${CONTROLLER_IMAGE}" "${PLAIN_IMAGE}" "${CONFORM_IMAGE}"; do
    kind load docker-image --name "${CLUSTER}" "${image}"
  done
  controller="$(digest_in_node "${CONTROLLER_IMAGE}")"
  plain="$(digest_in_node "${PLAIN_IMAGE}")"
  conform="$(digest_in_node "${CONFORM_IMAGE}")"
  {
    printf 'CONTROLLER_DIGEST=%s\n' "${controller}"
    printf 'PLAIN_DIGEST=%s\n' "${plain}"
    printf 'CONFORM_DIGEST=%s\n' "${conform}"
  } >"${DIGESTS_FILE}"
}
