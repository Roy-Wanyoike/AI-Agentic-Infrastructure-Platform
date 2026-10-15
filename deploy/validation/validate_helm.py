#!/usr/bin/env python3
"""validate_helm.py (issue #117): offline Helm chart checks.

No `helm` binary in this workspace, so this script does what it honestly can:
  1. Balanced template braces in every templates/ file + NOTES.txt.
  2. Strip {{ ... }} actions into YAML placeholders and parse each resulting
     document (duplicate mapping keys produced by stripped if/else branches are
     tolerated via an overriding loader -- real rendering only emits one).
  3. Cross-check: every `.Values.foo.bar` referenced in templates must exist in
     values.yaml (nested path check), and every key in values-dev/prod.yaml
     must exist in values.yaml (catches typos in env overlays).
  4. Chart.yaml sanity: apiVersion v2, version + appVersion present.
Exit code 0 = all good.
Honest limitation: this is NOT a substitute for `helm lint` / `helm template`;
run those in CI (see deploy/README.md).
"""
import re
import sys
from pathlib import Path

import yaml

CHART = Path(__file__).resolve().parent.parent / "helm" / "agentos"

class OverridingLoader(yaml.SafeLoader):
    """SafeLoader that tolerates duplicate mapping keys (last wins)."""

def _construct_mapping(loader, node, deep=False):
    mapping = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        mapping[key] = loader.construct_object(value_node, deep=deep)
    return mapping

OverridingLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _construct_mapping
)

def strip_template(text: str) -> str:
    """Make a template parseable as YAML:
    * multi-line template comments {{- /* ... */ -}} are removed first;
    * lines that contain ONLY template action(s) are dropped entirely
      (if/else/end/range/include blocks) -- both if/else branches then appear,
      which the OverridingLoader tolerates;
    * inline actions become a PLACEHOLDER scalar."""
    text = re.sub(r"\{\{-?\s*/\*.*?\*/\s*-?\}\}", "", text, flags=re.S)
    out = []
    for line in text.splitlines():
        stripped = line.strip()
        if stripped and re.fullmatch(r"(?:\{\{.*?\}\}\s*)+", stripped):
            continue  # action-only line
        out.append(re.sub(r"\{\{.*?\}\}", "PLACEHOLDER", line))
    return "\n".join(out) + "\n"

def check_braces(path: Path) -> list:
    errs = []
    text = path.read_text(encoding="utf-8")
    opens, closes = text.count("{{"), text.count("}}")
    if opens != closes:
        errs.append(f"unbalanced braces: {opens} '{{{{' vs {closes} '}}}}'")
    return errs

def collect_values_refs(text: str):
    return {m.group(1).rstrip(".") for m in re.finditer(r"\.Values\.([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*)", text)}

def path_exists(tree, dotted: str) -> bool:
    node = tree
    for part in dotted.split("."):
        if isinstance(node, dict) and part in node:
            node = node[part]
        else:
            return False
    return True

def main() -> int:
    failures = []
    values = yaml.safe_load((CHART / "values.yaml").read_text(encoding="utf-8"))
    chart_meta = yaml.safe_load((CHART / "Chart.yaml").read_text(encoding="utf-8"))

    # 4. Chart metadata sanity
    if chart_meta.get("apiVersion") != "v2":
        failures.append("Chart.yaml: apiVersion must be v2")
    for key in ("version", "appVersion"):
        if not chart_meta.get(key):
            failures.append(f"Chart.yaml: missing {key}")
    print(f"Chart.yaml: apiVersion={chart_meta.get('apiVersion')} version={chart_meta.get('version')} appVersion={chart_meta.get('appVersion')}")

    template_dir = CHART / "templates"
    all_refs = set()
    tmpl_files = sorted(template_dir.glob("*")) + [CHART / "NOTES.txt"]
    for path in tmpl_files:
        if not path.is_file():
            continue
        text = path.read_text(encoding="utf-8")
        rel = path.relative_to(CHART)
        # 1. braces
        for e in check_braces(path):
            failures.append(f"{rel}: {e}")
        # 2. parse stripped YAML (skip .tpl helpers / NOTES: not standalone YAML)
        if path.suffix == ".yaml":
            try:
                stripped = strip_template(text)
                docs = [d for d in yaml.load_all(stripped, Loader=OverridingLoader) if d]
                kinds = [d.get("kind", "?") for d in docs if isinstance(d, dict)]
                print(f"OK   {rel}: renders {len(docs)} doc(s) {kinds}")
            except yaml.YAMLError as exc:
                failures.append(f"{rel}: YAML after strip failed: {str(exc).splitlines()[0]}")
        all_refs |= collect_values_refs(text)

    # 3. values cross-check (templates -> values.yaml)
    missing = sorted(r for r in all_refs if not path_exists(values, r))
    for m in missing:
        failures.append(f".Values.{m} referenced in templates but missing from values.yaml")
    print(f"Values cross-check: {len(all_refs)} distinct .Values.* paths referenced, {len(missing)} missing")

    # 3b. env overlay values -> values.yaml (catches typos)
    for env_file in ("values-dev.yaml", "values-prod.yaml"):
        env_path = CHART / env_file
        env = yaml.safe_load(env_path.read_text(encoding="utf-8"))

        def walk(node, prefix=""):
            if isinstance(node, dict):
                for k, v in node.items():
                    dotted = f"{prefix}.{k}" if prefix else k
                    if not path_exists(values, dotted):
                        failures.append(f"{env_file}: key {dotted} not present in values.yaml (typo?)")
                    walk(v, dotted)

        walk(env)
        print(f"{env_file}: overlay keys checked against values.yaml")

    if failures:
        print("\nFAILURES:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("\nHelm chart validation PASSED (structural; `helm lint`/`helm template` pending CI)")
    return 0

if __name__ == "__main__":
    sys.exit(main())
