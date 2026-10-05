#!/usr/bin/env python3
"""The module-graph gate: walk ES module imports from the entry (js/app.js)
through the SERVED tree and verify (a) every edge resolves to a file that
exists and (b) every named import is actually exported by its target.

This is the mechanical check that would have caught the live defect where
sessions.js imported a renderBoard the board module never exported — the
binary built, the page loaded, and every console view died (PO-12).
"""
import os
import re
import sys

ROOT = "internal/daemon/static/js"
ENTRY = "app.js"


def strip_comments(src: str) -> str:
    src = re.sub(r"/\*[\s\S]*?\*/", "", src)
    # line comments (careful: only // outside strings — the heuristic is fine
    # for this tree, whose strings rarely contain //)
    return src


def read(path: str) -> str:
    with open(path, encoding="utf-8") as handle:
        return strip_comments(handle.read())


def imports_of(src: str):
    for m in re.finditer(
        r"import\s+(?:[\w$*{},\s]+?)\s+from\s+['\"]([^'\"]+)['\"]", src
    ):
        yield m.group(1)
    for m in re.finditer(r"export\s+(?:[\w$*{},\s]+?)\s+from\s+['\"]([^'\"]+)['\"]", src):
        yield m.group(1)


def resolve(from_file: str, spec: str):
    if not spec.startswith("."):
        return None  # bare specifier: none in this tree
    base = os.path.dirname(from_file)
    joined = os.path.normpath(os.path.join(base, spec))
    for candidate in (joined, joined + ".js"):
        if os.path.isfile(os.path.join(ROOT, candidate)):
            return candidate
    return ""


def exports_of(src: str):
    names = set()
    for m in re.finditer(
        r"export\s+(?:async\s+)?(?:function|const|let|var|class)\s+([A-Za-z_$][\w$]*)", src
    ):
        names.add(m.group(1))
    for m in re.finditer(r"export\s*\{([^}]*)\}", src):
        for part in m.group(1).split(","):
            part = part.split(" as ")[-1].strip()
            if part:
                names.add(part)
    return names


def main() -> int:
    missing_edges = []
    missing_exports = []
    seen = set()
    queue = [ENTRY]
    while queue:
        file = queue.pop(0)
        if file in seen:
            continue
        seen.add(file)
        path = os.path.join(ROOT, file)
        if not os.path.isfile(path):
            missing_edges.append(f"missing file: {file}")
            continue
        src = read(path)
        for spec in imports_of(src):
            resolved = resolve(file, spec)
            if resolved is None:
                missing_edges.append(f"{file}: unresolved specifier '{spec}'")
                continue
            target_src = read(os.path.join(ROOT, resolved))
            # every named import must be exported by its target
            for m in re.finditer(
                r"import\s+\{([^}]*)\}\s+from\s+['\"]" + re.escape(spec) + r"['\"]", src
            ):
                for part in m.group(1).split(","):
                    part = part.strip()
                    if not part or part.startswith("type "):
                        continue
                    name = part.split(" as ")[0].strip()
                    if name and name not in exports_of(target_src):
                        missing_exports.append(
                            f"{file}: imports '{name}' from '{spec}' — not exported"
                        )
            queue.append(resolved)
    for line in missing_edges:
        print("graph-gate:", line)
    for line in missing_exports:
        print("graph-gate:", line)
    if missing_edges or missing_exports:
        print("MODULE-GRAPH GATE FAIL")
        return 1
    print(f"MODULE-GRAPH GATE PASS ({len(seen)} modules)")
    return 0


if __name__ == "__main__":
    sys.exit(main())