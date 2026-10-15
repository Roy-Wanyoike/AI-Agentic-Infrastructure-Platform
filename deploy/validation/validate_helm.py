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
  5. Label/selector consistency: a scoped mini-renderer resolves the chart's
     OWN label helpers (agentos.labels / componentSelectorLabels / componentName
     / podSecurityContext / ...) into concrete label lines, then verifies:
       * Deployment/StatefulSet/DaemonSet spec.selector.matchLabels is a subset
         of the object's own pod template labels (API server rejects otherwise),
       * Service/PDB selectors match some rendered pod's labels,
       * every NetworkPolicy podSelector (top-level + ingress/egress peers)
         references labels/values that some rendered pod actually carries.
     The renderer is keyed to the helpers defined in templates/_helpers.tpl --
     each include is rendered at its call site with the call's own args.
     Anything unresolved degrades to a skipped check (never a false failure).
Exit code 0 = all good.
Honest limitation: this is NOT a substitute for `helm lint` / `helm template`;
run those in CI (see deploy/README.md).
"""
import re
import sys
from pathlib import Path

import yaml

CHART = Path(__file__).resolve().parent.parent / "helm" / "agentos"

# Synthetic release name used when rendering label helpers (check 5).
RELEASE = "myrelease"

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

# ---------------------------------------------------------------------------
# Check 5: label/selector consistency (see module docstring).
#
# The label helpers are DERIVED from templates/_helpers.tpl by a tiny template
# interpreter so the checks stay in sync with the helpers (a stale hardcoded
# renderer would silently miss helper bugs -- that mattered here: the original
# issue-#117 draft emitted a chart-level app.kubernetes.io/name from
# componentSelectorLabels, breaking every StatefulSet selector/NetworkPolicy).
# Only the helpers too hairy for the interpreter (printf-chains, if/else on
# values) fall back to hardcoded equivalents below.
# ---------------------------------------------------------------------------
_C = "agentos"          # Chart.Name
_FULL = f"{RELEASE}-{_C}"

class UnsupportedExpression(Exception):
    """Raised when the mini-interpreter meets a template construct it does not
    know; callers degrade gracefully (skip the check) instead of failing."""

_CTX = object()  # sentinel: the template root context

def _split_ws(s: str):
    """Split on whitespace at paren depth 0, outside double quotes.
    Go template argument lists (list/printf/...) are SPACE-separated:
    'list . "postgres"' is two args -- a comma-only split would keep them
    as one bogus token and drop the component name from componentName."""
    parts, buf, depth, in_q = [], "", 0, False
    for ch in s:
        if in_q:
            buf += ch
            if ch == '"':
                in_q = False
            continue
        if ch == '"':
            in_q = True
            buf += ch
            continue
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        if ch.isspace() and depth == 0:
            if buf:
                parts.append(buf)
                buf = ""
        else:
            buf += ch
    if buf.strip():
        parts.append(buf)
    return parts


def _parse_args(expr):
    """Parse a helper call's argument expression into an args list where _CTX
    marks the root context. Raises UnsupportedExpression on $variables."""
    if not expr:
        return [_CTX]
    inner = expr.strip()
    if inner.startswith("(") and inner.endswith(")"):
        inner = inner[1:-1].strip()
    if inner in (".", "$", ""):
        return [_CTX]
    if re.fullmatch(r"\$\w+", inner):
        raise UnsupportedExpression(inner)
    if inner.startswith("list"):
        out = []
        for a in _split_ws(inner[len("list"):]):
            if a in (".", "$", ""):
                out.append(_CTX)
            elif re.fullmatch(r"\$\w+", a):
                raise UnsupportedExpression(a)
            elif a.startswith('"') and a.endswith('"'):
                out.append(a[1:-1])
            else:
                out.append(a)
        return out
    if inner.startswith("index"):
        # (index . 0) -- the root context passed through
        return [_CTX]
    return [_CTX]

def _release(key):
    return {"Name": RELEASE, "Namespace": "agentos-ns", "Service": "Helm"}.get(key)

def _chartval(key):
    return {"Name": _C, "Version": "0.1.0", "AppVersion": "0.1.0"}.get(key)

def _eval_head(h, args, defs, depth):
    if depth > 8:
        raise UnsupportedExpression("recursion too deep")
    h = h.strip()
    if h in (".", "$", ""):
        return args
    m = re.fullmatch(r'include\s+"agentos\.(\w+)"\s*(\(.*\))?(?:\s+(\.|\$))?\s*', h, flags=re.S)
    if m:
        argexpr = m.group(2)
        if argexpr is None or argexpr.strip() in (".", "$"):
            inner_args = args  # context pass-through: keep the caller's args
        else:
            inner_args = _parse_args(argexpr)
        return _render_helper(m.group(1), inner_args, defs, depth + 1)
    m = re.fullmatch(r"\(index\s+\.\s+0\)\.Release\.(\w+)", h)
    if m:
        v = _release(m.group(1))
        if v is None:
            raise UnsupportedExpression(h)
        return v
    m = re.fullmatch(r"\.Release\.(\w+)", h)
    if m:
        v = _release(m.group(1))
        if v is None:
            raise UnsupportedExpression(h)
        return v
    m = re.fullmatch(r"\.Chart\.(\w+)", h)
    if m:
        v = _chartval(m.group(1))
        if v is None:
            raise UnsupportedExpression(h)
        return v
    m = re.fullmatch(r"index\s+\.\s+(\d+)", h)
    if m:
        i = int(m.group(1))
        if i >= len(args):
            raise UnsupportedExpression(h)
        return args[i]
    m = re.fullmatch(r"default\s+\.Chart\.Name\s+\.Values\.nameOverride.*", h)
    if m:
        return _C  # nameOverride unset in the synthetic render
    m = re.fullmatch(r'printf\s+"((?:[^"\\]|\\.)*)"\s*(.*)', h, flags=re.S)
    if m:
        fmt, rest = m.group(1), m.group(2)
        vals = [_eval_head(a, args, defs, depth + 1) for a in _split_ws(rest) if a]
        try:
            return fmt % tuple(str(v) for v in vals)
        except TypeError:
            raise UnsupportedExpression(h)
    raise UnsupportedExpression(h)

def _load_helper_defs():
    """Extract {{ define "agentos.X" }} ... {{ end }} bodies from _helpers.tpl,
    honouring nested if/range/with ends."""
    path = CHART / "templates" / "_helpers.tpl"
    text = path.read_text(encoding="utf-8")
    defs = {}
    for m in re.finditer(r'\{\{-?\s*define\s+"agentos\.(\w+)"\s*-?\}\}', text):
        name, start = m.group(1), m.end()
        # depth starts at 1: the define's own {{ end }} closes this block.
        # Starting at 0 made every define body swallow the next helper's end.
        depth = 1
        end_at = len(text)
        for tok in re.finditer(r"\{\{-?\s*(if|range|with|block|define|end)\b", text[start:]):
            if tok.group(1) == "end":
                depth -= 1
                if depth == 0:
                    close = text.find("}}", start + tok.end())
                    end_at = (close + 2) if close != -1 else len(text)
                    break
            else:
                depth += 1
        defs[name] = text[start:end_at]
    return defs

def _strip_control_flow(body: str) -> str:
    body = re.sub(r"\{\{-?\s*/\*.*?\*/\s*-?\}\}", "", body, flags=re.S)
    out = []
    for line in body.splitlines():
        s = line.strip()
        if s and re.fullmatch(
            r"(?:\{\{-?\s*(?:if|else|end|range|with|block)\b[^{}]*\}\}\s*)+", s
        ):
            continue
        out.append(line)
    return "\n".join(out)

# Helpers whose logic the interpreter does not model; hardcoded equivalents.
# These are name-mangling helpers only -- NOT the label bodies the selector
# checks depend on (those are derived from _helpers.tpl below).
def _component(args):
    comp = args[1] if len(args) > 1 and isinstance(args[1], str) else ""
    return f"{_FULL}-{comp}"

_HARDCODED = {
    "name": lambda args: _C,
    "fullname": lambda args: _FULL,
    "chart": lambda args: f"{_C}-0.1.0",
    "componentName": _component,
    "serviceAccountName": _component,  # serviceAccount.create=true branch
    "secretName": lambda args: _component(["", "secrets"]),
    "configName": lambda args: _component(["", "config"]),
    "otelConfigName": lambda args: _component(["", "otel-config"]),
    "image": lambda args: "PLACEHOLDER",
}

def _render_helper(name, args, defs, depth=0):
    if name in _HARDCODED:
        return _HARDCODED[name](args)
    if name not in defs:
        raise UnsupportedExpression(name)
    body = _strip_control_flow(defs[name])

    def repl(m):
        parts = [p.strip() for p in m.group(1).split("|") if p.strip()]
        val = _eval_head(parts[0], args, defs, depth)
        for p in parts[1:]:
            nm = re.fullmatch(r"nindent\s+(\d+)", p)
            if nm:
                n = int(nm.group(1))
                val = "\n" + "\n".join(" " * n + ln for ln in str(val).splitlines())
            elif re.fullmatch(r"(quote|squote|toYaml|sha256sum|trunc\s+\d+|trimSuffix.*|replace\s+.*)", p):
                pass  # cosmetic for label derivation purposes
            else:
                raise UnsupportedExpression(p)
        return str(val)

    return re.sub(r"\{\{-?\s*(.*?)\s*-?\}\}", repl, body, flags=re.S)

_HELPER_DEFS = _load_helper_defs()

_INCLUDE_RE = re.compile(
    # trailing bare "."/"$" context is common: include "agentos.labels" . | nindent 4
    r"\{\{-?\s*include\s+\"agentos\.([A-Za-z]+)\"\s*(\([^)]*\))?\s*(?:\.|\$)?\s*"
    r"(?:\|\s*nindent\s+(\d+)\s*)?-?\}\}"
)

def render_label_helpers(text: str) -> str:
    """Resolve the chart's own helper includes into concrete YAML, rendering each
helper with the args from ITS call site (so componentName/otelEnv get the right
component). Unresolved expressions ($vars from range loops, unknown helpers)
are left in place and later replaced by the generic PLACEHOLDER stripper =>
those checks skip, never false-fail."""
    text = re.sub(r"\{\{-?\s*/\*.*?\*/\s*-?\}\}", "", text, flags=re.S)

    def _repl(m):
        helper, argexpr, nindent = m.group(1), m.group(2), m.group(3)
        try:
            args = _parse_args(argexpr)
            body = str(_render_helper(helper, args, _HELPER_DEFS, depth=1))
        except UnsupportedExpression:
            # e.g. (list $ $component) from serviceaccount.yaml's range loop
            return m.group(0)
        if nindent:
            n = int(nindent)
            return "\n" + "\n".join(" " * n + ln for ln in body.splitlines())
        return body  # inline scalar

    text = _INCLUDE_RE.sub(_repl, text)

    # Resolve the .Release/.Chart inline expressions used in selectors/labels so
    # both sides of a selector comparison render identically.
    text = re.sub(
        r"\{\{-?\s*\.Release\.(\w+)\s*-?\}\}",
        lambda m: _release(m.group(1)) or "PLACEHOLDER",
        text,
    )
    text = re.sub(
        r"\{\{-?\s*\.Chart\.(\w+)\s*-?\}\}",
        lambda m: _chartval(m.group(1)) or "PLACEHOLDER",
        text,
    )
    return text

def parse_docs(rendered: str):
    out = []
    for line in rendered.splitlines():
        stripped = line.strip()
        if stripped and re.fullmatch(r"(?:\{\{.*?\}\}\s*)+", stripped):
            continue
        out.append(re.sub(r"\{\{.*?\}\}", "PLACEHOLDER", line))
    return [d for d in yaml.load_all("\n".join(out), Loader=OverridingLoader) if isinstance(d, dict)]

def _obj_name(d, idx):
    name = (d.get("metadata") or {}).get("name")
    if not name or "PLACEHOLDER" in str(name):
        name = f"doc#{idx}"
    return f"{d.get('kind')}/{name}"

def _pod_label_sets(docs):
    sets = []
    for d in docs:
        kind = d.get("kind")
        spec = d.get("spec")
        if kind not in ("Deployment", "StatefulSet", "DaemonSet", "Job") or not isinstance(spec, dict):
            continue
        tmpl = spec.get("template")
        labels = ((tmpl or {}).get("metadata") or {}).get("labels") if isinstance(tmpl, dict) else None
        if isinstance(labels, dict) and labels:
            sets.append(labels)
    return sets

def check_label_selectors(tagged_docs) -> list:
    """tagged_docs: list of (src_rel_path, doc). Selectors are checked against
    the pod templates of the WHOLE chart (pods live in other files than the
    policies/services that select them)."""
    errs = []
    docs = [(src, d) for src, d in tagged_docs if isinstance(d, dict)]
    pods = _pod_label_sets([d for _, d in docs])

    def label_in_some_pod(k, v):
        return any(p.get(k) == v for p in pods)

    for i, (src, d) in enumerate(docs):
        kind, spec = d.get("kind"), d.get("spec")
        if not isinstance(spec, dict):
            continue
        name = f"{src}: {_obj_name(d, i)}"

        if kind in ("Deployment", "StatefulSet", "DaemonSet"):
            tmpl = spec.get("template") or {}
            tlabels = ((tmpl.get("metadata") or {}).get("labels")) if isinstance(tmpl, dict) else None
            sel = spec.get("selector") or {}
            mlabels = sel.get("matchLabels") if isinstance(sel, dict) else None
            if isinstance(mlabels, dict) and isinstance(tlabels, dict) and tlabels:
                for k, v in mlabels.items():
                    if tlabels.get(k) != v:
                        errs.append(
                            f"{name}: spec.selector.matchLabels[{k}]={v!r} is not the pod template's label "
                            f"(got {tlabels.get(k)!r}) -- API server rejects selector/template mismatch"
                        )

        if kind in ("Service",):
            sel = spec.get("selector")
            if isinstance(sel, dict) and sel and pods:
                if not all(any(p.get(k) == v for p in pods) for k, v in sel.items()):
                    errs.append(f"{name}: spec.selector {sel} matches no rendered pod template labels")

        if kind == "PodDisruptionBudget":
            sel = spec.get("selector") or {}
            mlabels = sel.get("matchLabels") if isinstance(sel, dict) else None
            if isinstance(mlabels, dict) and mlabels and pods:
                if not all(any(p.get(k) == v for p in pods) for k, v in mlabels.items()):
                    errs.append(f"{name}: PDB selector {mlabels} matches no rendered pod template labels")

        if kind == "NetworkPolicy":
            # top-level podSelector + any nested podSelector peers (ingress/egress)
            stack = [spec]
            while stack:
                node = stack.pop()
                if not isinstance(node, dict):
                    continue
                for k, v in node.items():
                    if k == "podSelector" and isinstance(v, dict):
                        mlabels = v.get("matchLabels") or {}
                        for mk, mv in mlabels.items():
                            if not label_in_some_pod(mk, mv):
                                errs.append(f"{name}: NetworkPolicy podSelector {mk}={mv!r} matches no rendered pod")
                        for expr in v.get("matchExpressions") or []:
                            key, op, vals = expr.get("key"), expr.get("operator"), expr.get("values") or []
                            if op == "In" and pods:
                                for mv in vals:
                                    if not label_in_some_pod(key, mv):
                                        errs.append(
                                            f"{name}: NetworkPolicy podSelector matchExpressions {key} In {mv!r} "
                                            "matches no rendered pod"
                                        )
                    stack.append(v)
    return errs

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
    all_docs = []  # (rel_path, doc) for the label/selector pass
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
            # 5. collect helper-resolved docs (selector checks run after ALL
            # files are parsed -- pods and their selectors live in different files)
            try:
                docs5 = parse_docs(render_label_helpers(text))
                all_docs.extend((str(rel), d) for d in docs5)
            except yaml.YAMLError as exc:
                failures.append(f"{rel}: label-rendered YAML failed: {str(exc).splitlines()[0]}")
        all_refs |= collect_values_refs(text)

    n_docs = len(all_docs)
    for e in check_label_selectors(all_docs):
        failures.append(e)

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

    # 5b. summary of the selector consistency pass
    sel_failures = [f for f in failures if "selector" in f or "podSelector" in f]
    print(f"Label/selector consistency: {n_docs} doc(s) checked, {len(sel_failures)} selector problem(s)")

    if failures:
        print("\nFAILURES:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("\nHelm chart validation PASSED (structural; `helm lint`/`helm template` pending CI)")
    return 0

if __name__ == "__main__":
    sys.exit(main())
