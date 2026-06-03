#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  hack/live-cloudflare-local.sh preflight
  hack/live-cloudflare-local.sh up
  hack/live-cloudflare-local.sh lifecycle
  hack/live-cloudflare-local.sh failover
  hack/live-cloudflare-local.sh down
  hack/live-cloudflare-local.sh delete-cluster
  hack/live-cloudflare-local.sh stop-colima

The script sources .env.live from the repo root. Copy .env.live.example to
.env.live and fill in real Cloudflare values before running it.

Docker runtime:
  LOCAL_DOCKER_RUNTIME=auto|colima|docker  default: auto
  COLIMA_PROFILE=default                   default: default
  COLIMA_START_ARGS="--cpu 4 --memory 8"   optional extra start args

Route smoke overrides:
  CF_SMOKE_ROUTE_CIDR=100.64.207.0/24
  CF_SMOKE_ROUTE_CONFLICT_CIDR=100.64.208.0/24
EOF
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
env_file="${repo_root}/.env.live"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

load_env() {
  if [[ ! -f "${env_file}" ]]; then
    echo "missing ${env_file}; copy .env.live.example to .env.live first" >&2
    exit 1
  fi
  set -a
  # shellcheck source=/dev/null
  source "${env_file}"
  set +a
}

load_env_if_present() {
  if [[ -f "${env_file}" ]]; then
    set -a
    # shellcheck source=/dev/null
    source "${env_file}"
    set +a
  fi
}

docker_runtime() {
  echo "${LOCAL_DOCKER_RUNTIME:-auto}"
}

colima_context() {
  local profile="${COLIMA_PROFILE:-default}"
  if [[ "${profile}" == "default" ]]; then
    echo "colima"
  else
    echo "colima-${profile}"
  fi
}

colima_running() {
  colima status --profile "${COLIMA_PROFILE:-default}" >/dev/null 2>&1
}

ensure_docker_runtime() {
  require_cmd docker

  case "$(docker_runtime)" in
    colima)
      ensure_colima
      ;;
    docker)
      if ! docker info >/dev/null 2>&1; then
        echo "docker runtime is not reachable; start Docker Desktop or set LOCAL_DOCKER_RUNTIME=colima" >&2
        exit 1
      fi
      ;;
    auto)
      if command -v colima >/dev/null 2>&1; then
        ensure_colima
      elif ! docker info >/dev/null 2>&1; then
        echo "docker runtime is not reachable and colima is not installed" >&2
        exit 1
      fi
      ;;
    *)
      echo "unsupported LOCAL_DOCKER_RUNTIME=$(docker_runtime); use auto, colima, or docker" >&2
      exit 1
      ;;
  esac
}

ensure_colima() {
  require_cmd colima
  local profile="${COLIMA_PROFILE:-default}"
  if ! colima_running; then
    # Split intentionally allows standard COLIMA_START_ARGS="--cpu 4 --memory 8".
    # shellcheck disable=SC2086
    colima start --profile "${profile}" --runtime docker ${COLIMA_START_ARGS:-}
  fi
  local context
  context="$(colima_context)"
  if docker context inspect "${context}" >/dev/null 2>&1; then
    docker context use "${context}" >/dev/null
  fi
  if ! docker info >/dev/null 2>&1; then
    echo "colima started, but docker is not reachable through the selected context" >&2
    exit 1
  fi
}

ensure_kind_cluster() {
  require_cmd kind
  require_cmd kubectl
  ensure_docker_runtime
  local cluster_name="${KIND_CLUSTER:-cfzt-live-local}"
  if ! kind get clusters | grep -qx "${cluster_name}"; then
    kind create cluster --name "${cluster_name}"
  fi
  kubectl config use-context "kind-${cluster_name}" >/dev/null
}

build_and_load_image() {
  ensure_docker_runtime
  local cluster_name="${KIND_CLUSTER:-cfzt-live-local}"
  local image_repository="${IMAGE_REPOSITORY:-cfzt-operator}"
  local image_tag="${IMAGE_TAG:-live-local}"
  local docker_platform
  docker_platform="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')"
  local target_os="${docker_platform%%/*}"
  local target_arch="${docker_platform##*/}"

  local -a buildx_cmd=()
  if docker buildx version >/dev/null 2>&1; then
    buildx_cmd=(docker buildx)
  elif command -v docker-buildx >/dev/null 2>&1; then
    buildx_cmd=(docker-buildx)
  fi

  if [[ "${#buildx_cmd[@]}" -gt 0 ]]; then
    "${buildx_cmd[@]}" build \
      --load \
      --platform "${docker_platform}" \
      --build-arg "BUILDPLATFORM=${docker_platform}" \
      --build-arg "TARGETOS=${target_os}" \
      --build-arg "TARGETARCH=${target_arch}" \
      -t "${image_repository}:${image_tag}" \
      "${repo_root}"
    kind load docker-image "${image_repository}:${image_tag}" --name "${cluster_name}"
    return
  fi

  local dockerfile
  dockerfile="$(mktemp "${TMPDIR:-/tmp}/cfzt-local-Dockerfile.XXXXXX")"
  sed "s|^FROM --platform=\${BUILDPLATFORM} golang:1.25 AS builder$|FROM golang:1.25 AS builder|" \
    "${repo_root}/Dockerfile" > "${dockerfile}"
  local build_status=0
  docker build \
    --build-arg "BUILDPLATFORM=${docker_platform}" \
    --build-arg "TARGETOS=${target_os}" \
    --build-arg "TARGETARCH=${target_arch}" \
    -f "${dockerfile}" \
    -t "${image_repository}:${image_tag}" \
    "${repo_root}" || build_status=$?
  rm -f "${dockerfile}"
  if [[ "${build_status}" -ne 0 ]]; then
    return "${build_status}"
  fi
  kind load docker-image "${image_repository}:${image_tag}" --name "${cluster_name}"
}

run_go_test() {
  require_cmd go
  (
    cd "${repo_root}"
    go test -tags=live ./test/live -run "$1" -count=1 -timeout=30m -v
  )
}

cmd="${1:-}"
case "${cmd}" in
  preflight)
    load_env
    run_go_test '^TestCloudflarePreflight$'
    ;;
  up)
    load_env_if_present
    ensure_kind_cluster
    ;;
  lifecycle)
    load_env
    require_cmd helm
    export CHART_REF="${CHART_REF:-charts/cfzt-operator}"
    export IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-cfzt-operator}"
    export IMAGE_TAG="${IMAGE_TAG:-live-local}"
    ensure_kind_cluster
    build_and_load_image
    export GITHUB_REF_NAME="${GITHUB_REF_NAME:-v0.1.0-alpha-local}"
    export GITHUB_RUN_ID="${GITHUB_RUN_ID:-$(date +%s)}"
    export GITHUB_RUN_ATTEMPT="${GITHUB_RUN_ATTEMPT:-1}"
    run_go_test '^TestCloudflareLifecycle$'
    ;;
  failover)
    load_env
    require_cmd helm
    export CHART_REF="${CHART_REF:-charts/cfzt-operator}"
    export IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-cfzt-operator}"
    export IMAGE_TAG="${IMAGE_TAG:-live-local}"
    ensure_kind_cluster
    build_and_load_image
    export GITHUB_REF_NAME="${GITHUB_REF_NAME:-v0.1.0-alpha-local}"
    export GITHUB_RUN_ID="${GITHUB_RUN_ID:-$(date +%s)}"
    export GITHUB_RUN_ATTEMPT="${GITHUB_RUN_ATTEMPT:-1}"
    run_go_test '^TestFailoverLifecycle$'
    ;;
  down)
    load_env_if_present
    require_cmd kind
    cluster_name="${KIND_CLUSTER:-cfzt-live-local}"
    kind delete cluster --name "${cluster_name}"
    if [[ "$(docker_runtime)" == "colima" || "$(docker_runtime)" == "auto" ]]; then
      if command -v colima >/dev/null 2>&1 && colima_running; then
        colima stop --profile "${COLIMA_PROFILE:-default}"
      fi
    fi
    ;;
  delete-cluster)
    load_env_if_present
    require_cmd kind
    cluster_name="${KIND_CLUSTER:-cfzt-live-local}"
    kind delete cluster --name "${cluster_name}"
    ;;
  stop-colima)
    load_env_if_present
    require_cmd colima
    colima stop --profile "${COLIMA_PROFILE:-default}"
    ;;
  -h|--help|help|"")
    usage
    ;;
  *)
    usage >&2
    exit 1
    ;;
esac
