import re, sys, yaml

path = sys.argv[1]
raw = open(path).read()
doc = yaml.safe_load(raw)

# every local $ref must resolve inside the fragment
missing = []
def walk(node):
    if isinstance(node, dict):
        for k, v in node.items():
            if k == "$ref" and isinstance(v, str) and v.startswith("#/"):
                cur = doc
                for part in v[2:].split("/"):
                    part = part.replace("~1", "/").replace("~0", "~")
                    if isinstance(cur, dict) and part in cur:
                        cur = cur[part]
                    else:
                        missing.append(v)
                        return
            else:
                walk(v)
    elif isinstance(node, list):
        for item in node:
            walk(item)
walk(doc)

# security scheme names used by `security:` must exist in components.securitySchemes
schemes = set((doc.get("components", {}) or {}).get("securitySchemes", {}) or {})
def walk_sec(node):
    if isinstance(node, dict):
        if "security" in node and isinstance(node["security"], list):
            for req in node["security"]:
                if isinstance(req, dict):
                    for name in req:
                        if name not in schemes:
                            missing.append(f"securityScheme:{name}")
        for v in node.values():
            walk_sec(v)
    elif isinstance(node, list):
        for item in node:
            walk_sec(item)
walk_sec(doc)

print("\n".join(missing) if missing else f"ALL_REFS_RESOLVE ({path})")
sys.exit(1 if missing else 0)
