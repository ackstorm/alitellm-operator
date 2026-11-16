# LiteLLM `/key/*` — measured contract

Measured against the e2e LiteLLM (`test/e2e/cluster/01-deps/litellm.values.yaml`
`image.tag`, v1.106.0) with the master key. `internal/litellm/key.go` and the
mock's `writeKey` follow this file.

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

## Rules the operator relies on

- `key` must start with `sk-` and be at least 16 characters.
- A key is identified by the sha256 hex of its value everywhere except the
  `/key/generate` body. A raw key in a query string is written to HTTP access
  logs.
- `key_alias` is unique across all keys.
- A key with no team and `models: []` can call every model.
- `general_settings.disable_custom_api_keys: true` on the proxy makes
  `/key/generate` with a caller `key` answer 403.
- A deleted key stays readable through `/key/info` with `status: "deleted"`;
  existence checks must test `status`, not just the HTTP code.
