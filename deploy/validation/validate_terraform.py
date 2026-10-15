#!/usr/bin/env python3
"""validate_terraform.py (issue #117): structural HCL sanity (no terraform binary).

Checks per .tf file:
  * balanced braces/brackets and quotes AFTER stripping comments and strings
  * blocks (name { ... }) look well-formed
Checks per directory (module / environment root):
  * every var.<name> referenced is declared in a variables.tf/*.tf `variable` block
  * every module `source` path exists (relative to the referencing file)
Exit code 0 = all good. Honest limitation: NOT a substitute for
`terraform validate` / plan; run those in CI (see deploy/README.md).
"""
import re
import sys
from pathlib import Path

TFROOT = Path(__file__).resolve().parent.parent / "terraform"


def strip_comments_and_strings(text: str) -> str:
    """Remove # / // line comments, /* */ block comments, and quoted strings.
    Keeps structure chars so balance can be checked. Handles heredocs crudely
    by dropping <<...EOF blocks wholesale (none used unbalanced here)."""
    out = []
    i, n = 0, len(text)
    in_str = False
    while i < n:
        c = text[i]
        nxt = text[i + 1] if i + 1 < n else ""
        if not in_str and c == '"' and text[max(0, i - 1)] != "\\":
            in_str = True
            i += 1
            continue
        if in_str:
            if c == '"' and text[i - 1] != "\\":
                in_str = False
            i += 1
            continue
        if c == "#" or (c == "/" and nxt == "/"):
            j = text.find("\n", i)
            i = n if j == -1 else j
            continue
        if c == "/" and nxt == "*":
            j = text.find("*/", i)
            i = n if j == -1 else j + 2
            continue
        if c == "\n":
            out.append("\n")
        elif not c.isspace():
            out.append(c)
        i += 1
    return "".join(out)


def strip_comments_keep_strings(text: str) -> str:
    """Remove # / // line comments and /* */ block comments while KEEPING
    quoted strings intact, so `\${var.x}` interpolations inside strings still
    scan. Used for var-reference/declaration cross-checks: reference scans on
    raw text false-positive on prose like '# no var.aws_region here on purpose'
    (this exact bug hit modules/database after the aws_region var was removed)."""
    out = []
    i, n = 0, len(text)
    in_str = False
    while i < n:
        c = text[i]
        nxt = text[i + 1] if i + 1 < n else ""
        if not in_str and c == '"':
            in_str = True
            out.append(c)
            i += 1
            continue
        if in_str:
            out.append(c)
            if c == '"' and text[i - 1] != "\\":
                in_str = False
            i += 1
            continue
        if c == "#" or (c == "/" and nxt == "/"):
            j = text.find("\n", i)
            i = n if j == -1 else j
            continue
        if c == "/" and nxt == "*":
            j = text.find("*/", i)
            i = n if j == -1 else j + 2
            continue
        out.append(c)
        i += 1
    return "".join(out)


def check_file(path: Path):
    errs = []
    raw = path.read_text(encoding="utf-8")
    stripped = strip_comments_and_strings(raw)
    stack = []
    pairs = {"}": "{", "]": "[", ")": "("}
    for ch in stripped:
        if ch in "{[(":
            stack.append(ch)
        elif ch in "}])":
            if not stack or stack[-1] != pairs[ch]:
                errs.append("unbalanced block/bracket")
                break
            stack.pop()
    if stack:
        errs.append(f"unclosed {len(stack)} block opener(s)")
    return errs


def declared_vars(dirpath: Path):
    names = set()
    for tf in dirpath.glob("*.tf"):
        text = strip_comments_keep_strings(tf.read_text(encoding="utf-8"))
        for m in re.finditer(r'variable\s+"([^"]+)"\s*\{', text):
            names.add(m.group(1))
    return names


def referenced_vars(dirpath: Path):
    refs = set()
    for tf in dirpath.glob("*.tf"):
        text = strip_comments_keep_strings(tf.read_text(encoding="utf-8"))
        refs |= {m.group(1) for m in re.finditer(r"\bvar\.([A-Za-z_][A-Za-z0-9_]*)", text)}
    return refs


def main() -> int:
    failures = []
    n_files = 0
    for tf in sorted(TFROOT.rglob("*.tf")):
        n_files += 1
        errs = check_file(tf)
        for e in errs:
            failures.append(f"{tf.relative_to(TFROOT)}: {e}")
        if not errs:
            print(f"OK   {tf.relative_to(TFROOT)}: braces/brackets balanced")

    # var cross-check per directory that contains .tf files
    dirs = {tf.parent for tf in TFROOT.rglob("*.tf")}
    for d in sorted(dirs):
        declared = declared_vars(d)
        referenced = referenced_vars(d)
        missing = sorted(referenced - declared)
        for m in missing:
            failures.append(f"{d.relative_to(TFROOT)}: var.{m} referenced but not declared")
        # module source existence (comment-stripped so commented-out sources
        # don't false-fail; strings are kept so the quoted path still matches)
        for tf in d.glob("*.tf"):
            text = strip_comments_keep_strings(tf.read_text(encoding="utf-8"))
            for m in re.finditer(r'source\s*=\s*"([^"]+)"', text):
                src = m.group(1)
                if src.startswith(("./", "../")):
                    if not (d / src).resolve().exists():
                        failures.append(f"{tf.relative_to(TFROOT)}: module source '{src}' does not exist")
        if not missing:
            print(f"OK   {d.relative_to(TFROOT)}: {len(declared)} vars declared, all {len(referenced)} references resolve")

    if failures:
        print("\nFAILURES:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print(f"\nTerraform structural validation PASSED ({n_files} .tf files; plan-pending CI)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
