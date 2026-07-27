#!/usr/bin/env bash
# UAT runbook for alitellm-operator — repeat on every release.
#
# Usage:
#   ./uat-runbook.sh                    # full run (mutating, ~3 min)
#   ./uat-runbook.sh --dry-run          # read-only checks only
#   OPERATOR_NS=ackstorm LITELLM_NS=litellm ./uat-runbook.sh
#
# Pre-reqs on host: kubectl, curl, python3, jq (optional).
# Pre-reqs in cluster: alitellm-operator running, LiteLLM proxy reachable
# at svc/litellm in $LITELLM_NS, master key in
# secret/externalsecret-litellm key=LITELLM_MASTER_KEY.
#
# Reuses uat-* labelled CRs so flux GitOps will NOT restore them
# (assumes flux kustomizations don't ship anything with
# uat.ackstorm.ai/owner=uat-agent). Cleans up at end.

set -euo pipefail

OPERATOR_NS=${OPERATOR_NS:-ackstorm}
LITELLM_NS=${LITELLM_NS:-litellm}
DRY_RUN=${DRY_RUN:-0}
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

PASS=0; FAIL=0; SKIP=0; FINDINGS=()
LOG_FILE=${LOG_FILE:-/tmp/uat-$(date -u +%Y%m%d-%H%M%S).log}
echo "UAT log → $LOG_FILE"
exec > >(tee -a "$LOG_FILE") 2>&1

ok()    { echo "✅ PASS $*"; PASS=$((PASS+1)); }
bad()   { echo "❌ FAIL $*"; FAIL=$((FAIL+1)); FINDINGS+=("FAIL: $*"); }
warn()  { echo "⚠️  WARN $*"; FINDINGS+=("WARN: $*"); }
skip()  { echo "↷ SKIP $*"; SKIP=$((SKIP+1)); }
section() { echo; echo "═══ $* ═══"; }

key()   { kubectl -n "$LITELLM_NS" get secret externalsecret-litellm -o jsonpath='{.data.LITELLM_MASTER_KEY}' | base64 -d; }

LITELLM_KEY="$(key)"
[[ -z "$LITELLM_KEY" ]] && { echo "FATAL: no master key"; exit 2; }
PF_PORT=${PF_PORT:-14000}

# port-forward helper — opens, runs cmd, closes
with_pf() {
  kubectl -n "$LITELLM_NS" port-forward svc/litellm "${PF_PORT}:4000" >/tmp/uat-pf.log 2>&1 &
  local pf=$!
  sleep 2
  "$@"
  local rc=$?
  kill "$pf" 2>/dev/null || true
  wait "$pf" 2>/dev/null || true
  return $rc
}

# ─── P0: baseline ───────────────────────────────────────────────────
section "P0 — baseline"

OP_POD=$(kubectl -n "$OPERATOR_NS" get pod -l control-plane=alitellm-operator -o name | head -1)
[[ -z "$OP_POD" ]] && { bad "operator pod missing"; exit 1; }
OP_VER=$(kubectl -n "$OPERATOR_NS" get deploy alitellm-operator -o jsonpath='{.spec.template.spec.containers[0].image}')
echo "operator image: $OP_VER"
RESTARTS=$(kubectl -n "$OPERATOR_NS" get "$OP_POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')
[[ "$RESTARTS" -eq 0 ]] && ok "operator restart count = 0" || warn "operator restart count = $RESTARTS"

ERR_CNT=$(kubectl -n "$OPERATOR_NS" logs deploy/alitellm-operator --tail=500 2>&1 | grep -cE '\sERROR\s' || true)
[[ "$ERR_CNT" -eq 0 ]] && ok "operator log ERROR lines = 0" || warn "operator log ERROR lines = $ERR_CNT"

# Connection probe state
READY=$(kubectl -n "$OPERATOR_NS" get litellmconnection default -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "missing")
[[ "$READY" == "True" ]] && ok "LiteLLMConnection Ready=True" || bad "LiteLLMConnection Ready=$READY"

# Quick CR health summary
for KIND in litellmteams litellmmodels litellmmcpservers litellmmodelaliases litellma2aagents litellmguardrails litellmmodeldiscoveries litellmmcpserverdiscoveries; do
  T=$(kubectl -n "$OPERATOR_NS" get "$KIND" --no-headers 2>/dev/null | wc -l)
  N=$(kubectl -n "$OPERATOR_NS" get "$KIND" -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c True || true)
  if [[ "$T" -gt 0 && "$N" -eq "$T" ]]; then
    ok "$KIND: $N/$T Ready"
  elif [[ "$T" -gt 0 ]]; then
    bad "$KIND: only $N/$T Ready"
  fi
done

# ─── S1: status truth audit (sample) ────────────────────────────────
section "S1 — CR status vs LiteLLM truth (sample 10)"
audit_models() {
  local lite
  lite=$(curl -sS -m 10 -H "x-litellm-api-key: $LITELLM_KEY" "http://localhost:$PF_PORT/model/info")
  local miss=0 total=0
  while IFS='=' read -r name mid; do
    [[ -z "$mid" ]] && continue
    total=$((total+1))
    if ! echo "$lite" | python3 -c "import sys,json;ids=[m.get('model_info',{}).get('id') for m in json.load(sys.stdin).get('data',[])];import sys as s;s.exit(0 if '$mid' in ids else 1)"; then
      miss=$((miss+1)); FINDINGS+=("Model $name (id=$mid) absent in LiteLLM but CR Ready=True")
    fi
  done < <(kubectl -n "$OPERATOR_NS" get litellmmodels -o jsonpath='{range .items[*]}{.metadata.name}={.status.lastRendered.modelID}{"\n"}{end}' | shuf -n 10)
  [[ "$miss" -eq 0 ]] && ok "models sample (n=$total) all present" || bad "models sample: $miss/$total MISSING in LiteLLM"
}
with_pf audit_models || true

# ─── S2: metrics endpoint ───────────────────────────────────────────
section "S2 — metrics endpoint"
M=$(kubectl run -n "$OPERATOR_NS" "uat-curl-$$" --rm -i --restart=Never \
      --image=curlimages/curl:8.10.1 --quiet --timeout=20s -- \
      -sS -m 5 http://alitellm-operator-metrics."$OPERATOR_NS".svc:8080/metrics 2>/dev/null || true)
LINES=$(echo "$M" | wc -l)
LITELLM_M=$(echo "$M" | grep -cE '^litellm_' || true)
QUEUE_HOT=$(echo "$M" | awk '/^workqueue_depth\{/ {if ($NF+0 > 0) c++} END{print c+0}')
[[ "$LINES" -gt 100 ]] && ok "metrics lines=$LINES, litellm_*=$LITELLM_M" || bad "metrics suspiciously empty ($LINES lines)"
[[ "$QUEUE_HOT" -eq 0 ]] && ok "no hot workqueues" || warn "$QUEUE_HOT workqueue(s) depth>0"

# ─── D1: discovery consistency ──────────────────────────────────────
section "D1 — discovery fan-out consistency"
for d in $(kubectl -n "$OPERATOR_NS" get litellmmodeldiscovery -o jsonpath='{.items[*].metadata.name}'); do
  G=$(kubectl -n "$OPERATOR_NS" get litellmmodeldiscovery "$d" -o jsonpath='{.status.generatedCount}')
  C=$(kubectl -n "$OPERATOR_NS" get litellmmodels -l "litellm.ackstorm.ai/generated-by=$d" --no-headers 2>/dev/null | wc -l)
  [[ "$G" == "$C" ]] && ok "modeldiscovery/$d: $G=$C children" || bad "modeldiscovery/$d: status=$G actual=$C"
done
for d in $(kubectl -n "$OPERATOR_NS" get litellmmcpserverdiscovery -o jsonpath='{.items[*].metadata.name}'); do
  G=$(kubectl -n "$OPERATOR_NS" get litellmmcpserverdiscovery "$d" -o jsonpath='{.status.generatedCount}')
  C=$(kubectl -n "$OPERATOR_NS" get litellmmcpservers -l "litellm.ackstorm.ai/generated-by=$d" --no-headers 2>/dev/null | wc -l)
  [[ "$G" == "$C" ]] && ok "mcpserverdiscovery/$d: $G=$C children" || bad "mcpserverdiscovery/$d: status=$G actual=$C"
done

# ─── Functional: chat alias ─────────────────────────────────────────
section "FN1 — chat completion via alias ackstorm.lite"
chat_probe() {
  local body
  body=$(curl -sS -m 30 -H "x-litellm-api-key: $LITELLM_KEY" -H 'content-type: application/json' \
    -X POST "http://localhost:$PF_PORT/v1/chat/completions" \
    -d '{"model":"ackstorm.lite","max_tokens":40,"messages":[{"role":"user","content":"reply UAT-PROBE-OK"}]}')
  if echo "$body" | python3 -c 'import sys,json;d=json.load(sys.stdin);assert d.get("choices") and "UAT" in d["choices"][0]["message"]["content"]' 2>/dev/null; then
    ok "chat completion OK"
  else
    bad "chat completion failed: $(echo "$body" | head -c 200)"
  fi
}
with_pf chat_probe || true

# ─── Functional: MCP tools listing ──────────────────────────────────
section "FN2 — MCP tools listing"
mcp_probe() {
  local n
  n=$(curl -sS -m 10 -H "x-litellm-api-key: $LITELLM_KEY" "http://localhost:$PF_PORT/v1/mcp/tools" \
        | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("tools",[])))')
  [[ "$n" -gt 0 ]] && ok "MCP tools count = $n" || bad "MCP tools listing returned 0"
}
with_pf mcp_probe || true

if [[ "$DRY_RUN" == "1" ]]; then
  echo; echo "DRY_RUN — skipping mutating tests"
else
  # ─── F1: Team CRUD ────────────────────────────────────────────────
  section "F1 — Team CRUD"
  cat <<EOF | kubectl apply -f -
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMTeam
metadata:
  name: uat-ephemeral
  namespace: $OPERATOR_NS
  labels: { uat.ackstorm.ai/owner: uat-agent }
spec:
  budget: { limit: 1, period: 1d }
  rateLimits: { rpm: 10, tpm: 1000 }
  deletionPolicy: Delete
EOF
  sleep 5
  TID=$(kubectl -n "$OPERATOR_NS" get litellmteam uat-ephemeral -o jsonpath='{.status.lastRendered.teamID}')
  [[ -n "$TID" ]] && ok "team created teamID=$TID" || bad "team teamID missing"

  team_check() {
    curl -sS -m 5 -H "x-litellm-api-key: $LITELLM_KEY" "http://localhost:$PF_PORT/team/info?team_id=$TID" \
      | python3 -c 'import sys,json;d=json.load(sys.stdin)["team_info"];sys.exit(0 if d["rpm_limit"]==10 and d["tpm_limit"]==1000 else 1)' && ok "LiteLLM team_info matches spec" || bad "LiteLLM team_info mismatch"
  }
  with_pf team_check || true

  kubectl -n "$OPERATOR_NS" patch litellmteam uat-ephemeral --type=merge -p '{"spec":{"rateLimits":{"rpm":42,"tpm":2000}}}' >/dev/null
  sleep 4
  team_update_check() {
    curl -sS -m 5 -H "x-litellm-api-key: $LITELLM_KEY" "http://localhost:$PF_PORT/team/info?team_id=$TID" \
      | python3 -c 'import sys,json;d=json.load(sys.stdin)["team_info"];sys.exit(0 if d["rpm_limit"]==42 else 1)' && ok "team update propagated" || bad "team update did NOT propagate"
  }
  with_pf team_update_check || true

  kubectl -n "$OPERATOR_NS" delete litellmteam uat-ephemeral --wait=true --timeout=30s >/dev/null 2>&1 || true
  sleep 3
  team_delete_check() {
    HTTP=$(curl -sS -m 5 -o /dev/null -w '%{http_code}' -H "x-litellm-api-key: $LITELLM_KEY" "http://localhost:$PF_PORT/team/info?team_id=$TID")
    [[ "$HTTP" == "404" ]] && ok "team deleted (404)" || bad "team still present after CR delete (HTTP $HTTP)"
  }
  with_pf team_delete_check || true

  # ─── F2: Model + invalid ──────────────────────────────────────────
  section "F2 — Model create + invalid"
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModel
metadata:
  name: uat-fake-mock
  namespace: $OPERATOR_NS
  labels: { uat.ackstorm.ai/owner: uat-agent }
spec:
  deletionPolicy: Delete
  params:
    model: openai/uat-fake-mock-model
    api_base: http://localhost:9
    api_key: sk-uat-fake
    rpm: 1
EOF
  sleep 4
  R=$(kubectl -n "$OPERATOR_NS" get litellmmodel uat-fake-mock -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
  [[ "$R" == "True" ]] && ok "valid model Ready=True" || bad "valid model Ready=$R"

  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMModel
metadata:
  name: uat-invalid
  namespace: $OPERATOR_NS
  labels: { uat.ackstorm.ai/owner: uat-agent }
spec:
  deletionPolicy: Delete
  params: { api_base: http://localhost:9 }
EOF
  sleep 5
  R=$(kubectl -n "$OPERATOR_NS" get litellmmodel uat-invalid -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
  REASON=$(kubectl -n "$OPERATOR_NS" get litellmmodel uat-invalid -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')
  [[ "$R" == "False" && "$REASON" == "LiteLLMRejected" ]] && ok "invalid model Ready=False reason=$REASON" || bad "invalid model status=$R reason=$REASON"
  kubectl -n "$OPERATOR_NS" delete litellmmodel uat-invalid --wait=false >/dev/null

  # ─── F3: MCP CRUD ─────────────────────────────────────────────────
  section "F3 — MCP CRUD"
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMMCPServer
metadata:
  name: uat-mcp-fake
  namespace: $OPERATOR_NS
  labels: { uat.ackstorm.ai/owner: uat-agent }
spec:
  deletionPolicy: Delete
  endpoint: http://127.0.0.1:9/mcp
  transport: http
  params: { auth_type: none, description: UAT, spec_version: "2025-03-26" }
EOF
  sleep 4
  SID=$(kubectl -n "$OPERATOR_NS" get litellmmcpserver uat-mcp-fake -o jsonpath='{.status.lastRendered.serverID}')
  [[ -n "$SID" ]] && ok "mcp server registered serverID=$SID" || bad "mcp server registration failed"
  kubectl -n "$OPERATOR_NS" delete litellmmcpserver uat-mcp-fake --wait=false >/dev/null

  # ─── R1: operator restart ─────────────────────────────────────────
  section "R1 — operator restart idempotency"
  PRE_LT=$(kubectl -n "$OPERATOR_NS" get litellmmodels -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].lastTransitionTime}')
  kubectl -n "$OPERATOR_NS" delete pod -l control-plane=alitellm-operator --wait=false >/dev/null
  kubectl -n "$OPERATOR_NS" wait --for=condition=Ready pod -l control-plane=alitellm-operator --timeout=60s >/dev/null
  sleep 15
  POST_LT=$(kubectl -n "$OPERATOR_NS" get litellmmodels -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].lastTransitionTime}')
  [[ "$PRE_LT" == "$POST_LT" ]] && ok "restart: Ready lastTransitionTime preserved" || warn "restart: Ready flapped"

  # ─── R2: external drift heal via periodic safety-relist ───────────
  #
  # The operator wakes every CR on a per-controller safety-relist
  # interval (default 10 min + 0-10% jitter, env override
  # LITELLM_OPERATOR_SAFETY_RELIST_INTERVAL, Helm key
  # safetyRelistInterval). Each tick: GET /model/info?model_name=...,
  # compare model_info.id against status.lastRendered.modelID, on
  # mismatch clear ModelID → CREATE branch fires →
  # drift_corrected_total{action="create_missing"} increments.
  #
  # Wait budget: 12 min (>10m max-jitter ceiling). Override with
  # R2_WAIT_SECS=<n> for shorter cadences in staging.
  section "R2 — external drift auto-heal via periodic safety-relist"
  R2_WAIT_SECS=${R2_WAIT_SECS:-720}
  MID=$(kubectl -n "$OPERATOR_NS" get litellmmodel uat-fake-mock -o jsonpath='{.status.lastRendered.modelID}')
  drift_test() {
    local pre_metric=$(kubectl run -n "$OPERATOR_NS" "uat-curl-pre-$$" --rm -i --restart=Never \
        --image=curlimages/curl:8.10.1 --quiet --timeout=20s -- \
        -sS -m 5 http://alitellm-operator-metrics."$OPERATOR_NS".svc:8080/metrics 2>/dev/null \
        | awk '/^drift_corrected_total\{action="create_missing",domain="model"\}/ {print $2; exit}')
    pre_metric=${pre_metric:-0}
    curl -sS -m 5 -X POST -H "x-litellm-api-key: $LITELLM_KEY" -H 'content-type: application/json' \
      -d "{\"id\":\"$MID\"}" "http://localhost:$PF_PORT/model/delete" >/dev/null
    echo "  external delete at $(date -u +%H:%M:%S); pre-metric=$pre_metric; waiting ${R2_WAIT_SECS}s for relist tick..."
    sleep "$R2_WAIT_SECS"
    local hit=$(curl -sS -m 5 -H "x-litellm-api-key: $LITELLM_KEY" "http://localhost:$PF_PORT/model/info" \
                  | python3 -c 'import sys,json;print(sum(1 for m in json.load(sys.stdin).get("data",[]) if m.get("model_name")=="uat-fake-mock"))')
    local post_metric=$(kubectl run -n "$OPERATOR_NS" "uat-curl-post-$$" --rm -i --restart=Never \
        --image=curlimages/curl:8.10.1 --quiet --timeout=20s -- \
        -sS -m 5 http://alitellm-operator-metrics."$OPERATOR_NS".svc:8080/metrics 2>/dev/null \
        | awk '/^drift_corrected_total\{action="create_missing",domain="model"\}/ {print $2; exit}')
    post_metric=${post_metric:-0}
    if [[ "$hit" -gt 0 && "$post_metric" -gt "$pre_metric" ]]; then
      ok "external drift auto-healed (metric ${pre_metric}→${post_metric})"
    else
      bad "external drift NOT auto-healed in ${R2_WAIT_SECS}s; metric ${pre_metric}→${post_metric}, /model/info hit=${hit}"
    fi
  }
  with_pf drift_test || true
  kubectl -n "$OPERATOR_NS" delete litellmmodel uat-fake-mock --wait=false >/dev/null 2>&1 || true

  # ─── FN3: Team-scoped key ─────────────────────────────────────────
  section "FN3 — team-scoped key roundtrip"
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: litellm.ackstorm.ai/v1alpha1
kind: LiteLLMTeam
metadata:
  name: uat-key-team
  namespace: $OPERATOR_NS
  labels: { uat.ackstorm.ai/owner: uat-agent }
spec:
  budget: { limit: 1, period: 1d }
  rateLimits: { rpm: 5, tpm: 1000 }
  deletionPolicy: Delete
EOF
  sleep 4
  TID=$(kubectl -n "$OPERATOR_NS" get litellmteam uat-key-team -o jsonpath='{.status.lastRendered.teamID}')
  team_key_probe() {
    local kresp tkey body
    kresp=$(curl -sS -m 10 -H "x-litellm-api-key: $LITELLM_KEY" -H 'content-type: application/json' \
      -X POST "http://localhost:$PF_PORT/key/generate" \
      -d "{\"team_id\":\"$TID\",\"key_alias\":\"uat-key\",\"duration\":\"5m\",\"models\":[\"ackstorm.lite\"]}")
    tkey=$(echo "$kresp" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("key",""))')
    [[ -z "$tkey" ]] && { bad "team key not issued"; return; }
    body=$(curl -sS -m 20 -H "x-litellm-api-key: $tkey" -H 'content-type: application/json' \
      -X POST "http://localhost:$PF_PORT/v1/chat/completions" \
      -d '{"model":"ackstorm.lite","max_tokens":10,"messages":[{"role":"user","content":"say UAT-KEY-OK"}]}')
    echo "$body" | python3 -c 'import sys,json;d=json.load(sys.stdin);assert "UAT" in d["choices"][0]["message"]["content"]' 2>/dev/null \
      && ok "team key works for chat" || bad "team key chat failed"
    curl -sS -m 5 -H "x-litellm-api-key: $LITELLM_KEY" -H 'content-type: application/json' \
      -X POST "http://localhost:$PF_PORT/key/delete" -d "{\"keys\":[\"$tkey\"]}" >/dev/null
    ok "team key revoked"
  }
  with_pf team_key_probe || true
  kubectl -n "$OPERATOR_NS" delete litellmteam uat-key-team --wait=false >/dev/null
fi

# ─── Final cleanup safety ───────────────────────────────────────────
section "Cleanup safety check"
for k in litellmteams litellmmodels litellmmcpservers; do
  N=$(kubectl -n "$OPERATOR_NS" get "$k" -l uat.ackstorm.ai/owner=uat-agent -o name 2>/dev/null | wc -l)
  if [[ "$N" -gt 0 ]]; then
    warn "$N uat-owned $k still present — force deleting"
    kubectl -n "$OPERATOR_NS" delete "$k" -l uat.ackstorm.ai/owner=uat-agent --wait=false 2>/dev/null || true
  fi
done

# ─── Summary ────────────────────────────────────────────────────────
section "SUMMARY"
echo "PASS:    $PASS"
echo "FAIL:    $FAIL"
echo "SKIP:    $SKIP"
echo
if (( ${#FINDINGS[@]} > 0 )); then
  echo "FINDINGS:"
  for f in "${FINDINGS[@]}"; do echo "  - $f"; done
fi
[[ "$FAIL" -eq 0 ]] && echo "✅ UAT PASSED" && exit 0 || { echo "❌ UAT FAILED"; exit 1; }
