# LiteLLM `/key/*` — measured contract

Measured with the master key against LiteLLM v1.106.0; the `allowed_routes`
rows also against v1.111.0-dev.4, with identical results. e2e `KEY-01` and
`KEY-02` re-check the key lifecycle and the route rows on the pinned image
(`test/e2e/cluster/01-deps/litellm.values.yaml`). `internal/litellm/key.go`
and the mock's `writeKey` follow this file.

| Call | Measured |
|---|---|
| `POST /key/generate` with a caller `key` | 200; response `key` equals the supplied value; echoes `key_alias`, `models`, `max_budget`, `rpm_limit`, `budget_duration`, `metadata` |
| `GET /key/info?key=<sha256>` | 200; `info` holds `key_alias`, `models`, `max_budget` (5.0), `budget_duration` ("30d"), `rpm_limit` (60), `metadata`, `status: "active"`; `key_name` is masked (`sk-...cdef`) |
| `POST /key/generate`, alias already used | 400; `error.param` = `key_alias`, message "Key with alias 'key-probe' already exists. Unique key aliases across all keys are required." |
| `GET /key/list?key_alias=<alias>` | 200; `{"keys":["<sha256>"],"total_count":1,...}`; `keys` holds the hash |
| `GET /key/list?key_alias=kp` with keys aliased `kp` and `kp-api` | 200; one hash, the `kp` key: the alias filter is an exact match, not a substring |
| `GET /key/info` for a hash that never existed | 404; `error.type` = `not_found_error`, message "Key not found in database" |
| `POST /key/update` with `null` budget and limits | 200; models replaced (`["m2"]`); `max_budget`, `budget_duration`, `rpm_limit`, `tpm_limit` all `null` afterwards (info confirms) |
| `POST /key/delete` `{"keys":[<sha256>]}` | 200; `{"deleted_keys":["<sha256>"]}` |
| `GET /key/info` for a deleted key | 200 (not 404); `info.status` = `"deleted"`, `deleted_at` set |
| `POST /key/delete` for a missing key | 404; `{'error': 'No keys found'}` (`error.type` is `internal_server_error`, `code` "404") |
| `POST /key/generate` reusing a deleted key's value | 200; same `key` value accepted, alias reusable |
| `GET /key/info` for a key generated without `allowed_routes` | `info.allowed_routes` is `[]` |
| `POST /key/update` with `allowed_routes: []` | 200; `info.allowed_routes` is `[]` afterwards |
| `POST /key/update` with `allowed_routes: null` | 400; the stored list is unchanged |

Routes, with `litellm_settings.require_auth_for_metrics_endpoint: true` and the
prometheus callback:

| Key | Call | Measured |
|---|---|---|
| none | `GET /metrics/` | 401 |
| `allowed_routes: ["/metrics"]` | `GET /metrics/` | 200, Prometheus text |
| `allowed_routes: ["/metrics"]` | `GET /metrics` | 307 to `/metrics/` |
| `allowed_routes: ["/metrics"]` | `POST /v1/chat/completions` | 403; "Virtual key is not allowed to call this route. Only allowed to call routes: ['/metrics']" |
| no `allowed_routes` | `GET /metrics/` | 401; "Only proxy admin can be used to generate, delete, update info for new keys/users/teams. Route=/metrics/" |
| `models: []`, `allowed_routes: ["/v1/chat/completions"]` | `POST /v1/chat/completions` | reaches the provider: the model check passes for any model |
| `models: ["no-default-models"]`, same route | `POST /v1/chat/completions` | 403; `error.type` = `key_model_access_denied` |

## Rules the operator relies on

- `key` must start with `sk-` and be at least 16 characters.
- A key is identified by the sha256 hex of its value everywhere except the
  `/key/generate` body. A raw key in a query string is written to HTTP access
  logs.
- `key_alias` is unique across all keys.
- A key with no team and `models: []` can call every model, on any route
  `allowed_routes` leaves open. `models: ["no-default-models"]` is the only
  closed form, so the operator sends it for a key that lists no model.
- `allowed_routes` is cleared with `[]`. An empty list means no route
  restriction; `null` is rejected.
- A matching `allowed_routes` entry opens an admin-only route to a non-admin
  key (`non_proxy_admin_allowed_routes_check`, read in v1.111.0-dev.4 source).
- `general_settings.disable_custom_api_keys: true` on the proxy makes
  `/key/generate` with a caller `key` answer 403.
- A deleted key stays readable through `/key/info` with `status: "deleted"`;
  existence checks must test `status`, not just the HTTP code.
