#!/usr/bin/env bash
# Post-release UAT for alitellm-operator — run by hand against a REAL cluster.
#
# Usage:
#   ./uat-runbook.sh                    # full run (~2 min)
#   ./uat-runbook.sh --dry-run          # read-only checks only
#   OPERATOR_NS=ackstorm LITELLM_NS=litellm ./uat-runbook.sh
#
# Pre-reqs on host: kubectl, curl, python3.
# Pre-reqs in cluster: alitellm-operator running, LiteLLM proxy reachable at
# svc/litellm in $LITELLM_NS, master key in
# secret/externalsecret-litellm key=LITELLM_MASTER_KEY.
#
# ─── SCOPE: what belongs here and what does NOT ──────────────────────────────
#
# This is NOT a test suite. It is the thin acceptance layer for the things a
# kind cluster structurally cannot prove. Everything with a deterministic,
# mockable assertion lives in the Ginkgo e2e suite instead, where it runs on
# every PR rather than once per release by hand.
#
# Moved OUT of this runbook and into test/e2e (do not re-add them here):
#   - Team/Model/MCP CRUD .............. team_test.go, model_test.go,
#                                        mcpserver_test.go
#   - out-of-band drift auto-heal ...... model_test.go "AC-M3 conformance"
#                                        (seconds there vs a 720s wait here)
#   - /metrics exposure ................ metrics_test.go "AC-O1"
#   - status-truth audit ............... invariants_test.go "UAT-S1"
#   - discovery generatedCount ......... invariants_test.go "UAT-D1"
#   - restart idempotency .............. invariants_test.go "UAT-R1"
#   - team-scoped key CAN infer ........ team_test.go "TEAM-07"
#   - team-scoped key is DENIED ........ team_test.go "TEAM-05"
#
# What stays, and why only prod can show it:
#   P0  real deployed version + operator health under real load
#   V1  REAL provider credentials and endpoints. The e2e mock accepts any key,
#       so a model can be Ready=Synced in e2e and 401 in prod — exactly the
#       OpenRouter api_base bug (children Synced, inference
#       "Incorrect API key provided: sk-or-v1***"). Unmockable by construction.
#   V2  SCALE + elapsed time. Duplicate-row amplification only appears across
#       hundreds of models over days (claude-opus-4-7: 1718 rows in 9 days;
#       ackstorm.default: 7186 rows in 5h). A 3-model kind cluster cannot
#       produce it.
#   V3  Real LiteLLM build + real Postgres. e2e pins a chart version; prod
#       drifts, and endpoint behaviour changes with it (the toolset DELETE
#       returning 500-not-404 was a real-version behaviour).

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

LITELLM_KEY="$(kubectl -n "$LITELLM_NS" get secret externalsecret-litellm \
  -o jsonpath='{.data.LITELLM_MASTER_KEY}' | base64 -d)"
[[ -z "$LITELLM_KEY" ]] && { echo "FATAL: no master key"; exit 2; }
PF_PORT=${PF_PORT:-14000}

# port-forward helper — opens, runs cmd, closes. Bounded readiness wait; never
# a naked poll loop.
with_pf() {
  kubectl -n "$LITELLM_NS" port-forward svc/litellm "${PF_PORT}:4000" >/tmp/uat-pf.log 2>&1 &
  local pf=$! i
  for i in $(seq 1 15); do
    curl -sf -m 2 -o /dev/null "http://localhost:${PF_PORT}/health/liveliness" \
      -H "x-litellm-api-key: $LITELLM_KEY" && break
    sleep 1
  done
  if [[ "$i" -eq 15 ]]; then
    kill "$pf" 2>/dev/null || true
    bad "port-forward to svc/litellm never became ready"
    return 1
  fi
  "$@"
  local rc=$?
  kill "$pf" 2>/dev/null || true
  wait "$pf" 2>/dev/null || true
  return $rc
}

# ─── P0: deployed version + health under real load ──────────────────────────
section "P0 — deployed version & operator health"

OP_POD=$(kubectl -n "$OPERATOR_NS" get pod -l control-plane=alitellm-operator -o name | head -1)
[[ -z "$OP_POD" ]] && { bad "operator pod missing"; exit 1; }
OP_VER=$(kubectl -n "$OPERATOR_NS" get deploy alitellm-operator \
  -o jsonpath='{.spec.template.spec.containers[0].image}')
echo "operator image: $OP_VER"
kubectl -n "$OPERATOR_NS" logs deploy/alitellm-operator 2>/dev/null \
  | grep -m1 'operator identity' || true

RESTARTS=$(kubectl -n "$OPERATOR_NS" get "$OP_POD" -o jsonpath='{.status.containerStatuses[0].restartCount}')
[[ "$RESTARTS" -eq 0 ]] && ok "operator restart count = 0" || warn "operator restart count = $RESTARTS"

# Panics never appear in e2e (short run, low load); they do here.
PANICS=$(kubectl -n "$OPERATOR_NS" logs deploy/alitellm-operator --tail=2000 2>&1 \
  | grep -cE 'panic:|Observed a panic' || true)
[[ "$PANICS" -eq 0 ]] && ok "no panics in last 2000 log lines" || bad "$PANICS panic line(s) in operator log"

READY=$(kubectl -n "$OPERATOR_NS" get litellmconnection default \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "missing")
[[ "$READY" == "True" ]] && ok "LiteLLMConnection Ready=True" || bad "LiteLLMConnection Ready=$READY"

# Fleet-wide Ready tally. e2e asserts per-CR correctness on a handful of CRs;
# only prod has hundreds, where a systemic regression shows up as a ratio.
section "P0b — fleet Ready tally (scale)"
for KIND in litellmteams litellmmodels litellmmcpservers litellmmcptoolsets \
            litellmmodelaliases litellma2aagents litellmguardrails \
            litellmmodeldiscoveries litellmmcpserverdiscoveries; do
  T=$(kubectl -n "$OPERATOR_NS" get "$KIND" --no-headers 2>/dev/null | wc -l)
  [[ "$T" -eq 0 ]] && continue
  N=$(kubectl -n "$OPERATOR_NS" get "$KIND" \
      -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' \
      2>/dev/null | grep -c True || true)
  if [[ "$N" -eq "$T" ]]; then ok "$KIND: $N/$T Ready"; else bad "$KIND: only $N/$T Ready"; fi
done

# ─── V2: duplicate-row amplification (scale + elapsed time) ─────────────────
#
# POST /model/new is NOT idempotent: LiteLLM happily stores several deployment
# rows under one model_name. A resolve-by-first-row bug turns that into
# self-amplifying churn that only becomes visible at scale over days. There is
# no kind-cluster equivalent — this is the check that catches it early.
section "V2 — duplicate LiteLLM model rows (scale invariant)"
dup_check() {
  local dups
  dups=$(curl -sS -m 30 -H "x-litellm-api-key: $LITELLM_KEY" \
      "http://localhost:$PF_PORT/model/info" \
    | python3 -c '
import sys, json, collections
data = json.load(sys.stdin).get("data", [])
c = collections.Counter(m.get("model_name") for m in data)
dups = {k: v for k, v in c.items() if v > 1}
print(f"TOTAL={len(data)}")
for k, v in sorted(dups.items(), key=lambda kv: -kv[1])[:10]:
    print(f"DUP {k} {v}")
')
  echo "$dups" | grep '^TOTAL=' || true
  local n
  n=$(echo "$dups" | grep -c '^DUP ' || true)
  if [[ "$n" -eq 0 ]]; then
    ok "no duplicate model_name rows in LiteLLM"
  else
    bad "$n model_name(s) with >1 deployment row — duplicate amplification"
    echo "$dups" | grep '^DUP ' | sed 's/^/    /'
  fi
}
with_pf dup_check || true

if [[ "$DRY_RUN" == "1" ]]; then
  echo; echo "DRY_RUN — skipping real-provider inference"
  skip "V1 real-provider inference"
else
  # ─── V1: REAL provider credentials end-to-end ─────────────────────────────
  #
  # THE reason this runbook exists. The e2e mock accepts any api_key and any
  # api_base, so a misrouted model is indistinguishable from a correct one
  # there. Only a real provider call proves the credential and endpoint the
  # operator rendered are actually the right ones.
  section "V1 — real-provider inference (unmockable)"
  UAT_MODELS=${UAT_MODELS:-ackstorm.lite}
  infer_probe() {
    local model="$1" body
    body=$(curl -sS -m 45 -H "x-litellm-api-key: $LITELLM_KEY" \
      -H 'content-type: application/json' \
      -X POST "http://localhost:$PF_PORT/v1/chat/completions" \
      -d "{\"model\":\"${model}\",\"max_tokens\":40,\"messages\":[{\"role\":\"user\",\"content\":\"reply UAT-PROBE-OK\"}]}")
    if echo "$body" | python3 -c 'import sys,json;d=json.load(sys.stdin);assert d.get("choices") and d["choices"][0]["message"]["content"]' 2>/dev/null; then
      ok "inference OK via ${model}"
    else
      bad "inference FAILED via ${model}: $(echo "$body" | head -c 300)"
    fi
  }
  probe_all() { local m; for m in ${UAT_MODELS//,/ }; do infer_probe "$m"; done; }
  with_pf probe_all || true

  # MCP tool listing against REAL servers. e2e uses stub MCP backends, so tool
  # counts there prove nothing about live server reachability.
  section "V1b — MCP tool listing against real servers"
  mcp_probe() {
    local n
    n=$(curl -sS -m 30 -H "x-litellm-api-key: $LITELLM_KEY" \
          "http://localhost:$PF_PORT/v1/mcp/tools" \
        | python3 -c 'import sys,json;d=json.load(sys.stdin);t=d.get("tools",d) if isinstance(d,dict) else d;print(len(t))')
    [[ "$n" -gt 0 ]] && ok "MCP tools reachable, count = $n" || bad "MCP tools listing returned 0"
  }
  with_pf mcp_probe || true
fi

# ─── V3: real LiteLLM build ─────────────────────────────────────────────────
section "V3 — LiteLLM build in this cluster"
LL_IMG=$(kubectl -n "$LITELLM_NS" get deploy litellm \
  -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo unknown)
echo "litellm image: $LL_IMG"
# /v1/mcp/toolset only exists on 1.93.0+. A 404 here means the cluster is older
# than the operator's MCPToolset support, and toolset CRs cannot work.
toolset_probe() {
  local code
  code=$(curl -sS -m 10 -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $LITELLM_KEY" "http://localhost:$PF_PORT/v1/mcp/toolset")
  case "$code" in
    200) ok "/v1/mcp/toolset present (LiteLLM >= 1.93.0)" ;;
    404) warn "/v1/mcp/toolset 404 — LiteLLM < 1.93.0; LiteLLMMCPToolset unsupported here" ;;
    *)   bad "/v1/mcp/toolset unexpected HTTP $code" ;;
  esac
}
with_pf toolset_probe || true

# ─── Summary ────────────────────────────────────────────────────────────────
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
