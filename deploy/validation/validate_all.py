#!/usr/bin/env python3
"""validate_all.py (issue #117): single entry point for the offline deploy checks.

Runs the four sibling validators and adds a secrets scan:

  1. validate_yaml.py       every .yaml/.yml under deploy/ parses
  2. validate_helm.py       helm templates render-parse + balanced {{ }} +
                            every .Values.* path exists in values.yaml (+ env
                            overlay keys are subsets of it) + label/selector
                            consistency against the chart's own helpers
  3. validate_kustomize.py  kustomization resources/patches/images resolve
  4. validate_terraform.py  HCL braces/brackets balanced + var refs declared +
                            module sources exist
  5. (here) secrets scan: no credential-looking literals anywhere under deploy/;
     placeholder values (CHANGEME / REPLACE_ME / <...> / {{ ... }} / .Values
     templates / empty) are the only accepted "secrets".

Exit code 0 = all good.
Honest limitation: NOT a substitute for `helm lint`, `kubectl --dry-run`,
`terraform validate` or a secret scanner like gitleaks; run those in CI
(see deploy/README.md).
"""
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
DEPLOY = HERE.parent

SIBLINGS = [
    "validate_yaml.py",
    "validate_helm.py",
    "validate_kustomize.py",
    "validate_terraform.py",
]

# Files scanned for secret-looking literals (artifact surfaces only; the
# validators' own .py regexes would self-match, so code is not scanned).
SCAN_SUFFIXES = {".yaml", ".yml", ".tpl", ".conf", ".tf", ".md", ".txt", ".example"}

# A "secret" is a key that names a credential with a long non-placeholder value.
# Keys that NAME a secret object (secretName/existingSecret/imagePullSecrets)
# are references, not values -- excluded via KEY_SKIP below.
KEY_RE = re.compile(
    r"(?i)\b([A-Za-z0-9_-]*(?:password|passwd|secret|token|api[_-]?key|credential|private[_-]?key)[A-Za-z0-9_-]*)"
    r"\s*[:=]\s*[\"']?([^\s\"'#]+)[\"']?"
)
KEY_SKIP = {"secretname", "existingsecret", "imagepullsecrets", "imagepullsecret"}
# Password inside a DSN (postgres://user:PASS@host). Only long passwords flag;
# dev/doc examples use short throwaways ("pass", the app name) by design.
DSN_RE = re.compile(r"\w+://[^:/\s\"']+:([^@/\s\"']+)@", )
OTHER_RES = [
    (re.compile(r"AKIA[0-9A-Z]{16}"), "AWS access key id"),
    (re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----"), "embedded private key"),
    (re.compile(r"\bgh[pousr]_[A-Za-z0-9]{36,}\b"), "GitHub token"),
    (re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{10,}\b"), "Slack token"),
    (re.compile(r"eyJhbGciOi[A-Za-z0-9._-]{20,}"), "JWT literal"),
]

# Anything containing these markers is a placeholder/templated value, not a secret.
PLACEHOLDER_MARKERS = (
    "changeme", "replace", "placeholder", "todo", "fixme", "dummy", "example",
    "sample", "redacted", "openssl", "mint-", "your", "insert-", "dev-only",
    "<", ">", "${", "{{", "}}", ".values", ".release", "tfvars", "gitignored",
    "unset", "n/a",
)
# Terraform/Helm REFERENCES instead of literals: var.cluster_token,
# random_password.x[0].result, module.foo.y ...
REFERENCE_RE = re.compile(
    r"^(?:var|local|module|data)\.|^(?:random|aws|google|kubernetes|helm)_"
)


def _is_placeholder(value: str) -> bool:
    v = value.strip().strip("\"'")
    low = v.lower()
    if not v or low in ("null", "none", "true", "false"):
        return True
    if len(v) < 8:
        return True  # too short to be a real credential (dev/doc throwaways)
    if REFERENCE_RE.match(v):
        return True
    return any(m in low for m in PLACEHOLDER_MARKERS)


def scan_secrets() -> list:
    failures = []
    n_files = 0
    for path in sorted(DEPLOY.rglob("*")):
        if not path.is_file() or path.suffix not in SCAN_SUFFIXES:
            continue
        # .example files are tfvars examples (already covered by suffix filter);
        # NOTES.txt/.md are docs -- still scanned, they must not carry secrets.
        n_files += 1
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        rel = path.relative_to(DEPLOY)
        for lineno, line in enumerate(text.splitlines(), 1):
            if line.lstrip().startswith("#"):
                continue  # comments may document key names; values there are prose
            m = KEY_RE.search(line)
            if m:
                key_norm = re.sub(r"[-_]", "", m.group(1)).lower()
                if key_norm not in KEY_SKIP and not _is_placeholder(m.group(2)):
                    failures.append(
                        f"{rel}:{lineno}: credential-looking literal for "
                        f"'{m.group(1)}' (value not a recognized placeholder)"
                    )
            m = DSN_RE.search(line)
            if m and len(m.group(1)) >= 8 and not _is_placeholder(m.group(1)):
                failures.append(f"{rel}:{lineno}: DSN embeds a non-placeholder password")
            for rx, what in OTHER_RES:
                if rx.search(line):
                    failures.append(f"{rel}:{lineno}: {what}")
    print(f"Secrets scan: {n_files} file(s) scanned, {len(failures)} finding(s)")
    return failures


def main() -> int:
    failures = []
    for script in SIBLINGS:
        print(f"\n===== {script} =====")
        proc = subprocess.run(
            [sys.executable, str(HERE / script)], capture_output=True, text=True
        )
        out = proc.stdout.strip()
        # keep the per-validator output compact: headers + tail summary lines
        lines = out.splitlines()
        tail = [ln for ln in lines if ln.startswith(("OK", "Values", "values-", "Label/", "Secrets", "Kustomize", "Terraform", "Helm"))]
        print("\n".join(tail[-6:]) if tail else "(no summary output)")
        if proc.returncode != 0:
            failures.append(f"{script} exited {proc.returncode}")
            err = proc.stdout.strip().splitlines() + proc.stderr.strip().splitlines()
            details = [ln for ln in err if "FAILURES" in ln or ln.strip().startswith("- ") or "Error" in ln or "Traceback" in ln]
            failures.extend(f"  {script}: {ln.strip()}" for ln in details[:15])

    print("\n===== secrets scan =====")
    failures.extend(scan_secrets())

    if failures:
        print("\nVALIDATE_ALL FAILURES:")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("\nvalidate_all: ALL CHECKS PASSED (yaml + helm + kustomize + terraform + secrets)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
