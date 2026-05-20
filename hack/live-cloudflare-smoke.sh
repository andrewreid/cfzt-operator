#!/usr/bin/env bash
set -euo pipefail

OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-cfzt-system}"
SMOKE_NAMESPACE="${SMOKE_NAMESPACE:-cfzt-smoke}"
RELEASE_TAG="${GITHUB_REF_NAME:?GITHUB_REF_NAME is required}"
VERSION="${RELEASE_TAG#v}"
RUN_SUFFIX="${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}-${GITHUB_RUN_ATTEMPT:?GITHUB_RUN_ATTEMPT is required}"
TUNNEL_NAME="cfzt-smoke-${RUN_SUFFIX}"
ACCESS_POLICY="cfzt-smoke-policy-${RUN_SUFFIX}"
ACCESS_POLICY_NAME="${ACCESS_POLICY}"
PUBLIC_EXPOSURE="public-smoke"
ACCESS_EXPOSURE="access-smoke"
CONFLICT_EXPOSURE="conflict-smoke"
PUBLIC_HOSTNAME="public-${RUN_SUFFIX}.${CF_TEST_ZONE:?CF_TEST_ZONE is required}"
ACCESS_HOSTNAME="access-${RUN_SUFFIX}.${CF_TEST_ZONE}"
CONFLICT_HOSTNAME="conflict-${RUN_SUFFIX}.${CF_TEST_ZONE}"
CHART_REF="${CHART_REF:-oci://ghcr.io/andrewreid/charts/cfzt-operator}"
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-ghcr.io/andrewreid/cfzt-operator}"
IMAGE_TAG="${IMAGE_TAG:-${RELEASE_TAG}}"
CF_API_BASE="${CF_API_BASE:-https://api.cloudflare.com/client/v4}"
FOREIGN_RECORD_ID=""
TMP_DIR="$(mktemp -d)"

required_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "missing required environment variable: ${name}" >&2
    exit 1
  fi
}

required_env CF_ACCOUNT_ID
required_env CF_API_TOKEN
required_env CF_TEST_ZONE

echo "::add-mask::${CF_ACCOUNT_ID}"
echo "::add-mask::${CF_API_TOKEN}"

log() {
  printf '\n==> %s\n' "$*"
}

die() {
  echo "error: $*" >&2
  exit 1
}

cf_api() {
  local method="$1"
  local path="$2"
  local data="${3:-}"
  local response="${TMP_DIR}/cf-response.json"
  local args=(
    -sS
    -X "$method"
    "${CF_API_BASE}${path}"
    -H "Authorization: Bearer ${CF_API_TOKEN}"
    -H "Content-Type: application/json"
    -o "$response"
    -w "%{http_code}"
  )
  if [[ -n "$data" ]]; then
    args+=(--data "$data")
  fi
  local status
  status="$(curl "${args[@]}")"
  if ! jq -e '.success == true' "$response" >/dev/null; then
    echo "Cloudflare API ${method} ${path} failed with HTTP ${status}" >&2
    jq '{success, errors, messages}' "$response" >&2 || cat "$response" >&2
    return 1
  fi
  cat "$response"
}

cf_zone_id_for_hostname() {
  local hostname="$1"
  cf_api GET "/zones" | jq -r --arg host "$hostname" '
    .result
    | map(select($host == .name or ($host | endswith("." + .name))))
    | sort_by(.name | length)
    | last
    | .id // empty
  '
}

cf_dns_records_for_hostname() {
  local zone_id="$1"
  local hostname="$2"
  cf_api GET "/zones/${zone_id}/dns_records?type=CNAME&name=${hostname}"
}

cf_access_apps_for_hostname() {
  local hostname="$1"
  cf_api GET "/accounts/${CF_ACCOUNT_ID}/access/apps?domain=${hostname}&exact=true"
}

cf_access_policies_for_name() {
  local name="$1"
  cf_api GET "/accounts/${CF_ACCOUNT_ID}/access/policies" | jq --arg name "$name" '
    .result |= map(select(.name == $name))
  '
}

cf_tunnels_for_name() {
  local name="$1"
  cf_api GET "/accounts/${CF_ACCOUNT_ID}/cfd_tunnel?name=${name}"
}

wait_for_jsonpath() {
  local description="$1"
  local command="$2"
  local timeout_seconds="$3"
  local deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    if value="$(eval "$command" 2>/dev/null)" && [[ -n "$value" ]]; then
      echo "$value"
      return 0
    fi
    sleep 5
  done
  die "timed out waiting for ${description}"
}

wait_for_public_route() {
  local body="${TMP_DIR}/public-body.txt"
  local status
  local deadline=$((SECONDS + 600))
  log "waiting for public route https://${PUBLIC_HOSTNAME}/hostname"
  while (( SECONDS < deadline )); do
    status="$(curl -k -sS --max-time 15 -o "$body" -w "%{http_code}" "https://${PUBLIC_HOSTNAME}/hostname" || true)"
    if [[ "$status" == "200" && -s "$body" ]]; then
      cat "$body"
      return 0
    fi
    sleep 10
  done
  die "public hostname did not return HTTP 200 through Cloudflare Tunnel"
}

assert_access_challenged() {
  local body="${TMP_DIR}/access-body.txt"
  local headers="${TMP_DIR}/access-headers.txt"
  local status
  log "checking unauthenticated Access response for https://${ACCESS_HOSTNAME}/hostname"
  status="$(curl -k -sS --max-time 20 -D "$headers" -o "$body" -w "%{http_code}" "https://${ACCESS_HOSTNAME}/hostname" || true)"
  case "$status" in
    301|302|303|307|308|401|403)
      echo "Access hostname returned expected unauthenticated status ${status}"
      ;;
    200)
      echo "Access response headers:" >&2
      sed -n '1,40p' "$headers" >&2
      die "Access hostname returned HTTP 200; policy may be bypassing unauthenticated users"
      ;;
    *)
      echo "Access response headers:" >&2
      sed -n '1,40p' "$headers" >&2
      die "Access hostname returned unexpected status ${status}"
      ;;
  esac
}

wait_for_condition_reason() {
  local namespace="$1"
  local name="$2"
  local timeout_seconds="$3"
  local deadline=$((SECONDS + timeout_seconds))
  local reason
  while (( SECONDS < deadline )); do
    reason="$(kubectl -n "$namespace" get cloudflareexposure "$name" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || true)"
    if [[ "$reason" == "HostnameConflict" || "$reason" == "ForeignResource" ]]; then
      echo "$reason"
      return 0
    fi
    sleep 5
  done
  die "timed out waiting for ${namespace}/${name} to report a conflict reason"
}

wait_cloudflare_absent() {
  local description="$1"
  local command="$2"
  local jq_filter="$3"
  local timeout_seconds="$4"
  local deadline=$((SECONDS + timeout_seconds))
  local response
  while (( SECONDS < deadline )); do
    response="$(eval "$command")"
    if jq -e "$jq_filter" <<<"$response" >/dev/null; then
      echo "${description} absent"
      return 0
    fi
    sleep 10
  done
  die "timed out waiting for ${description} to be absent"
}

collect_diagnostics() {
  log "collecting diagnostics"
  kubectl get cloudflaretunnels -o yaml || true
  kubectl get cloudflareaccesspolicies -o yaml || true
  kubectl get cloudflareexposures -A -o yaml || true
  kubectl -n "$OPERATOR_NAMESPACE" get pods,deploy,ds,events || true
  kubectl -n "$SMOKE_NAMESPACE" get all,events || true
  kubectl -n "$OPERATOR_NAMESPACE" logs deploy/cfzt-operator --all-containers --tail=200 || true
  kubectl -n "$OPERATOR_NAMESPACE" logs "daemonset/cloudflared-${TUNNEL_NAME}" --all-containers --tail=200 || true
}

cleanup() {
  local status=$?
  set +e
  if [[ "$status" -ne 0 ]]; then
    collect_diagnostics
  fi

  log "cleanup: deleting CloudflareExposure resources"
  kubectl -n "$SMOKE_NAMESPACE" delete cloudflareexposure \
    "$PUBLIC_EXPOSURE" "$ACCESS_EXPOSURE" "$CONFLICT_EXPOSURE" \
    --ignore-not-found --wait=true --timeout=300s

  log "cleanup: deleting CloudflareAccessPolicy"
  kubectl delete cloudflareaccesspolicy "$ACCESS_POLICY" --ignore-not-found --wait=true --timeout=300s

  log "cleanup: deleting CloudflareTunnel"
  kubectl delete cloudflaretunnel "$TUNNEL_NAME" --ignore-not-found --wait=true --timeout=300s

  if [[ -n "${ZONE_ID:-}" ]]; then
    if [[ -n "$FOREIGN_RECORD_ID" ]]; then
      log "cleanup: deleting foreign conflict DNS record"
      cf_api DELETE "/zones/${ZONE_ID}/dns_records/${FOREIGN_RECORD_ID}" >/dev/null || true
    fi
    wait_cloudflare_absent "public DNS record" "cf_dns_records_for_hostname '${ZONE_ID}' '${PUBLIC_HOSTNAME}'" '.result | length == 0' 180
    wait_cloudflare_absent "access DNS record" "cf_dns_records_for_hostname '${ZONE_ID}' '${ACCESS_HOSTNAME}'" '.result | length == 0' 180
  fi
  wait_cloudflare_absent "Access application" "cf_access_apps_for_hostname '${ACCESS_HOSTNAME}'" '.result | length == 0' 180
  wait_cloudflare_absent "Access policy" "cf_access_policies_for_name '${ACCESS_POLICY_NAME}'" '.result | length == 0' 180
  wait_cloudflare_absent "Cloudflare tunnel" "cf_tunnels_for_name '${TUNNEL_NAME}'" '.result | length == 0' 180

  rm -rf "$TMP_DIR"
  exit "$status"
}
trap cleanup EXIT

log "resolving Cloudflare zone for ${CF_TEST_ZONE}"
ZONE_ID="$(cf_zone_id_for_hostname "$CF_TEST_ZONE")"
[[ -n "$ZONE_ID" ]] || die "no Cloudflare zone found for ${CF_TEST_ZONE}"

log "installing released Helm chart ${CHART_REF} ${VERSION}"
kubectl create namespace "$OPERATOR_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace "$SMOKE_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: cloudflare-credentials
  namespace: ${OPERATOR_NAMESPACE}
type: Opaque
stringData:
  accountId: "${CF_ACCOUNT_ID}"
  apiToken: "${CF_API_TOKEN}"
EOF

helm install cfzt-operator "$CHART_REF" \
  --version "$VERSION" \
  --namespace "$OPERATOR_NAMESPACE" \
  --create-namespace \
  --set image.repository="$IMAGE_REPOSITORY" \
  --set image.tag="$IMAGE_TAG" \
  --set image.pullPolicy=Never \
  --set replicaCount=1
kubectl -n "$OPERATOR_NAMESPACE" rollout status deploy/cfzt-operator --timeout=180s

log "deploying echo workload"
kubectl -n "$SMOKE_NAMESPACE" apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: smoke-echo
spec:
  replicas: 1
  selector:
    matchLabels:
      app: smoke-echo
  template:
    metadata:
      labels:
        app: smoke-echo
    spec:
      containers:
        - name: agnhost
          image: registry.k8s.io/e2e-test-images/agnhost:2.53
          args:
            - netexec
            - --http-port=8080
          ports:
            - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: smoke-echo
spec:
  selector:
    app: smoke-echo
  ports:
    - name: http
      port: 8080
      targetPort: 8080
EOF
kubectl -n "$SMOKE_NAMESPACE" rollout status deploy/smoke-echo --timeout=180s

log "creating managed Access policy"
kubectl apply -f - <<EOF
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareAccessPolicy
metadata:
  name: ${ACCESS_POLICY}
spec:
  credentialsSecretRef:
    namespace: ${OPERATOR_NAMESPACE}
    name: cloudflare-credentials
  policyName: ${ACCESS_POLICY_NAME}
  decision: allow
  rules:
    include:
      - emailDomain: ${CF_TEST_ZONE}
  sessionDuration: 24h
EOF
kubectl wait --for=condition=Ready "cloudflareaccesspolicy/${ACCESS_POLICY}" --timeout=420s
POLICY_ID_BEFORE="$(wait_for_jsonpath "Access policy ID" "kubectl get cloudflareaccesspolicy '${ACCESS_POLICY}' -o jsonpath='{.status.policyId}'" 60)"
POLICY_RULES_HASH_BEFORE="$(wait_for_jsonpath "Access policy rules hash" "kubectl get cloudflareaccesspolicy '${ACCESS_POLICY}' -o jsonpath='{.status.observedRulesHash}'" 60)"
[[ "$POLICY_RULES_HASH_BEFORE" == sha256:* ]] || die "unexpected Access policy rules hash ${POLICY_RULES_HASH_BEFORE}"
POLICIES_FOR_NAME="$(cf_access_policies_for_name "$ACCESS_POLICY_NAME")"
[[ "$(jq -r '.result | length' <<<"$POLICIES_FOR_NAME")" == "1" ]] || die "expected exactly one managed Access policy named ${ACCESS_POLICY_NAME}"
[[ "$(jq -r '.result[0].id' <<<"$POLICIES_FOR_NAME")" == "$POLICY_ID_BEFORE" ]] || die "managed Access policy ID mismatch"

log "creating tunnel"
kubectl apply -f - <<EOF
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnel
metadata:
  name: ${TUNNEL_NAME}
spec:
  tunnelName: ${TUNNEL_NAME}
  credentialsSecretRef:
    name: cloudflare-credentials
  cloudflared:
    namespace: ${OPERATOR_NAMESPACE}
EOF
kubectl wait --for=condition=Ready "cloudflaretunnel/${TUNNEL_NAME}" --timeout=420s
TUNNEL_ID_BEFORE="$(wait_for_jsonpath "tunnel ID" "kubectl get cloudflaretunnel '${TUNNEL_NAME}' -o jsonpath='{.status.tunnelId}'" 60)"
TOKEN_SECRET="$(wait_for_jsonpath "token Secret ref" "kubectl get cloudflaretunnel '${TUNNEL_NAME}' -o jsonpath='{.status.tokenSecretRef.name}'" 60)"
[[ "$TOKEN_SECRET" == "${TUNNEL_NAME}-token" ]] || die "unexpected token Secret ${TOKEN_SECRET}"
kubectl -n "$OPERATOR_NAMESPACE" rollout status "daemonset/cloudflared-${TUNNEL_NAME}" --timeout=180s

log "creating public and Access exposures"
kubectl -n "$SMOKE_NAMESPACE" apply -f - <<EOF
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: ${PUBLIC_EXPOSURE}
spec:
  tunnelRef:
    name: ${TUNNEL_NAME}
  hostname: ${PUBLIC_HOSTNAME}
  sourceRef:
    apiVersion: v1
    kind: Service
    name: smoke-echo
  access:
    enabled: false
---
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: ${ACCESS_EXPOSURE}
spec:
  tunnelRef:
    name: ${TUNNEL_NAME}
  hostname: ${ACCESS_HOSTNAME}
  sourceRef:
    apiVersion: v1
    kind: Service
    name: smoke-echo
  access:
    enabled: true
    policyRef:
      name: ${ACCESS_POLICY}
EOF
kubectl -n "$SMOKE_NAMESPACE" wait --for=condition=Ready "cloudflareexposure/${PUBLIC_EXPOSURE}" --timeout=420s
kubectl -n "$SMOKE_NAMESPACE" wait --for=condition=Ready "cloudflareexposure/${ACCESS_EXPOSURE}" --timeout=420s

PUBLIC_DNS_BEFORE="$(wait_for_jsonpath "public DNS record ID" "kubectl -n '${SMOKE_NAMESPACE}' get cloudflareexposure '${PUBLIC_EXPOSURE}' -o jsonpath='{.status.cloudflare.dnsRecordId}'" 60)"
PUBLIC_ROUTE_BEFORE="$(wait_for_jsonpath "public route hash" "kubectl -n '${SMOKE_NAMESPACE}' get cloudflareexposure '${PUBLIC_EXPOSURE}' -o jsonpath='{.status.cloudflare.publicHostnameRouteHash}'" 60)"
ACCESS_DNS_BEFORE="$(wait_for_jsonpath "access DNS record ID" "kubectl -n '${SMOKE_NAMESPACE}' get cloudflareexposure '${ACCESS_EXPOSURE}' -o jsonpath='{.status.cloudflare.dnsRecordId}'" 60)"
ACCESS_APP_BEFORE="$(wait_for_jsonpath "Access application ID" "kubectl -n '${SMOKE_NAMESPACE}' get cloudflareexposure '${ACCESS_EXPOSURE}' -o jsonpath='{.status.cloudflare.accessApplicationId}'" 60)"
ACCESS_ROUTE_BEFORE="$(wait_for_jsonpath "access route hash" "kubectl -n '${SMOKE_NAMESPACE}' get cloudflareexposure '${ACCESS_EXPOSURE}' -o jsonpath='{.status.cloudflare.publicHostnameRouteHash}'" 60)"
[[ -n "$PUBLIC_ROUTE_BEFORE" && -n "$ACCESS_ROUTE_BEFORE" ]]

wait_for_public_route
assert_access_challenged

log "checking idempotency after re-apply and operator restart"
kubectl apply -f - <<EOF
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareAccessPolicy
metadata:
  name: ${ACCESS_POLICY}
spec:
  credentialsSecretRef:
    namespace: ${OPERATOR_NAMESPACE}
    name: cloudflare-credentials
  policyName: ${ACCESS_POLICY_NAME}
  decision: allow
  rules:
    include:
      - emailDomain: ${CF_TEST_ZONE}
  sessionDuration: 24h
EOF
kubectl apply -f - <<EOF
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnel
metadata:
  name: ${TUNNEL_NAME}
spec:
  tunnelName: ${TUNNEL_NAME}
  credentialsSecretRef:
    name: cloudflare-credentials
  cloudflared:
    namespace: ${OPERATOR_NAMESPACE}
EOF
kubectl -n "$SMOKE_NAMESPACE" apply -f - <<EOF
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: ${PUBLIC_EXPOSURE}
spec:
  tunnelRef:
    name: ${TUNNEL_NAME}
  hostname: ${PUBLIC_HOSTNAME}
  sourceRef:
    apiVersion: v1
    kind: Service
    name: smoke-echo
  access:
    enabled: false
---
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: ${ACCESS_EXPOSURE}
spec:
  tunnelRef:
    name: ${TUNNEL_NAME}
  hostname: ${ACCESS_HOSTNAME}
  sourceRef:
    apiVersion: v1
    kind: Service
    name: smoke-echo
  access:
    enabled: true
    policyRef:
      name: ${ACCESS_POLICY}
EOF
kubectl -n "$OPERATOR_NAMESPACE" rollout restart deploy/cfzt-operator
kubectl -n "$OPERATOR_NAMESPACE" rollout status deploy/cfzt-operator --timeout=180s
kubectl wait --for=condition=Ready "cloudflareaccesspolicy/${ACCESS_POLICY}" --timeout=240s
kubectl wait --for=condition=Ready "cloudflaretunnel/${TUNNEL_NAME}" --timeout=240s
kubectl -n "$SMOKE_NAMESPACE" wait --for=condition=Ready "cloudflareexposure/${PUBLIC_EXPOSURE}" --timeout=240s
kubectl -n "$SMOKE_NAMESPACE" wait --for=condition=Ready "cloudflareexposure/${ACCESS_EXPOSURE}" --timeout=240s

[[ "$(kubectl get cloudflareaccesspolicy "$ACCESS_POLICY" -o jsonpath='{.status.policyId}')" == "$POLICY_ID_BEFORE" ]] || die "Access policy ID changed after idempotency check"
[[ "$(kubectl get cloudflareaccesspolicy "$ACCESS_POLICY" -o jsonpath='{.status.observedRulesHash}')" == "$POLICY_RULES_HASH_BEFORE" ]] || die "Access policy rules hash changed after idempotency check"
[[ "$(kubectl get cloudflaretunnel "$TUNNEL_NAME" -o jsonpath='{.status.tunnelId}')" == "$TUNNEL_ID_BEFORE" ]] || die "tunnel ID changed after idempotency check"
[[ "$(kubectl -n "$SMOKE_NAMESPACE" get cloudflareexposure "$PUBLIC_EXPOSURE" -o jsonpath='{.status.cloudflare.dnsRecordId}')" == "$PUBLIC_DNS_BEFORE" ]] || die "public DNS record ID changed after idempotency check"
[[ "$(kubectl -n "$SMOKE_NAMESPACE" get cloudflareexposure "$ACCESS_EXPOSURE" -o jsonpath='{.status.cloudflare.dnsRecordId}')" == "$ACCESS_DNS_BEFORE" ]] || die "access DNS record ID changed after idempotency check"
[[ "$(kubectl -n "$SMOKE_NAMESPACE" get cloudflareexposure "$ACCESS_EXPOSURE" -o jsonpath='{.status.cloudflare.accessApplicationId}')" == "$ACCESS_APP_BEFORE" ]] || die "Access application ID changed after idempotency check"

log "checking foreign DNS conflict safety"
FOREIGN_RECORD_ID="$(cf_api POST "/zones/${ZONE_ID}/dns_records" "$(jq -n \
  --arg name "$CONFLICT_HOSTNAME" \
  --arg content "example.com" \
  '{type:"CNAME", name:$name, content:$content, ttl:1, proxied:false, comment:"cfzt-live-smoke-foreign"}')" \
  | jq -r '.result.id')"
kubectl -n "$SMOKE_NAMESPACE" apply -f - <<EOF
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: ${CONFLICT_EXPOSURE}
spec:
  tunnelRef:
    name: ${TUNNEL_NAME}
  hostname: ${CONFLICT_HOSTNAME}
  sourceRef:
    apiVersion: v1
    kind: Service
    name: smoke-echo
  access:
    enabled: false
EOF
CONFLICT_REASON="$(wait_for_condition_reason "$SMOKE_NAMESPACE" "$CONFLICT_EXPOSURE" 240)"
echo "conflict exposure reported ${CONFLICT_REASON}"
FOREIGN_RECORD="$(cf_api GET "/zones/${ZONE_ID}/dns_records/${FOREIGN_RECORD_ID}")"
[[ "$(jq -r '.result.content' <<<"$FOREIGN_RECORD")" == "example.com" ]] || die "foreign DNS content was changed"
[[ "$(jq -r '.result.comment' <<<"$FOREIGN_RECORD")" == "cfzt-live-smoke-foreign" ]] || die "foreign DNS comment was changed"

log "live Cloudflare smoke passed"
