#!/usr/bin/env python3
"""validate_yaml.py (issue #117): every .yaml/.yml under deploy/ that is NOT a
helm template must parse with yaml.safe_load. Helm templates (containing {{ }})
are handled by validate_helm.py -- this script skips them on purpose.
Exit code 0 = all good; 1 = at least one parse failure (details on stdout).
"""
import sys
from pathlib import Path

import yaml

DEPLOY = Path(__file__).resolve().parent.parent

def yaml_files():
    for p in sorted(DEPLOY.rglob("*")):
        if p.suffix in (".yaml", ".yml") and p.is_file():
            if "{{" in p.read_text(encoding="utf-8"):
                continue  # helm template -> validate_helm.py
            yield p

def main() -> int:
    failures = []
    checked = 0
    for path in yaml_files():
        checked += 1
        try:
            docs = list(yaml.safe_load_all(path.read_text(encoding="utf-8")))
            empty = sum(1 for d in docs if d is None)
            print(f"OK   {path.relative_to(DEPLOY)} ({len(docs) - empty} doc(s))")
        except yaml.YAMLError as exc:
            failures.append((path, str(exc).splitlines()[0]))
            print(f"FAIL {path.relative_to(DEPLOY)}: {exc}")
    print(f"\n{checked} file(s) parsed, {len(failures)} failure(s)")
    return 1 if failures else 0

if __name__ == "__main__":
    sys.exit(main())
