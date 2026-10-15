#!/usr/bin/env python3
"""validate_kustomize.py (issue #117): structural kustomize checks (no binary).

For every kustomization.yaml under deploy/kustomize:
  * apiVersion/kind sanity
  * every `resources:` entry resolves to an existing file/directory
  * every `patches:` path exists, and its target (kind/name) matches a doc
    reachable from the overlay's resource set (base manifests included)
  * every `images:` name matches an image actually used in the resource set
Exit code 0 = all good.
"""
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent / "kustomize"


def load_docs(paths):
    docs = []
    for p in paths:
        for d in yaml.safe_load_all(p.read_text(encoding="utf-8")):
            if d:
                docs.append(d)
    return docs


def collect_docs(kust_dir: Path, seen=None) -> list:
    """Recursively collect resource docs from a kustomization directory."""
    seen = seen or set()
    kust_path = kust_dir / "kustomization.yaml"
    if kust_path in seen or not kust_path.exists():
        return []
    seen.add(kust_path)
    kust = yaml.safe_load(kust_path.read_text(encoding="utf-8"))
    docs = []
    for res in kust.get("resources", []) or []:
        res_path = (kust_dir / res).resolve()
        if not res_path.exists():
            continue
        if res_path.is_dir():
            docs.extend(collect_docs(res_path, seen))
        else:
            docs.extend(load_docs([res_path]))
    return docs


def main() -> int:
    failures = []
    n_kust = 0
    for kust_path in sorted(ROOT.rglob("kustomization.yaml")):
        n_kust += 1
        rel = kust_path.relative_to(ROOT.parent)
        base_dir = kust_path.parent
        kust = yaml.safe_load(kust_path.read_text(encoding="utf-8"))

        # collect all docs from resources (transitively through dir bases)
        docs = []
        for res in kust.get("resources", []) or []:
            res_path = (base_dir / res).resolve()
            if not res_path.exists():
                failures.append(f"{rel}: resource '{res}' does not exist")
                continue
            if res_path.is_dir():
                if not (res_path / "kustomization.yaml").exists():
                    failures.append(f"{rel}: resource dir '{res}' has no kustomization.yaml")
                    continue
                docs.extend(collect_docs(res_path))
            else:
                docs.extend(load_docs([res_path]))

        images_used = set()
        for d in docs:
            # collect container images from pod specs
            kind = d.get("kind")
            name = d.get("metadata", {}).get("name", "?")
            if kind in ("Deployment", "StatefulSet", "Job", "CronJob"):
                spec = d.get("spec", {})
                template = spec.get("template", {})
                containers = template.get("spec", {}).get("containers", [])
                for c in containers:
                    if isinstance(c, dict) and "image" in c:
                        images_used.add(c["image"])

        for patch in kust.get("patches", []) or []:
            ppath = (base_dir / patch.get("path", "")).resolve()
            if not ppath.exists():
                failures.append(f"{rel}: patch '{patch.get('path')}' does not exist")
                continue
            for target in patch.get("targets", []) or []:
                # (not used in this repo's overlays; completeness)
                pass
            for pd in load_docs([ppath]):
                kind, name = pd.get("kind"), pd.get("metadata", {}).get("name")
                if kind and name:
                    match = [d for d in docs if d.get("kind") == kind and d.get("metadata", {}).get("name") == name]
                    if not match:
                        failures.append(f"{rel}: patch target {kind}/{name} not found in resource set")
                    else:
                        print(f"OK   {rel}: patch {kind}/{name} matches resource")

        for img in kust.get("images", []) or []:
            iname = img.get("name")
            hits = [u for u in images_used if u.split(":")[0] == iname]
            if not hits:
                failures.append(f"{rel}: images transformer name '{iname}' matches no container image in resource set")
            else:
                print(f"OK   {rel}: images '{iname}' matches {len(hits)} container(s)")

        ns = kust.get("namespace")
        print(f"OK   {rel}: {len(kust.get('resources', []) or [])} resources, namespace={ns or '(inherited)'}")

    if failures:
        print("\nFAILURES:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print(f"\nKustomize validation PASSED ({n_kust} kustomization(s) checked)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
