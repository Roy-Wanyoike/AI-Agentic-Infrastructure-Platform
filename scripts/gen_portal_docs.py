#!/usr/bin/env python3
"""Generate docs/portal/api-data.js from api/openapi.yaml.

Reads the AgentOS OpenAPI 3.1 contract and emits a self-contained JS data
file consumed by docs/portal/index.html (the developer portal). The script
uses ONLY the Python standard library — no PyYAML — via a small YAML-subset
tokenizer/parser scoped to the constructs this spec actually uses:

  * block mappings / sequences (including sequences at the parent key's
    indent, which OpenAPI specs emit constantly)
  * flow mappings / sequences on a single line ({ $ref: "..." },
    { type: string, enum: [a, b] })
  * multi-line quoted scalars (folded to a single line) and block scalars
    (| and >, with chomping indicators)
  * quoted / plain scalars, null, booleans, integers
  * full-line comments and blank lines

Output: window.AGENTOS_API = { meta, tags, groups, schemas } where groups
are operations grouped by their FIRST tag and schemas is a compact index of
components.schemas used by the reference browser and playground defaults.

Usage:  python3 scripts/gen_portal_docs.py
        (paths default relative to the repo root; --spec/--out to override)

The output is deterministic except for the "generated" ISO date (UTC day),
which keeps diffs small between runs.
"""

from __future__ import annotations

import argparse
import datetime as _dt
import json
import re
import sys
from pathlib import Path

HTTP_METHODS = ("get", "post", "put", "patch", "delete", "head", "options", "trace")

# ---------------------------------------------------------------------------
# Minimal YAML-subset tokenizer / parser
# ---------------------------------------------------------------------------


class YamlError(Exception):
    pass


BLOCK_HEADERS = ("|", "|-", "|+", ">", ">-", ">+")


def _strip_comment(text: str) -> str:
    """Remove a trailing comment that is outside quotes.

    YAML treats ' #' (whitespace + hash) as a comment start; a hash glued to
    a plain scalar or inside quotes is literal. We honour that distinction.
    """
    quote = ""
    i = 0
    while i < len(text):
        ch = text[i]
        if quote:
            if ch == "\\" and quote == '"':
                i += 2
                continue
            if ch == quote:
                quote = ""
        elif ch in "'\"":
            quote = ch
        elif ch == "#" and (i == 0 or text[i - 1] in " \t"):
            return text[:i].rstrip()
        i += 1
    return text.rstrip()


def _find_colon(content: str) -> int | None:
    """Index of the key/value colon in a line (outside quotes / brackets)."""
    depth = 0
    quote = ""
    i = 0
    while i < len(content):
        ch = content[i]
        if quote:
            if ch == "\\" and quote == '"':
                i += 2
                continue
            if ch == quote:
                quote = ""
        elif ch in "'\"":
            quote = ch
        elif ch in "[{":
            depth += 1
        elif ch in "]}":
            depth -= 1
        elif ch == ":" and depth == 0:
            nxt = content[i + 1] if i + 1 < len(content) else " "
            if nxt in (" ", "\t", ""):
                return i
        i += 1
    return None


def _value_part(content: str) -> str:
    """The value text of a logical line (after '- ' marker and 'key:')."""
    rest = content
    if rest == "-" or rest.startswith("- "):
        rest = rest[1:].lstrip()
    if rest.startswith(("{", "[")):
        return rest  # flow collection: key/value handled by the flow parser
    idx = _find_colon(rest)
    if idx is None:
        return rest if not rest.startswith("-") else rest
    return rest[idx + 1 :].strip()


def _quoted_prefix_len(text: str) -> int:
    """If text starts with a quoted scalar, its length on this line, else 0.

    Returns -1 when the quoted scalar is unterminated (multi-line).
    """
    quote = text[0]
    i = 1
    while i < len(text):
        ch = text[i]
        if quote == '"' and ch == "\\":
            i += 2
            continue
        if ch == quote:
            if quote == "'" and text[i + 1 : i + 2] == "'":
                i += 2
                continue
            return i + 1
        i += 1
    return -1


def _tokenize(source: str) -> list[tuple[int, str]]:
    """Return [(indent, logical content)] with comments/blanks removed and
    multi-line quoted scalars + plain-scalar continuations folded."""
    raw_lines = source.splitlines()
    tokens: list[tuple[int, str]] = []
    i = 0
    n = len(raw_lines)

    def raw_strip(line: str) -> str:
        return _strip_comment(line.rstrip())

    while i < n:
        raw = raw_lines[i]
        i += 1
        if not raw.strip() or raw.lstrip().startswith("#") or raw.strip() in ("---", "..."):
            continue
        m = re.match(r"( *)(.*)$", raw)
        indent = len(m.group(1))
        content = raw_strip(m.group(2))
        if not content:
            continue

        value = _value_part(content)

        # Block scalar header ('|' / '>'): swallow deeper-indented lines.
        if value in BLOCK_HEADERS:
            key_indent = indent
            while i < n:
                nxt = raw_lines[i]
                if nxt.strip() == "":
                    i += 1
                    continue
                if len(nxt) - len(nxt.lstrip(" \t")) <= key_indent:
                    break
                i += 1
            tokens.append((indent, content))
            continue

        # Multi-line quoted scalar: fold continuation lines into one logical
        # line. Only applies when the value actually *starts* with a quote.
        if value[:1] in ("'", '"'):
            body_start = content.index(value)
            if _quoted_prefix_len(value) == -1:
                merged = content
                while i < n:
                    nxt = raw_lines[i]
                    merged += " " + nxt.strip()
                    i += 1
                    tail = _value_part_of_merged(merged, body_start)
                    if _quoted_prefix_len(tail) != -1:
                        break
                indent, content = indent, merged.rstrip()

        # Plain multi-line scalar: continuation lines are deeper-indented and
        # (per YAML grammar) never contain a top-level ': '.
        if not content.startswith("- ") and _find_colon(content) is not None and not _value_part(content)[:1] in (
            "'",
            '"',
            "{",
            "[",
            "|",
            ">",
        ):
            base_value = _value_part(content)
            if base_value and base_value not in BLOCK_HEADERS:
                while i < n:
                    nxt = raw_lines[i]
                    if nxt.strip() == "":
                        i += 1
                        continue
                    nxt_indent = len(nxt) - len(nxt.lstrip(" \t"))
                    nxt_content = raw_strip(nxt.strip())
                    if nxt_indent <= indent or nxt_content.startswith("- ") or _find_colon(nxt_content) is not None:
                        break
                    content += " " + nxt_content
                    i += 1

        tokens.append((indent, content))
    return tokens


def _value_part_of_merged(merged: str, body_start: int) -> str:
    """Value part of a merged multi-line quoted scalar (for termination check)."""
    # Count quotes from body_start within the whole merged text; simpler: return
    # the substring after the original key, which is where the quote opened.
    return merged[body_start:]


def _split_flow(text: str) -> list[str]:
    """Split 'a: 1, b: [1, 2]' style content on top-level commas."""
    parts: list[str] = []
    depth = 0
    quote = ""
    start = 0
    i = 0
    while i < len(text):
        ch = text[i]
        if quote:
            if ch == "\\" and quote == '"':
                i += 2
                continue
            if ch == quote:
                quote = ""
        elif ch in "'\"":
            quote = ch
        elif ch in "[{":
            depth += 1
        elif ch in "]}":
            depth -= 1
        elif ch == "," and depth == 0:
            parts.append(text[start:i].strip())
            start = i + 1
        i += 1
    tail = text[start:].strip()
    if tail:
        parts.append(tail)
    return parts


def _parse_flow(text: str):
    """Parse a flow collection or scalar contained on a single line."""
    text = text.strip()
    if text.startswith("{") and text.endswith("}"):
        out: dict = {}
        for part in _split_flow(text[1:-1].strip()):
            idx = _find_colon(part)
            if idx is None:
                raise YamlError(f"flow map entry without ':': {part!r}")
            key = _scalar(part[:idx].strip())
            out[key] = _parse_flow(part[idx + 1 :].strip())
        return out
    if text.startswith("[") and text.endswith("]"):
        return [_parse_flow(part) for part in _split_flow(text[1:-1].strip()) if part]
    return _scalar(text)


def _scalar(raw: str):
    """Parse a scalar (null / bool / int / float / quoted or plain string)."""
    if raw == "" or raw == "~" or raw == "null":
        return None
    if len(raw) >= 2 and raw[0] == raw[-1] and raw[0] in "'\"":
        inner = raw[1:-1]
        if raw[0] == '"':
            inner = re.sub(r"\\(.)", r"\1", inner)
        else:
            inner = inner.replace("''", "'")
        return inner
    low = raw.lower()
    if low in ("true", "false"):
        return low == "true"
    try:
        return int(raw)
    except ValueError:
        pass
    try:
        return float(raw)
    except ValueError:
        pass
    return raw


def _is_seq_line(content: str) -> bool:
    return content == "-" or content.startswith("- ")


class _Parser:
    def __init__(self, tokens: list[tuple[int, str]]):
        self.tokens = tokens
        self.i = 0

    def peek(self) -> tuple[int, str] | None:
        return self.tokens[self.i] if self.i < len(self.tokens) else None

    def parse(self, indent: int):
        tok = self.peek()
        if tok is None or tok[0] < indent:
            return None
        if _is_seq_line(tok[1]):
            return self.parse_seq(tok[0])
        return self.parse_map(tok[0])

    def parse_seq(self, seq_indent: int):
        items = []
        while True:
            tok = self.peek()
            if tok is None or tok[0] != seq_indent or not _is_seq_line(tok[1]):
                break
            indent, content = tok
            rest = content[1:].strip() if content != "-" else ""
            if rest == "-" or rest.startswith("- "):
                # Nested sequence ('- - a' / deeper continuation): re-parse the
                # remainder as a sequence at its own virtual indent.
                nested_indent = indent + (len(content) - len(content[1:].lstrip()))
                self.tokens[self.i] = (nested_indent, rest)  # current token
                items.append(self.parse_seq(nested_indent))
                continue
            self.i += 1
            if rest == "":
                nxt = self.peek()
                items.append(self.parse(seq_indent + 1) if nxt and nxt[0] > seq_indent else None)
                continue
            key, sep, value = self._split_key(rest)
            if sep:
                # Virtual column of the key inside '- key: value'.
                virtual_indent = indent + (len(content) - len(content[1:].lstrip()))
                items.append(self._inline_map(key, value, virtual_indent))
            else:
                items.append(self._scalar_or_flow(rest))
        return items

    def _inline_map(self, first_key: str, first_value: str, virtual_indent: int) -> dict:
        """A mapping whose first entry was inline ('- key: value' or deeper)."""
        result: dict = {}
        self._assign(result, first_key, first_value, virtual_indent)
        while True:
            tok = self.peek()
            if tok is None or tok[0] != virtual_indent or _is_seq_line(tok[1]):
                break
            key, sep, value = self._split_key(tok[1])
            if not sep:
                break
            self.i += 1
            self._assign(result, key, value, virtual_indent)
        return result

    def _assign(self, target: dict, key: str, value: str, key_indent: int) -> None:
        if value in BLOCK_HEADERS:
            target[key] = self._block_scalar_text(key_indent)
            return
        nxt = self.peek()
        if value == "":
            if nxt and nxt[0] > key_indent:
                if _is_seq_line(nxt[1]):
                    target[key] = self.parse_seq(nxt[0])
                else:
                    target[key] = self.parse_map(nxt[0])
            elif nxt and nxt[0] == key_indent and _is_seq_line(nxt[1]):
                target[key] = self.parse_seq(key_indent)
            else:
                target[key] = None
            return
        if nxt and nxt[0] == key_indent and _is_seq_line(nxt[1]):
            # Sequence at the key's own indent (OpenAPI style).
            target[key] = self._scalar_or_flow(value)
            # Re-parse: the value actually belonged to a previous key; the
            # tokenizer never produces this shape, so this is unreachable in
            # practice — keep the scalar and let parse_seq consume the items.
            target[key] = [target[key]] + self.parse_seq(key_indent)
            return
        target[key] = self._scalar_or_flow(value)

    def _block_scalar_text(self, key_indent: int) -> str:
        """Consume a block scalar's lines; returns the folded text."""
        lines: list[str] = []
        block_indent = None
        while True:
            tok = self.peek()
            if tok is None:
                break
            indent, content = tok
            if indent <= key_indent:
                break
            if block_indent is None:
                block_indent = indent
            lines.append(" " * max(0, indent - block_indent) + content)
            self.i += 1
        return "\n".join(lines)

    def parse_map(self, map_indent: int):
        result: dict = {}
        while True:
            tok = self.peek()
            if tok is None or tok[0] < map_indent or _is_seq_line(tok[1]):
                break
            indent, content = tok
            if indent > map_indent:
                raise YamlError(f"unexpected indent at: {content!r}")
            key, sep, value = self._split_key(content)
            if not sep:
                raise YamlError(f"expected 'key: value' at: {content!r}")
            self.i += 1
            self._assign(result, key, value, map_indent)
        return result

    @staticmethod
    def _split_key(content: str) -> tuple[str, bool, str]:
        """Split 'key: value' honouring quoted keys; returns (key, sep, value)."""
        if content.startswith(("'", '"')):
            end = _quoted_prefix_len(content)
            if end == -1:
                return content, False, ""
            key = _scalar(content[:end])
            rest = content[end:].lstrip()
            if rest.startswith(":"):
                return key, True, rest[1:].strip()
            return content, False, ""
        idx = _find_colon(content)
        if idx is None:
            return content, False, ""
        return content[:idx].strip(), True, content[idx + 1 :].strip()

    @staticmethod
    def _scalar_or_flow(raw: str):
        if raw.startswith(("{", "[")):
            return _parse_flow(raw)
        return _scalar(raw)


def load_yaml_subset(path: Path):
    source = path.read_text(encoding="utf-8")
    parser = _Parser(_tokenize(source))
    doc = parser.parse(0)
    if parser.peek() is not None:
        raise YamlError(f"trailing tokens at: {parser.peek()!r}")
    return doc


# ---------------------------------------------------------------------------
# OpenAPI extraction
# ---------------------------------------------------------------------------


def ref_name(ref) -> str | None:
    if not isinstance(ref, str) or not ref.startswith("#/"):
        return None
    return ref.split("/")[-1]


def _content_schema(content) -> str | None:
    if not isinstance(content, dict):
        return None
    for media in ("application/json", "application/problem+json", "text/plain", "*/*"):
        node = content.get(media)
        if isinstance(node, dict):
            schema = node.get("schema")
            if not isinstance(schema, dict):
                continue
            name = ref_name(schema.get("$ref"))
            if name:
                return name
            if schema.get("type") == "array":
                item_name = ref_name((schema.get("items") or {}).get("$ref"))
                return f"{item_name}[]" if item_name else "array"
            if schema.get("type"):
                return str(schema["type"])
            for combo in ("allOf", "oneOf", "anyOf"):
                if isinstance(schema.get(combo), list) and schema[combo]:
                    name = ref_name(schema[combo][0].get("$ref"))
                    if name:
                        return name
    return None


def _permission(op: dict) -> str:
    perm = op.get("x-required-permission")
    return perm if isinstance(perm, str) and perm else ""


def _body_example(body) -> str | None:
    content = body.get("content") if isinstance(body, dict) else None
    if not isinstance(content, dict):
        return None
    for media in ("application/json", "*/*"):
        node = content.get(media)
        if isinstance(node, dict) and "example" in node:
            try:
                return json.dumps(node["example"], indent=2)
            except (TypeError, ValueError):
                return None
    return None


def extract(spec: dict) -> dict:
    info = spec.get("info") or {}
    servers = [
        {"url": str(s["url"]), "description": str(s.get("description") or "").strip()}
        for s in (spec.get("servers") or [])
        if isinstance(s, dict) and s.get("url")
    ]

    tag_docs = {t["name"]: str(t.get("description") or "") for t in spec.get("tags") or [] if isinstance(t, dict)}
    components = spec.get("components") or {}
    param_index = {k: v for k, v in (components.get("parameters") or {}).items() if isinstance(v, dict)}
    response_index = {k: v for k, v in (components.get("responses") or {}).items() if isinstance(v, dict)}

    groups: dict[str, dict] = {}
    op_count = 0
    path_count = 0
    for path, item in (spec.get("paths") or {}).items():
        if not isinstance(item, dict):
            continue
        path_count += 1
        shared_params = item.get("parameters") or []
        for method, op in item.items():
            if method not in HTTP_METHODS or not isinstance(op, dict):
                continue
            op_count += 1
            tags = op.get("tags") or []
            if tags:
                tag = str(tags[0])
            else:
                # Domain fallback for untagged operations: first non-parameter
                # path segment (e.g. /orchestration/delegate -> orchestration).
                segments = [seg for seg in str(path).strip("/").split("/") if seg and not seg.startswith("{")]
                tag = segments[0] if segments else "root"
            group = groups.setdefault(tag, {"tag": tag, "description": tag_docs.get(tag, ""), "operations": []})

            params = []
            for p in list(shared_params) + list(op.get("parameters") or []):
                if isinstance(p, dict) and "$ref" in p:
                    p = param_index.get(ref_name(p["$ref"]), {})
                if not isinstance(p, dict):
                    continue
                schema = p.get("schema") or {}
                params.append(
                    {
                        "name": str(p.get("name") or "?"),
                        "in": str(p.get("in") or "?"),
                        "required": bool(p.get("required")),
                        "type": str(schema.get("type") or "string") if isinstance(schema, dict) else "string",
                        "description": str(p.get("description") or ""),
                    }
                )

            body = op.get("requestBody") if isinstance(op.get("requestBody"), dict) else {}
            responses = []
            for status, r in (op.get("responses") or {}).items():
                if isinstance(r, dict) and "$ref" in r:
                    r = response_index.get(ref_name(r["$ref"]), {})
                if not isinstance(r, dict):
                    continue
                responses.append(
                    {
                        "status": str(status),
                        "description": str(r.get("description") or "").strip(),
                        "schema": _content_schema(r.get("content")),
                    }
                )

            security = op.get("security")
            if security is None:
                auth = "bearerToken or apiKey"
            elif security:
                names: list[str] = []
                for entry in security:
                    if isinstance(entry, dict):
                        names.extend(str(k) for k in entry.keys())
                auth = " or ".join(sorted(set(names))) if names else "public"
            else:
                auth = "public"

            description = re.sub(r"\s+", " ", str(op.get("description") or "")).strip()

            group["operations"].append(
                {
                    "method": method.upper(),
                    "path": str(path),
                    "operationId": str(op.get("operationId") or ""),
                    "summary": str(op.get("summary") or ""),
                    "description": description,
                    "permission": _permission(op),
                    "auth": auth,
                    "params": params,
                    "bodySchema": _content_schema(body.get("content")) if body else None,
                    "bodyRequired": bool(body.get("required")) if body else False,
                    "bodyExample": _body_example(body) if body else None,
                    "responses": responses,
                }
            )

    schema_index = []
    for name, node in (components.get("schemas") or {}).items():
        if not isinstance(node, dict):
            continue
        props = []
        for pname, pnode in (node.get("properties") or {}).items():
            if not isinstance(pnode, dict):
                continue
            ptype = str(pnode.get("type") or "")
            ref = ref_name(pnode.get("$ref"))
            if not ptype and isinstance(pnode.get("items"), dict):
                item_ref = ref_name((pnode.get("items") or {}).get("$ref"))
                ptype = f"{item_ref}[]" if item_ref else "array"
            if ref and not ptype:
                ptype = ref
            props.append({"name": str(pname), "type": ptype or "-", "ref": ref})
        schema_index.append(
            {
                "name": name,
                "type": str(node.get("type") or "-"),
                "required": [str(r) for r in node.get("required") or [] if isinstance(r, (str, int))],
                "properties": props,
            }
        )
    schema_index.sort(key=lambda s: s["name"].lower())
    ordered_groups = sorted(groups.values(), key=lambda g: g["tag"].lower())

    return {
        "meta": {
            "title": str(info.get("title") or "AgentOS API"),
            "version": str(info.get("version") or ""),
            "summary": str(info.get("summary") or ""),
            "openapi": str(spec.get("openapi") or ""),
            "servers": servers,
            "pathCount": path_count,
            "operationCount": op_count,
            "schemaCount": len(schema_index),
            "generated": _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%d"),
            "source": "api/openapi.yaml",
        },
        "tags": [{"name": g["tag"], "description": g["description"]} for g in ordered_groups],
        "groups": [
            {"tag": g["tag"], "description": g["description"], "operations": g["operations"]} for g in ordered_groups
        ],
        "schemas": schema_index,
    }


def render(data: dict) -> str:
    banner = (
        "// GENERATED FILE — do not edit by hand.\n"
        "// Source: api/openapi.yaml via scripts/gen_portal_docs.py\n"
        "// Regenerate: python3 scripts/gen_portal_docs.py\n"
    )
    payload = json.dumps(data, indent=1, ensure_ascii=True, sort_keys=False)
    return banner + "window.AGENTOS_API = " + payload + ";\n"


def main(argv: list[str] | None = None) -> int:
    repo = Path(__file__).resolve().parent.parent
    ap = argparse.ArgumentParser(description="Generate the developer portal API data from the OpenAPI spec.")
    ap.add_argument("--spec", type=Path, default=repo / "api" / "openapi.yaml")
    ap.add_argument("--out", type=Path, default=repo / "docs" / "portal" / "api-data.js")
    args = ap.parse_args(argv)

    try:
        spec = load_yaml_subset(args.spec)
    except (OSError, YamlError, IndexError, ValueError) as exc:
        print(f"gen_portal_docs: failed to parse {args.spec}: {exc}", file=sys.stderr)
        return 1
    if not isinstance(spec, dict) or "paths" not in spec:
        print("gen_portal_docs: input does not look like an OpenAPI document", file=sys.stderr)
        return 1

    data = extract(spec)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(render(data), encoding="utf-8")

    meta = data["meta"]
    print(
        f"gen_portal_docs: wrote docs/portal/api-data.js "
        f"({meta['pathCount']} paths, {meta['operationCount']} operations, "
        f"{meta['schemaCount']} schemas, {len(data['groups'])} tag groups)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
