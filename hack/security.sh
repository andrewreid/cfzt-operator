#!/usr/bin/env bash
set -euo pipefail

# Shared policy for local checks, CI, scheduled scans, and publication gates.
report_dir="${SECURITY_REPORT_DIR:-bin/security}"
trivy="${TRIVY:-trivy}"
mkdir -p "${report_dir}"

require_trivy() {
  local expected="${TRIVY_VERSION:-0.75.0}"
  local actual
  actual="$("${trivy}" --version | sed -n 's/^Version: //p')"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "Trivy ${expected} required; found ${actual:-unavailable}" >&2
    exit 1
  fi
}

render_report() {
  local report="$1" exit_code="$2" title="$3" status=0
  "${trivy}" convert --config '' --ignorefile '' --scanners vuln --format table \
    --exit-code "${exit_code}" "${report}" | tee "${report%.json}.txt" || status=$?
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    {
      printf '### %s\n\n```text\n' "${title}"
      "${trivy}" convert --config '' --ignorefile '' --scanners vuln --report summary "${report}"
      printf '\n```\n'
    } >> "${GITHUB_STEP_SUMMARY}"
  fi
  return "${status}"
}

scan_image() {
  local image="$1" report="$2" exit_code="$3" title="$4"
  local platform="${5:-${SECURITY_IMAGE_PLATFORM:-}}"
  local platform_args=()
  if [[ -n "${platform}" ]]; then
    platform_args=(--platform "${platform}")
  fi
  require_trivy
  "${trivy}" image --config '' --ignorefile '' --scanners vuln \
    --pkg-types os,library ${platform_args[@]+"${platform_args[@]}"} \
    --format json --output "${report}" "${image}"
  render_report "${report}" "${exit_code}" "${title}"
}

case "${1:-}" in
  go)
    govulncheck="${GOVULNCHECK:-govulncheck}"
    # Module mode accepts no patterns; cmd contains Go files in this module.
    "${govulncheck}" -C cmd -scan module | tee "${report_dir}/go-modules.txt"
    "${govulncheck}" ./... | tee "${report_dir}/go-source.txt"
    ;;
  deps)
    require_trivy
    "${trivy}" fs --config '' --ignorefile '' --scanners vuln --pkg-types library \
      --skip-dirs bin --skip-dirs dist --skip-dirs .git \
      --format json --output "${report_dir}/dependencies.json" .
    render_report "${report_dir}/dependencies.json" 1 'Go dependency advisories'
    ;;
  image)
    scan_image "${2:?operator image reference required}" \
      "${report_dir}/operator-image.json" 1 'Operator image advisories'
    ;;
  connector)
    image="$(awk -F '"' '/DefaultCloudflaredImage =/ {print $2}' internal/workload/daemonset.go)"
    if [[ -z "${image}" ]]; then
      echo 'Cannot determine the default cloudflared image' >&2
      exit 1
    fi
    # Cloudflared is maintained upstream. Report every finding without blocking
    # an otherwise clean operator release on unresolved connector advisories.
    scan_image "${image}" "${report_dir}/connector-image.json" 0 \
      "Cloudflared ${image} linux/amd64 advisories (report only)" linux/amd64
    ;;
  *)
    echo "Usage: $0 {go|deps|image IMAGE|connector}" >&2
    exit 2
    ;;
esac
