# Access Group Tag Fields Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (inline) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `LiteLLMAccessGroup` gets a name field and a tag field per dimension — `models`/`modelGroups`, `mcpServers`/`mcpServerGroups`, `agents`/`agentGroups` — so groups are written as tags instead of long name lists.

**Architecture:** `modelGroups` is unioned with `models` into `access_model_names` (LiteLLM expands model tags natively). `mcpServerGroups` mirrors the existing `agentGroups`: the operator selects every registered, non-deleting `LiteLLMMCPServer` in the namespace whose `params.mcp_access_groups` (or `access_groups` alias) carries a tag, and unions its `serverID` into `access_mcp_server_ids`; a `LiteLLMMCPServer` watch re-drives tag-selecting groups. One generic tag-selection helper serves agents and MCP servers.

**Tech Stack:** Go, controller-runtime, kubebuilder, envtest.

## Global Constraints

- Additive, patch release (v0.8.14). `models` keeps accepting tags (no way to tell a tag from a name); docs steer tags to `modelGroups`.
- `mcpServerGroups` selection = registered (`status.lastRendered.serverID != ""`), not deleting, tag in `mcp_access_groups`, else `access_groups` (same precedence as `extractMCPParams` in `params_mcp.go`).
- A tag matching nothing is not an error; tag matches never count as missing names.
- Gitops migration must render the **identical** `access_mcp_server_ids` / `access_model_names` per group as before; verify against prod status before pushing, show a diff and stop if not identical.
- Out of scope: stripping tags from `models`; group fields on `LiteLLMTeam`.

---

### Task 1: Operator — `modelGroups` + `mcpServerGroups`

**Files:**
- Modify: `api/litellm/v1alpha1/accessgroup_types.go` (two fields + docs)
- Modify: `internal/controller/accessgroup_controller.go` (render, generic tag helper, reconcile Step 4, watch + mapper, RBAC)
- Test: `internal/controller/accessgroup_controller_test.go`
- Docs: `docs/user-guide/access-group.md`, `docs/api-reference/litellm.ackstorm.ai.md`, `CHANGELOG.md`, chart via `make helm-sync`

**Interfaces:**
- Produces: `AccessGroupSpec.ModelGroups []string \`json:"modelGroups,omitempty"\``, `AccessGroupSpec.MCPServerGroups []string \`json:"mcpServerGroups,omitempty"\``; `func renderAccessGroup(spec, serverIDs, agentIDs map[string]string, taggedServers, taggedAgents []string)`; `func taggedIDs(items []taggable, groups []string) []string`; `func taggedMCPServerIDs(servers []litellmv1alpha1.LiteLLMMCPServer, groups []string) []string`; `func (r *AccessGroupReconciler) mcpServerToAccessGroups(ctx, obj) []reconcile.Request`.

- [ ] **Step 1: Failing unit tests** (append to `accessgroup_controller_test.go`)

```go
func mcpWithTags(name, serverID, params string, deleting bool) litellmv1alpha1.LiteLLMMCPServer {
	s := litellmv1alpha1.LiteLLMMCPServer{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: WatchNamespace}}
	s.Status.LastRendered.ServerID = serverID
	if params != "" {
		s.Spec.Params = runtime.RawExtension{Raw: []byte(params)}
	}
	if deleting {
		now := metav1.Now()
		s.DeletionTimestamp = &now
	}
	return s
}

// TestTaggedMCPServerIDs pins the selection rule: mcp_access_groups wins over
// the access_groups alias, registered only, not deleting.
func TestTaggedMCPServerIDs(t *testing.T) {
	servers := []litellmv1alpha1.LiteLLMMCPServer{
		mcpWithTags("a", "s-a", `{"access_groups":["default"]}`, false),
		mcpWithTags("b", "s-b", `{"mcp_access_groups":["default","ro"]}`, false),
		mcpWithTags("c", "s-c", `{"access_groups":["other"]}`, false),
		mcpWithTags("d", "", `{"access_groups":["default"]}`, false),
		mcpWithTags("e", "s-e", `{"access_groups":["default"]}`, true),
		mcpWithTags("f", "s-f", ``, false),
		mcpWithTags("g", "s-g", `{"mcp_access_groups":["x"],"access_groups":["default"]}`, false),
	}
	if got, want := taggedMCPServerIDs(servers, []string{"default"}), []string{"s-a", "s-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("taggedMCPServerIDs = %v, want %v", got, want)
	}
	if got := taggedMCPServerIDs(servers, nil); len(got) != 0 {
		t.Fatalf("no groups must select nothing, got %v", got)
	}
}

// TestRenderAccessGroup_UnionsModelGroupsAndTaggedServers: modelGroups join
// models; tagged server ids join resolved names; sorted, deduped, never missing.
func TestRenderAccessGroup_UnionsModelGroupsAndTaggedServers(t *testing.T) {
	spec := litellmv1alpha1.AccessGroupSpec{
		Models: []string{"gpt-x", "openai"}, ModelGroups: []string{"openai", "a2a"},
		MCPServers: []string{"slack"}, MCPServerGroups: []string{"default"},
	}
	got, missing := renderAccessGroup(spec, map[string]string{"slack": "s-1"}, nil, []string{"s-2", "s-1"}, nil)
	if len(missing.MCPServers) != 0 {
		t.Fatalf("missing = %v", missing.MCPServers)
	}
	if want := []string{"a2a", "gpt-x", "openai"}; !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("Models = %v, want %v", got.Models, want)
	}
	if want := []string{"s-1", "s-2"}; !reflect.DeepEqual(got.MCPServerIDs, want) {
		t.Fatalf("MCPServerIDs = %v, want %v", got.MCPServerIDs, want)
	}
}
```
Update the existing `renderAccessGroup(...)` calls in this file to the new 5-arg signature: `renderAccessGroup(spec, serverIDs, agentIDs, nil, nil)`; the agent-tag test passes its ids as the 5th arg.

Run: `make test-unit-pkg PKG=./internal/controller/... ; echo EXIT=$?` → compile failure (`ModelGroups`, `taggedMCPServerIDs` undefined).

- [ ] **Step 2: CRD fields** — in `AccessGroupSpec`, after `Models`:
```go
	// ModelGroups lists legacy model access-group TAGS (model_info.access_groups,
	// e.g. `openai`, `a2a`). Unioned with Models into access_model_names;
	// LiteLLM expands tags itself. Prefer this over putting tags in Models.
	//
	// +optional
	ModelGroups []string `json:"modelGroups,omitempty"`
```
after `MCPServers`:
```go
	// MCPServerGroups is a list of MCP server access-group TAGS. Every
	// LiteLLMMCPServer in this namespace whose spec.params.mcp_access_groups
	// (or the access_groups alias) contains one of these tags, and which is
	// registered (status.lastRendered.serverID set), is added to this group's
	// access_mcp_server_ids alongside spec.mcpServers. Re-resolved whenever any
	// LiteLLMMCPServer changes. A tag matching no server is not an error.
	//
	// SECURITY: this widens automatically — tagging a new server grants it to
	// every team that attaches this group.
	//
	// +optional
	MCPServerGroups []string `json:"mcpServerGroups,omitempty"`
```
Update the `Models` doc: "Entries may be names or tags; put tags in ModelGroups."
Run: `make gen-manifests gen-code; echo EXIT=$?` → 0.

- [ ] **Step 3: Controller**

Replace `taggedAgentIDs` with a generic helper plus two thin adapters:
```go
// taggable is one registered-or-not resource that may carry access-group tags.
type taggable struct {
	id       string
	deleting bool
	tags     []string
}

// taggedIDs returns the sorted, deduped ids of items carrying any of groups.
// Unregistered items (no id yet) and items being deleted are skipped: the
// former join on the status update that registers them, the latter must lose
// access promptly.
func taggedIDs(items []taggable, groups []string) []string {
	if len(groups) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	for _, it := range items {
		if it.id == "" || it.deleting {
			continue
		}
		for _, tag := range it.tags {
			if slices.Contains(groups, tag) {
				seen[it.id] = struct{}{}
				break
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func paramsMap(raw []byte) map[string]any {
	var p map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	return p
}

func taggedAgentIDs(agents []litellmv1alpha1.LiteLLMA2AAgent, groups []string) []string {
	items := make([]taggable, 0, len(agents))
	for i := range agents {
		a := &agents[i]
		items = append(items, taggable{a.Status.LastRendered.AgentID, !a.DeletionTimestamp.IsZero(), agentAccessGroupsFromParams(paramsMap(a.Spec.Params.Raw))})
	}
	return taggedIDs(items, groups)
}

func taggedMCPServerIDs(servers []litellmv1alpha1.LiteLLMMCPServer, groups []string) []string {
	items := make([]taggable, 0, len(servers))
	for i := range servers {
		s := &servers[i]
		items = append(items, taggable{s.Status.LastRendered.ServerID, !s.DeletionTimestamp.IsZero(), extractMCPParams(paramsMap(s.Spec.Params.Raw)).MCPAccessGroups})
	}
	return taggedIDs(items, groups)
}
```
(`agentAccessGroupsFromParams` / `extractMCPParams` must tolerate a nil map — check; they index a map, which is nil-safe in Go.)

`renderAccessGroup` → signature `(spec, serverIDs, agentIDs map[string]string, taggedServers, taggedAgents []string)`:
```go
	models := append(append([]string{}, spec.Models...), spec.ModelGroups...)
	sort.Strings(models)
	models = slices.Compact(models)

	servers, missingServers := resolveNames(spec.MCPServers, serverIDs)
	missing.MCPServers = missingServers
	servers = append(servers, taggedServers...)
	sort.Strings(servers)
	servers = slices.Compact(servers)
```
(agents block unchanged except `tagged` → `taggedAgents`). Update the doc comment.

Reconcile Step 4, after the agent-tag block:
```go
	var taggedServers []string
	if len(ag.Spec.MCPServerGroups) > 0 {
		var servers litellmv1alpha1.LiteLLMMCPServerList
		if err := r.List(ctx, &servers, client.InNamespace(ag.Namespace)); err != nil {
			return ctrl.Result{}, err
		}
		taggedServers = taggedMCPServerIDs(servers.Items, ag.Spec.MCPServerGroups)
	}
```
and `renderAccessGroup(ag.Spec, serverIDs, agentIDs, taggedServers, tagged)`.

RBAC marker: `// +kubebuilder:rbac:groups=litellm.ackstorm.ai,resources=litellmmcpservers,verbs=get;list;watch`.

Watch + mapper (next to the agent one; update the SetupWithManager doc):
```go
		Watches(
			&litellmv1alpha1.LiteLLMMCPServer{},
			handler.EnqueueRequestsFromMapFunc(r.mcpServerToAccessGroups),
		).
```
```go
// mcpServerToAccessGroups enqueues every access group in the server's
// namespace that selects MCP servers by tag (a REMOVED tag must shrink too).
func (r *AccessGroupReconciler) mcpServerToAccessGroups(ctx context.Context, obj client.Object) []reconcile.Request {
	var groups litellmv1alpha1.LiteLLMAccessGroupList
	if err := r.List(ctx, &groups, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list access groups for mcp server fan-in")
		return nil
	}
	var reqs []reconcile.Request
	for _, g := range groups.Items {
		if len(g.Spec.MCPServerGroups) > 0 {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&g)})
		}
	}
	return reqs
}
```
Run: `make test-unit-pkg PKG=./internal/controller/... ; echo EXIT=$?` is envtest-backed for this package — use `make test-envtest-pkg PKG=./internal/controller/... FOCUS='AccessGroup|TaggedMCP|TaggedAgent' TIMEOUT=15m; echo EXIT=$?` → PASS.

- [ ] **Step 4: Envtest** — `TestAccessGroup_MCPServerGroupsFollowServerTags`, mirroring `TestAccessGroup_AgentGroupsFollowAgentTags`:
```go
func TestAccessGroup_MCPServerGroupsFollowServerTags(t *testing.T) {
	ctx := context.Background()
	name, serverName := "ag-mcp-tags-test", "mcp-tags-test"
	resetMockAccessGroup()
	ensureNoAccessGroup(t, ctx, name)
	ensureNoMCPServer(t, ctx, serverName)
	resetConnCacheSnapshot()
	cleanupConn := setupReadyConnectionAccessGroup(t, ctx)
	t.Cleanup(func() {
		cleanupConn()
		setConnCacheReady()
		ensureNoAccessGroup(t, context.Background(), name)
		ensureNoMCPServer(t, context.Background(), serverName)
	})

	if err := k8sClient.Create(ctx, accessGroupSampleCR(name, litellmv1alpha1.AccessGroupSpec{MCPServerGroups: []string{"default"}})); err != nil {
		t.Fatalf("create group: %v", err)
	}
	pollAccessGroupCondition(t, ctx, name, reasonSynced)

	srv := mcpServerSampleCR(serverName)
	srv.Spec.Params = runtime.RawExtension{Raw: []byte(`{"mcp_info":{"description":"sample"},"access_groups":["default"]}`)}
	if err := k8sClient.Create(ctx, srv); err != nil {
		t.Fatalf("create server: %v", err)
	}
	var serverID string
	waitFor(t, accessGroupPollTimeout, func() bool {
		var s litellmv1alpha1.LiteLLMMCPServer
		if k8sClient.Get(ctx, client.ObjectKey{Name: serverName, Namespace: WatchNamespace}, &s) != nil {
			return false
		}
		serverID = s.Status.LastRendered.ServerID
		g := mockAccessGroupByName(name)
		return serverID != "" && g != nil && slices.Contains(g.AccessMCPServerIDs, serverID)
	}, "tagged server never reached access_mcp_server_ids")

	var s litellmv1alpha1.LiteLLMMCPServer
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: serverName, Namespace: WatchNamespace}, &s); err != nil {
		t.Fatal(err)
	}
	s.Spec.Params = runtime.RawExtension{Raw: []byte(`{"mcp_info":{"description":"sample"},"access_groups":["other"]}`)}
	if err := k8sClient.Update(ctx, &s); err != nil {
		t.Fatal(err)
	}
	waitFor(t, accessGroupPollTimeout, func() bool {
		g := mockAccessGroupByName(name)
		return g != nil && !slices.Contains(g.AccessMCPServerIDs, serverID)
	}, "untagged server was not removed from access_mcp_server_ids")
}
```
(Check the mock's field name for server ids — `grep -n AccessMCPServerIDs internal/litellm/*mock* test/`; use whatever the mock exposes.)

Run: `make test-envtest-pkg PKG=./internal/controller/... FOCUS='AccessGroup|MCPServer' TIMEOUT=15m; echo EXIT=$?` → PASS.

- [ ] **Step 5: Docs, generate, lint, commit**
- `docs/user-guide/access-group.md`: table of the six fields (name vs tag, where each tag lives: `info.access_groups` / `params.access_groups` on MCP servers and agents) + example using `modelGroups`/`mcpServerGroups`/`agentGroups`.
- API reference: add both fields by hand (no generator target).
- CHANGELOG "Added: `LiteLLMAccessGroup.spec.modelGroups` and `spec.mcpServerGroups`".
```bash
make gen-manifests gen-code helm-sync; echo EXIT=$?
make qa-lint-changed; echo EXIT=$?   # after commit (lints committed diff vs origin/main)
make test-unit > "$LOG" 2>&1; echo EXIT=$?; grep -E -- '--- FAIL|^FAIL' "$LOG"
git commit -m "feat(accessgroup): add modelGroups and mcpServerGroups"
```

---

### Task 2: Gitops migration (after v0.8.14 is in prod)

**Files:** `/workspace/private/ackstorm/nglz-genai/gitops-genai-blueprint/workloads/config/access-groups.yaml`

- [ ] **Step 1: Baseline.** `status.lastRendered` holds only hash/id, so the baseline is by NAME: per group, the current `spec.mcpServers` list. With the EKS tool (`account_id 047612973873`, cluster `pro-ack-ai-platform`) read every `LiteLLMMCPServer` in `ackstorm`: `metadata.name`, `spec.params` tags (`mcp_access_groups`, else `access_groups`), registered (`status.lastRendered.serverID` set).
- [ ] **Step 2: Pick tags.** Per group, choose the tag set whose selected server NAMES ⊆ the current list; leftovers stay in `mcpServers`. The union (tag-selected ∪ leftovers) must equal the current list exactly — a tag that would add a server not in the list is not used. Unregistered servers count by name (they would join on registration either way).
- [ ] **Step 3: Rewrite.** Per group: `models:` → `modelGroups:` (all current entries are tags), `mcpServers:` → `mcpServerGroups:` + leftover `mcpServers:`. `kustomize build workloads/config > /dev/null; echo EXIT=$?`.
- [ ] **Step 4: Show the user** the per-group before/after and the "identical set" check; push only on their OK. Never touch `apps/litellm/base/files/a2a1_handler.py`.
- [ ] **Step 5: Verify after Flux.** Each group `Ready=True Synced`; `status.lastRendered.hash` UNCHANGED (same rendered sets hash identically — the projection is sorted); `/v1/models` and the MCP list with the user key unchanged.
