#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Verify the dashboards track the operator's namespaced metric names and the
# LiteLLM names our proxy version actually emits.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mapfile -t dashboards < <(find "$root" -mindepth 2 -maxdepth 2 -name '*.json' | sort)
[[ ${#dashboards[@]} -gt 0 ]] || { echo "no dashboards found under $root/*/" >&2; exit 1; }

for dashboard in "${dashboards[@]}"; do
  jq empty "$dashboard"
done

# 1. Operator metrics: the bare names retired in v0.7.29 must not come back.
#    They were unprefixed and collided with any other exporter in the same TSDB
#    (`reconcile_total` is nobody's metric in particular).
obsolete_operator='"[^"]*(?<![A-Za-z0-9_])(reconcile_total|connection_ready|cr_status_age_seconds|discovery_refresh_total|discovery_generated_count|discovery_failed_total|child_cr_writes_total|drift_corrected_total)(?![A-Za-z0-9_])'
if rg --pcre2 -n "$obsolete_operator" "${dashboards[@]}" | rg -v 'alitellm_operator_'; then
  echo "obsolete bare operator metric reference found (prefix it with alitellm_operator_)" >&2
  exit 1
fi

# 2. Operator metrics carry the CR namespace as cr_namespace. A metric label
#    named `namespace` collides with the target label and reaches the TSDB as
#    `exported_namespace`, so grouping by it silently groups by the operator
#    pod's own namespace.
if rg -n 'exported_namespace' "${dashboards[@]}"; then
  echo "dashboards must group operator metrics by cr_namespace, not exported_namespace" >&2
  exit 1
fi

# 3. LiteLLM renamed these two; the upstream cookbook dashboard still queries the
#    pre-rename names and renders empty against a current proxy.
obsolete_litellm='(?<![A-Za-z0-9_])litellm_remaining_(requests|tokens)(?!_metric)(?![A-Za-z0-9_])'
if rg --pcre2 -n "$obsolete_litellm" "${dashboards[@]}"; then
  echo "use litellm_remaining_{requests,tokens}_metric (the _metric suffix is current)" >&2
  exit 1
fi

required=(
  # operator (this repo)
  alitellm_operator_reconcile_total
  alitellm_operator_reconcile_outcome_total
  alitellm_operator_conflicts_total
  alitellm_operator_deletion_orphaned_total
  alitellm_operator_cr_status_age_seconds
  alitellm_operator_connection_ready
  alitellm_operator_drift_corrected_total
  # LiteLLM proxy callback
  litellm_proxy_total_requests_metric_total
  litellm_request_total_latency_metric_bucket
  litellm_spend_metric_total
  litellm_deployment_state
  # litellm-exporter (DB truth)
  litellm_total_spend
  litellm_team_spend
  litellm_key_expiry
)
for metric in "${required[@]}"; do
  rg -q --fixed-strings "$metric" "${dashboards[@]}" || {
    echo "expected metric reference missing: $metric" >&2
    exit 1
  }
done

echo "grafana metric-name check OK (${#dashboards[@]} dashboards)"
