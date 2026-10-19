# LiteLLM model access-group (tag) budget API — measured behaviour

Measured 2026-10-18 on LiteLLM image `ghcr.io/berriai/litellm-database:v1.106.0` (chart 1.106.0), e2e kind cluster, master key auth.
Tag `probe-tag` came from a model created with `model_info.access_groups: ["probe-tag"]`.

## Summary
- GET always 200 for a known tag; `budget: null` when none. Unknown tag -> 404 `{"detail":{"error":"Access group '<tag>' not found"}}` (GET, PUT, DELETE alike).
- PUT is a MERGE per field: omitted fields KEEP their value (same `budget_id`, update in place).
- `null` does NOT clear a field (`soft_budget:null` left soft_budget=4.0). Clearing a single field is not possible via PUT; only DELETE removes the budget.
- DELETE on an absent budget is 200 with `budget_deleted:false` (not 404). DELETE on a known tag with a budget: 200 `budget_deleted:true`.
- GET response shape: `{"access_group":<tag>,"spend":<float>,"budget":null|{"budget_id":<uuid>,"max_budget":<float|null>,"soft_budget":<float|null>,"budget_duration":<string|null>,"budget_reset_at":<string|null>}}`. PUT returns the same shape.

## Raw results
1. GET probe-tag/budget (no budget): `200 {"access_group":"probe-tag","spend":0.0,"budget":null}`
2. GET nonexistent-tag/budget: `404 {"detail":{"error":"Access group 'nonexistent-tag' not found"}}`
3. PUT probe-tag `{"max_budget":5,"soft_budget":4,"budget_duration":"30d"}`: `200 {"access_group":"probe-tag","spend":0.0,"budget":{"budget_id":"4b08d2f7-ef3c-47af-91c8-fbc68458a852","max_budget":5.0,"soft_budget":4.0,"budget_duration":"30d","budget_reset_at":null}}`; GET identical.
4. PUT `{"max_budget":6}`: `200 ... "max_budget":6.0,"soft_budget":4.0,"budget_duration":"30d"` same budget_id -> merge.
5. PUT `{"max_budget":6,"soft_budget":null}`: `200 ... "soft_budget":4.0` unchanged -> null ignored; GET identical.
6. PUT nonexistent-tag `{"max_budget":1}`: `404 {"detail":{"error":"Access group 'nonexistent-tag' not found"}}`
7. DELETE probe-tag/budget: `200 {"access_group":"probe-tag","budget_deleted":true,"message":"Budget for access group 'probe-tag' deleted successfully"}`; GET after: `200 {"access_group":"probe-tag","spend":0.0,"budget":null}`; second DELETE: `200 {"access_group":"probe-tag","budget_deleted":false,"message":"Access group 'probe-tag' has no budget to delete"}`
8. DELETE nonexistent-tag/budget: `404 {"detail":{"error":"Access group 'nonexistent-tag' not found"}}`

Note: the tag's existence depends on a model carrying it; after the last model with the tag is deleted, expect 404 for that tag.
