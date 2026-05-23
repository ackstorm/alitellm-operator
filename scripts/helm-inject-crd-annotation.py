#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""
Inject `helm.sh/resource-policy: keep` into each CRD's metadata.annotations.

Idempotent: skips files that already carry the annotation. Preserves the
controller-gen-emitted layout exactly (the annotation lands as a new line
inside the existing `annotations:` block).

Why we need this: with the chart moved to Option B (CRDs in templates/),
`helm uninstall` would otherwise delete the CRDs (and cascade-delete every
CR the user created). The `keep` policy preserves them on uninstall.

Used by `make helm-sync` after copying CRDs from config/crd/bases/.
"""

from __future__ import annotations
import pathlib
import re
import sys

ANNOTATION_LINE = "    helm.sh/resource-policy: keep"


def inject(path: pathlib.Path) -> bool:
    """Insert the helm annotation into the file. Returns True if modified."""
    text = path.read_text()
    if ANNOTATION_LINE in text:
        return False
    new_text, n = re.subn(
        r"^(  annotations:\n)",
        rf"\1{ANNOTATION_LINE}\n",
        text,
        count=1,
        flags=re.MULTILINE,
    )
    if n == 0:
        sys.stderr.write(
            f"ERROR: {path}: no `  annotations:` block found; cannot inject\n"
        )
        return False
    path.write_text(new_text)
    return True


def main() -> int:
    if len(sys.argv) < 2:
        sys.stderr.write(f"usage: {sys.argv[0]} <yaml-file> [<yaml-file> ...]\n")
        return 2
    rc = 0
    for arg in sys.argv[1:]:
        p = pathlib.Path(arg)
        if not p.is_file():
            sys.stderr.write(f"ERROR: not a file: {p}\n")
            rc = 1
            continue
        if inject(p):
            print(f"injected helm.sh/resource-policy=keep into {p}")
    return rc


if __name__ == "__main__":
    sys.exit(main())
