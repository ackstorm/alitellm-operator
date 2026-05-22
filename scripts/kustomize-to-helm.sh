#!/usr/bin/env bash
# scripts/kustomize-to-helm.sh — Phase 7 plan 07-02 (D-01); extended Phase 7.1 plan 07.1-01.
#
# Wraps `make build-installer` output in Helm {{ .Values.* }} placeholders.
#
# Input:  dist/install.yaml (kustomize-rendered)
# Output: deploy/helm/alitellm-operator/templates/install.yaml
#
# Substitutions (the 4 surfaces from D-02 + Tier 2 expansion):
#   1. image: controller:latest  →  image: {{ .Values.image.repo }}:{{ .Values.image.tag }}
#      + add imagePullPolicy: {{ .Values.image.pullPolicy }} on next line
#   2. WATCH_NAMESPACE env var   →  {{ .Values.watchNamespace }}
#      NOTE: the kustomize output does NOT include a WATCH_NAMESPACE env entry
#      (the operator reads the env var via envOr() in cmd/main.go). This script
#      ADDS the env block to the manager Deployment container so Helm users can
#      set watchNamespace via values.yaml.
#   3. toolhive ClusterRole+Bind →  wrap in {{- if .Values.toolhive.enabled }}...{{- end }}
#   4. ServiceMonitor (none in kustomize output yet) → emit a commented stub
#      gated by {{- if .Values.metrics.serviceMonitor.enabled }}...{{- end }}
#   5. extraEnv (Tier 2 plan amendment A5)  →  range .Values.extraEnv inside
#      the manager container env block, after WATCH_NAMESPACE. Lets values
#      files inject SAFETY_RELIST_INTERVAL, LOG_LEVEL, etc. without re-doing
#      helm-sync. Default in values.yaml is `extraEnv: []` so production
#      manifest is unchanged.
#   6. resources (Tier 2 plan amendment A5)  →  replace the hardcoded
#      manager container resources: block with `{{- toYaml .Values.resources
#      | nindent 10 }}`. Default in values.yaml mirrors the current kustomize
#      values so existing installs are unaffected.
#
# Phase 7.1 additions (D-7.1-04..D-7.1-08, D-7.1-11, D-7.1-IN04):
#   CR-04: escape {{TOKEN}} literals in CRD description: blocks → {{ "{{TOKEN}}" }}
#   CR-05: strip kind: CustomResourceDefinition documents (CRDs live in crds/ only)
#   CR-08: strip kind: Namespace documents (--create-namespace is the install flag)
#   CR-06: substitute workspace-system → {{ .Release.Namespace }} throughout
#   IN-04: assert WATCH_NAMESPACE present after Substitution 2 (fail loudly on drift)
#   CR-11: validate Service selectors match at least one Deployment pod-label set
#
# The kustomize source (config/default/) remains the canonical deploy unit
# per D-01; this script is a packaging veneer only. The generated output
# carries a "DO NOT EDIT" banner to enforce this discipline.
#
# Copyright 2026 ACKstorm
# Licensed under the Apache License, Version 2.0.

set -euo pipefail

INPUT="${1:-dist/install.yaml}"
OUTPUT="${2:-deploy/helm/alitellm-operator/templates/install.yaml}"

# ─── Step 1: Verify INPUT exists ────────────────────────────────────────────
if [ ! -f "${INPUT}" ]; then
    echo "ERROR: Input file '${INPUT}' not found." >&2
    echo "  Run 'make build-installer' first to generate the kustomize output." >&2
    exit 1
fi

echo "kustomize-to-helm.sh: transforming ${INPUT} → ${OUTPUT}"

# ─── Step 2: Run Python transformation ──────────────────────────────────────
# Python is used for the multi-document YAML transformation (multiline
# sed replacement for the toolhive conditional wrap + env injection is
# error-prone and less readable than a short Python script).

python3 - "${INPUT}" "${OUTPUT}" <<'PYEOF'
import sys
import re

input_file = sys.argv[1]
output_file = sys.argv[2]

with open(input_file, 'r') as f:
    content = f.read()

# ─── Substitution 1: Image reference ────────────────────────────────────────
# Replace: `        image: controller:latest`
# With:    `        image: {{ .Values.image.repo }}:{{ .Values.image.tag }}`
# And add: `        imagePullPolicy: {{ .Values.image.pullPolicy }}`
#
# The pattern targets ONLY the Deployment container image line (8-space indent
# in the kustomize default output). CRD-embedded strings don't contain
# "image: controller:latest" so this is safe.
content = re.sub(
    r'^(        image: )controller:latest$',
    r'\1{{ .Values.image.repo }}:{{ .Values.image.tag }}\n        imagePullPolicy: {{ .Values.image.pullPolicy }}',
    content,
    flags=re.MULTILINE,
)

# ─── CR-04 (D-7.1-04): Escape {{TOKEN}} literals inside CRD description: blocks
# Rewrite {{TOKEN}} → {{ "{{TOKEN}}" }} so Helm's text/template engine treats
# them as literal text rather than function calls.
# The regex matches {{ + identifier + }} anywhere in the document. All such
# occurrences in the current kustomize output live exclusively in CRD
# description: blocks (verified: {{NAME}}, {{ANTHROPIC_API_KEY}}, {{As}}, etc.).
# Running this pass BEFORE the CRD-doc strip (CR-05) ensures any future leakage
# into non-CRD docs is also caught.
content = re.sub(
    r'\{\{([A-Za-z_][A-Za-z0-9_]*)\}\}',
    r'{{ "{{\1}}" }}',
    content,
)

# ─── CR-05 (D-7.1-05): Strip kind: CustomResourceDefinition documents ───────
# CRDs are canonical in deploy/helm/alitellm-operator/crds/ (Phase 7 D-03). The
# install.yaml template must contain zero CRD documents.
_docs_cr05 = re.split(r'^---\n', content, flags=re.MULTILINE)
_result_cr05 = [
    doc for doc in _docs_cr05
    if not re.search(r'^kind:\s*CustomResourceDefinition\s*$', doc, flags=re.MULTILINE)
]
content = '---\n'.join(_result_cr05)

# ─── CR-08 (D-7.1-08): Strip kind: Namespace documents ──────────────────────
# The chart relies on `helm install --create-namespace`; shipping a Namespace
# document causes ownership-label conflicts on re-install.
_docs_cr08 = re.split(r'^---\n', content, flags=re.MULTILINE)
_result_cr08 = [
    doc for doc in _docs_cr08
    if not re.search(r'^kind:\s*Namespace\s*$', doc, flags=re.MULTILINE)
]
content = '---\n'.join(_result_cr08)

# ─── Substitution 2: WATCH_NAMESPACE env injection ──────────────────────────
# The kustomize default output does NOT include a WATCH_NAMESPACE env var in
# the Deployment (the operator reads envOr("WATCH_NAMESPACE","default")).
# We inject it after the `command:` block in the manager container so that
# Helm users can parameterize the watch namespace.
#
# Target pattern (in the Deployment):
#   command:
#   - /alitellm-operator
#   image: {{ .Values.image.repo }}:{{ .Values.image.tag }}
#
# We insert the env block BEFORE the `image:` line (which is already
# substituted by step 1 above).
content = content.replace(
    '        command:\n        - /alitellm-operator\n        image: {{ .Values.image.repo }}:{{ .Values.image.tag }}',
    '        command:\n        - /alitellm-operator\n        env:\n        - name: WATCH_NAMESPACE\n          value: {{ .Values.watchNamespace }}\n        {{- range .Values.extraEnv }}\n        - name: {{ .name }}\n          value: {{ .value | quote }}\n        {{- end }}\n        image: {{ .Values.image.repo }}:{{ .Values.image.tag }}',
)

# ─── Substitution 6 (Tier 2 A5): resources block templated from .Values.resources
# Replace the hardcoded manager container resources: block with a Helm template
# that emits the contents of .Values.resources. Defaults in values.yaml mirror
# the current kustomize numbers so production installs are unchanged.
#
# Target pattern (exact match against kustomize default output):
#         resources:
#           limits:
#             cpu: 500m
#             memory: 128Mi
#           requests:
#             cpu: 10m
#             memory: 64Mi
content = re.sub(
    r'^        resources:\n          limits:\n            cpu: \S+\n            memory: \S+\n          requests:\n            cpu: \S+\n            memory: \S+\n',
    '        resources:\n          {{- toYaml .Values.resources | nindent 10 }}\n',
    content,
    flags=re.MULTILINE,
)

# ─── IN-05 / IN-06: assert Tier 2 surfaces present ──────────────────────────
if '.Values.extraEnv' not in content:
    print("ERROR: extraEnv (Substitution 5) injection failed — kustomize output structure may have changed",
          file=sys.stderr)
    sys.exit(1)
if 'toYaml .Values.resources' not in content:
    print("ERROR: resources (Substitution 6) substitution failed — manager container resources: block did not match expected shape",
          file=sys.stderr)
    sys.exit(1)

# ─── IN-04 (D-7.1-IN04): Assert WATCH_NAMESPACE present after Substitution 2 ─
# Fail loudly if the env injection did not take effect. This guards against
# future kustomize-output changes that shift intermediate fields between
# `command:` and `image:` in dist/install.yaml.
if 'WATCH_NAMESPACE' not in content:
    print("ERROR: WATCH_NAMESPACE env injection failed — kustomize output structure may have changed "
          "(look for intermediate fields between `command:` and `image:` in dist/install.yaml)",
          file=sys.stderr)
    sys.exit(1)

# ─── CR-06 (D-7.1-06): Substitute workspace-system → {{ .Release.Namespace }} ─
# Enables `helm install --namespace <any-ns>` to deploy into the requested
# namespace. The kustomize source retains `namespace: workspace-system` as a
# build-time default; this script replaces every occurrence at chart-codegen
# time so the rendered template is namespace-agnostic.
content = content.replace('workspace-system', '{{ .Release.Namespace }}')

# ─── Substitution 3: toolhive ClusterRole + ClusterRoleBinding conditional ──
# Wrap both the ClusterRole and ClusterRoleBinding that contain
# "toolhive-reader" in their name with the toolhive.enabled Helm conditional.
#
# The ClusterRole and ClusterRoleBinding are separate YAML documents
# (separated by `---`). We identify them by the unique "toolhive-reader"
# string in their metadata.name field.

# Split on document boundaries (keeping the --- separators)
docs = re.split(r'^---\n', content, flags=re.MULTILINE)

result_docs = []
i = 0
toolhive_role_start = None  # index of first toolhive doc
toolhive_docs = []

while i < len(docs):
    doc = docs[i]
    if 'toolhive-reader' in doc:
        if toolhive_role_start is None:
            toolhive_role_start = len(result_docs)
        toolhive_docs.append(doc)
    else:
        if toolhive_role_start is not None and toolhive_docs:
            # Emit all accumulated toolhive docs wrapped in the if/end conditional
            wrapped = '{{- if .Values.toolhive.enabled }}\n'
            for j, td in enumerate(toolhive_docs):
                if j > 0:
                    wrapped += '---\n'
                wrapped += td
            wrapped = wrapped.rstrip('\n') + '\n{{- end }}\n'
            result_docs.append(wrapped)
            toolhive_docs = []
            toolhive_role_start = None
        result_docs.append(doc)
    i += 1

# Handle case where toolhive docs are at the end
if toolhive_docs:
    wrapped = '{{- if .Values.toolhive.enabled }}\n'
    for j, td in enumerate(toolhive_docs):
        if j > 0:
            wrapped += '---\n'
        wrapped += td
    wrapped = wrapped.rstrip('\n') + '\n{{- end }}\n'
    result_docs.append(wrapped)

# Re-join with --- separators
output_content = ''
for idx, doc in enumerate(result_docs):
    if idx == 0:
        output_content += doc
    else:
        output_content += '---\n' + doc

# ─── Substitution 4: ServiceMonitor stub ────────────────────────────────────
# The kustomize default does NOT include a ServiceMonitor. Add a commented
# stub at the end so future kustomize updates that include a ServiceMonitor
# flow through correctly.
servicemonitor_stub = """
---
{{- if .Values.metrics.serviceMonitor.enabled }}
# ServiceMonitor stub — generated by scripts/kustomize-to-helm.sh (plan 07-02 D-02).
# This block is activated when metrics.serviceMonitor.enabled=true.
# When prometheus-operator is installed and a ServiceMonitor is added to
# config/default/prometheus/ and included in config/default/kustomization.yaml,
# `make helm-sync` will replace this stub with the rendered ServiceMonitor.
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: alitellm-operator-metrics
  namespace: {{ .Release.Namespace }}
  labels:
    app.kubernetes.io/name: alitellm-operator
    app.kubernetes.io/managed-by: helm
spec:
  selector:
    matchLabels:
      control-plane: alitellm-operator
  namespaceSelector:
    matchNames:
      - {{ .Release.Namespace }}
  endpoints:
    - port: metrics
      path: /metrics
      interval: 30s
{{- end }}
"""
output_content = output_content.rstrip('\n') + '\n' + servicemonitor_stub

# ─── CR-11 (D-7.1-11): Validate Service selectors find ≥1 pod ───────────────
# Parse the final output with a regex-based scanner. For every Service doc,
# collect spec.selector key-value pairs. For every Deployment doc, collect
# spec.template.metadata.labels key-value pairs. Assert each Service's selector
# is a subset of at least one Deployment's pod-template label set.
# Failure means a zero-endpoint Service would ship — caught at codegen time.
_cr11_docs = re.split(r'^---\n', output_content, flags=re.MULTILINE)

def _parse_kv_block(text, indent_prefix):
    """Extract key: value pairs from an indented block at the given prefix."""
    pairs = {}
    pattern = re.compile(r'^' + re.escape(indent_prefix) + r'([^:\s][^:]*?):\s*(.+)$', re.MULTILINE)
    for m in pattern.finditer(text):
        key = m.group(1).strip()
        val = m.group(2).strip()
        # Skip nested blocks (values starting with '{' or multi-line anchors)
        if not val.startswith('{') and not val.startswith('|') and not val.startswith('>'):
            pairs[key] = val
    return pairs

# Collect Services: name → selector dict
_cr11_services = {}
# Collect Deployment pod-template label sets
_cr11_deploy_labels = []  # list of dicts

for doc in _cr11_docs:
    # Skip toolhive conditional wrappers — they are Helm template blocks, not YAML
    if '{{- if .Values.toolhive.enabled }}' in doc:
        continue
    # Determine kind
    kind_m = re.search(r'^kind:\s*(\S+)', doc, re.MULTILINE)
    if not kind_m:
        continue
    kind = kind_m.group(1)

    if kind == 'Service':
        name_m = re.search(r'^  name:\s*(.+)$', doc, re.MULTILINE)
        svc_name = name_m.group(1).strip() if name_m else '<unknown>'
        # Find selector block under spec:
        sel_m = re.search(r'  selector:\n((?:    [^\n]+\n?)+)', doc)
        if sel_m:
            sel_block = sel_m.group(1)
            sel_kv = _parse_kv_block(sel_block, '    ')
            if sel_kv:
                _cr11_services[svc_name] = sel_kv

    elif kind == 'Deployment':
        # Find spec.template.metadata.labels block
        labels_m = re.search(
            r'  template:\n(?:.*\n)*?    metadata:\n(?:.*\n)*?      labels:\n((?:        [^\n]+\n?)+)',
            doc
        )
        if labels_m:
            lbl_block = labels_m.group(1)
            lbl_kv = _parse_kv_block(lbl_block, '        ')
            if lbl_kv:
                _cr11_deploy_labels.append(lbl_kv)

for svc_name, selector in _cr11_services.items():
    # A selector is matched if ALL its key-value pairs are present in at least
    # one Deployment's pod-template label set.
    matched = any(
        all(deploy_labels.get(k) == v for k, v in selector.items())
        for deploy_labels in _cr11_deploy_labels
    )
    if not matched:
        print(f"ERROR: Service '{svc_name}' selector {selector} matches no Deployment pod labels "
              f"— chart will produce a zero-endpoint Service", file=sys.stderr)
        sys.exit(1)

# ─── Step 4: Add generated banner at the top ────────────────────────────────
banner = f"# Generated by scripts/kustomize-to-helm.sh from {input_file} — DO NOT EDIT.\n"
# Remove any existing banner
lines = output_content.split('\n')
if lines and lines[0].startswith('# Generated by'):
    output_content = '\n'.join(lines[1:])
output_content = banner + output_content

with open(output_file, 'w') as f:
    f.write(output_content)

print(f"kustomize-to-helm.sh: wrote {output_file} ({len(output_content)} bytes)")
PYEOF

# ─── Step 5: Optional helm template self-check ──────────────────────────────
if command -v helm >/dev/null 2>&1; then
    echo "kustomize-to-helm.sh: running 'helm template deploy/helm/alitellm-operator/' self-check..."
    CHART_DIR="$(dirname "${OUTPUT}")/.."
    if helm template "${CHART_DIR}" >/dev/null; then
        echo "kustomize-to-helm.sh: helm template PASS"
    else
        echo "ERROR: helm template failed — check template syntax in ${OUTPUT}" >&2
        exit 1
    fi
else
    echo "kustomize-to-helm.sh: helm not found on PATH — skipping self-check"
fi

echo "kustomize-to-helm.sh: done."
