# Grafana dashboards for aLiteLLM

Four dashboards, split by **who owns the metric**:

| File | uid | Folder | Source of its metrics |
|------|-----|--------|-----------------------|
| `operator/alitellm-operator.json` | `alitellm-operator` | aLiteLLM Operator | **this operator** — reconciles, drift corrections, discovery, conflicts, orphaned deletions, CR status age, controller-runtime |
| `litellm/alitellm-litellm-overview.json` | `alitellm-litellm-overview` | aLiteLLM | LiteLLM proxy **callback** (`PrometheusLogger`) — traffic, errors, latency split between provider and proxy |
| `litellm/alitellm-litellm-finops.json` | `alitellm-litellm-finops` | aLiteLLM | callback **plus** `litellm-exporter` — spend, tokens, budgets, key inventory |
| `litellm/alitellm-litellm-deployments.json` | `alitellm-litellm-deployments` | aLiteLLM | callback — per-deployment health, rate-limit headroom, proxy internals |

The `<group>` directory selects the Grafana folder (`metrics.dashboards.folders`
in the chart), which is why the operator's own dashboard lives apart from the
LiteLLM ones: we control the first set end to end, we only consume the second.
All four carry the `alitellm` tag and a tag-based dashboard link, so each one
links to the other three from its top bar.

## Design: the 03:00 test

The operator dashboard is laid out for the person who opens it because
something is broken, top row first:

1. **Is it healthy?** — six stats that are all green in a healthy system and
   name the failure domain when they are not: operator pods up, LiteLLM
   connection, reconcile errors, non-synced reconciles, stalest CR status age,
   CRs blocked in deletion. Volume counters (total reconciles) and
   permanently-zero-when-healthy curiosities (conflicts, orphaned deletions)
   live further down — they answer "what happened", not "is it broken".
2. **What is failing?** — the drill-down for the top row: errors per hour *by
   controller* (a single misbehaving discovery source shows up here named),
   the non-synced table (filtered to non-zero rows), the failure-reason mix,
   and discovery refresh outcomes per source.
3. **Activity / CR state / Controller runtime** — what the operator is doing,
   which CRs are stale or stuck, and the controller-runtime internals.

The `$namespace` variable was removed from the operator dashboard: the
operator's own metrics only ever exist in the namespace it runs in, and the
shared controller-runtime/workqueue families are scoped by
`job=~".*alitellm-operator.*"` instead. The LiteLLM dashboards keep the
variable — several proxies in different namespaces is a real deployment.

## Install

**Preferred — ship them with the release.** Set `metrics.dashboards.enabled=true`
in the chart: `make helm-sync` mirrors these JSONs into
`deploy/helm/alitellm-operator/dashboards/` and `templates/grafana-dashboards.yaml`
renders one ConfigMap per dashboard, labelled `grafana_dashboard: "1"` for the
kube-prometheus-stack Grafana sidecar to auto-load. No Prometheus Operator CRD is
involved (plain ConfigMaps). Provisioned dashboards are read-only in the UI —
edit them here. `make helm-sync-check` (pre-push gate 14b) fails on drift between
this directory and the chart copies.

The `folders` values only take effect where the sidecar runs with
`sidecar.dashboards.folderAnnotation=grafana_folder`; without it the annotation
is inert and everything lands in the sidecar provider's default folder.

**Manual** — Grafana → *Dashboards → New → Import → Upload JSON file*. Pick your
Prometheus datasource when prompted (the `datasource` template variable drives
every panel).

## What each dashboard needs

- **Operator dashboard**: `metrics.serviceMonitor.enabled=true` and
  **alitellm-operator ≥ v0.7.29**. The metric names were namespaced in that
  release (`reconcile_total` → `alitellm_operator_reconcile_outcome_total`,
  `connection_ready` → `alitellm_operator_connection_ready`, and so on); against
  an older operator these panels are empty.
- **LiteLLM dashboards**: a Prometheus job scraping the LiteLLM proxy with
  `callbacks: ["prometheus"]` enabled. The FinOps dashboard's *Inventory &
  budgets* row additionally needs the `litellm-exporter` — see below.

## Callback vs exporter — why both

The proxy callback only emits for entities that produced traffic **since the
proxy process started**, and its spend is a per-pod counter. The exporter reads
the LiteLLM database. Measured on one prod cluster, same moment:

| | exporter | callback |
|---|---|---|
| teams covered | 12 (all) | 6 |
| keys covered | 72 (all) | 9 |
| lifetime spend | $14.60 | $7.66 (since pod start) |
| key expiry | 86 series | no equivalent metric |

So the dashboards mix both on purpose: rates and per-request attribution come
from the callback, inventory and lifetime totals from the exporter. Each panel
that depends on the exporter says so in its description.

**Known exporter defect (2026-07-27):** `litellm_key_expiry` is documented as
seconds-until-expiry and does tick down at 1/s, but 61 of 86 keys read < 60s for
over an hour without ever expiring or disappearing. The dashboard therefore shows
the ordering, not a "keys expiring soon" alert tile. Fix belongs in the exporter.

## Rates vs counts

A stat reduced from `rate()` reads 0 whenever the counter is idle, which for an
operator is almost always: the reconcile counter ticked twice in six hours, and
`rate()` rendered that as `0 ops/s` while 698 controller-runtime errors showed
as `0.0333 ops/s`. Neither number moves anyone.

So the stats split by what the metric *is*:

- **Flows** — requests, tokens, spend — stay rates. They are genuinely rates and
  they scale to a busy proxy.
- **Incidents** — reconciles, conflicts, orphaned deletions, reconcile errors,
  deployment failures, cooldowns — are counts over the dashboard range
  (`increase(...[$__range])`, titled `(range)`). The question for a discrete
  event is how many, not how fast.

Note `increase()` extrapolates, so a count of 7 can compute as 7.02; the panels
render 0 decimals.

Timeseries panels keep `rate()` even for incident counters — the stat answers
"how many", the graph answers "when", and `increase(x[$__interval])` would make
the y-axis mean something different at every zoom level. But on the operator
dashboard the raw per-second rate was unreadable (698 errors in six hours is
0.03 ops/s), so every operator-dashboard rate is scaled `* 3600` and titled
`/h`: same zoom-independent semantics, human numbers. The LiteLLM dashboards
stay per-second — proxy traffic is a genuine per-second flow.

Rate windows use `$__rate_interval` (not a fixed `[5m]`) so zoomed-out views
stay correct; the deliberate slow windows (`[15m]` budget-reset job, `[1h]`
restart count) are the exception.

## Cardinality

`litellm_proxy_total_requests_metric_total` carries 16 labels, including
`client_ip`, `user_agent`, `end_user`, `hashed_api_key` and `user_email`;
`litellm_request_total_latency_metric_bucket` is already ~900 series on a small
cluster. Every panel here aggregates only over `team_alias`, `requested_model`,
`status_code`, `api_provider`, `model_id` or `litellm_model_name`. Keep it that
way, and consider trimming the label set in the LiteLLM config.

Unbounded by-model / by-team / by-key timeseries are additionally capped with
`topk(10, ...)` (or `bottomk(10, ...)` where the *lowest* values page, e.g.
remaining budget) so the panels stay readable on a proxy with hundreds of
models. `topk` on a range query re-evaluates per point, so series membership
can shift over the range — that is fine for "what is big right now".

## Alerts

`metrics.prometheusRule.enabled=true` in the chart ships a `PrometheusRule`
(`templates/prometheus-rules.yaml`) mirroring the operator dashboard's top
row: operator down (critical), LiteLLM connection not Ready 10m (critical),
sustained reconcile errors, repeated non-synced reconciles, CR status age
> 1h, deletions blocked 30m, discovery source failing 30m (all warning).
Requires the Prometheus Operator CRDs; needs operator ≥ v0.7.29 for the
`alitellm_operator_*` expressions. The stale-status threshold assumes the
default `safetyRelistInterval` (10m) — raise both together.

## Validate

```bash
bash examples/grafana/check-metric-names.sh
```

Checks the JSON parses, that no retired bare operator metric name (or
`exported_namespace`) crept back, that the LiteLLM rate-limit metrics use the
current `_metric` suffix, and that the expected families are still referenced.

## Prior art

LiteLLM ships a dashboard in
[`cookbook/litellm_proxy_server/grafana_dashboard`](https://github.com/BerriAI/litellm/tree/main/cookbook/litellm_proxy_server/grafana_dashboard)
(MIT). It is 7 real panels, and two of them query `litellm_remaining_requests` /
`litellm_remaining_tokens`, which current proxies emit as
`litellm_remaining_requests_metric` / `litellm_remaining_tokens_metric` — those
panels render empty. Useful as a starting shape; nothing here is copied from it.
