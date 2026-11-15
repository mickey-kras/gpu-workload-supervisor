"""Build or check the Graphify snapshot of application code and authored documentation.

The main branch owns graphify-out/graph.json. Pull requests never change it: the
graphify workflow rebuilds it after each merge and opens a one-file refresh PR.
"""

import argparse
import hashlib
import json
import os
import subprocess
import sys
import tempfile
from importlib.metadata import version
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
ROOT = Path(os.environ.get("GRAPHIFY_SOURCE_ROOT", TOOLS.parents[1])).resolve()
OUTPUT = ROOT / "graphify-out" / "graph.json"


CODE_ROOTS = {
    "cmd": {".go"},
    "internal": {".go"},
    "clients": {".js", ".mjs", ".cjs"},
    "training": {".py"},
    "tests": {".go", ".py", ".mjs"},
}
DOCUMENTS = {"README.md", "SECURITY.md", "AGENTS.md"}
EXCLUDED_PARTS = {"node_modules", "generated", "archive", "changelog"}


def is_source(name: str) -> bool:
    path = Path(name)
    if any(part in EXCLUDED_PARTS for part in path.parts):
        return False
    return (
        path.suffix in CODE_ROOTS.get(path.parts[0], set())
        or name in DOCUMENTS
        or (path.parts[0] == "docs" and path.suffix in {".md", ".mdx"})
    )


def tool_digest() -> str:
    digest = hashlib.sha256()
    for name in ("snapshot.py", "requirements.txt"):
        digest.update((TOOLS / name).read_bytes())
    return digest.hexdigest()


def content_digest(graph: dict[str, object]) -> str:
    content = {key: value for key, value in graph.items() if key != "graph"}
    return hashlib.sha256(json.dumps(content, sort_keys=True).encode()).hexdigest()


def source_files() -> dict[str, bytes]:
    """Read graph sources from the Git index, so a commit's graph never includes unstaged edits."""
    entries = (
        subprocess.check_output(
            ["git", "ls-files", "-s", "-z", "--", *CODE_ROOTS, "docs", *DOCUMENTS],
            cwd=ROOT,
        )
        .decode()
        .split("\0")
    )
    files: dict[str, bytes] = {}
    for entry in filter(None, entries):
        info, name = entry.split("\t", 1)
        mode, blob, _stage = info.split()
        if not is_source(name):
            continue
        if mode not in ("100644", "100755"):
            raise ValueError(f"Source must be a regular repository file: {name}")
        files[name] = subprocess.check_output(
            ["git", "cat-file", "blob", blob], cwd=ROOT
        )
    if not files:
        raise ValueError("No tracked graph sources found")
    return dict(sorted(files.items()))


def source_digest(files: dict[str, bytes]) -> str:
    digest = hashlib.sha256()
    for name, content in files.items():
        digest.update(name.encode() + b"\0" + hashlib.sha256(content).digest())
    return digest.hexdigest()


def build() -> dict[str, object]:
    files = source_files()
    sources = list(files)
    with tempfile.TemporaryDirectory(prefix="graphify-") as temporary:
        scan = Path(temporary) / "source"
        scan.mkdir()
        for name, content in files.items():
            target = scan / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(content)
        env = {**os.environ, "GRAPHIFY_BACKEND": "none", "PYTHONHASHSEED": "0"}
        subprocess.run(
            [
                sys.executable,
                "-m",
                "graphify",
                "extract",
                str(scan),
                "--code-only",
                "--no-cluster",
                "--max-workers",
                "1",
                "--out",
                temporary,
            ],
            cwd=scan,
            env=env,
            check=True,
        )
        graph = json.loads(
            (Path(temporary) / "graphify-out" / "graph.json").read_text()
        )
        # A separate deterministic Markdown pass adds headings, links and code citations.
        from graphify.build import build as merge_graphs
        from graphify.export import to_json
        from graphify.extract import extract

        documents = [
            scan / name for name in sources if Path(name).suffix in {".md", ".mdx"}
        ]
        docs = extract(
            documents,
            root=scan,
            cache_root=Path(temporary) / "docs-cache",
            parallel=False,
            resolution_context_nodes=graph["nodes"],
            resolution_context_edges=graph["edges"],
        )
        if docs["failed_sources"]:
            raise ValueError(
                f"Documentation extraction failed: {docs['failed_sources']}"
            )
        merged = merge_graphs([graph, docs], directed=True, dedup=False, root=scan)
        merged_path = Path(temporary) / "merged.json"
        if not to_json(
            merged,
            {},
            str(merged_path),
            force=True,
            original_links=graph["edges"] + docs["edges"],
        ):
            raise ValueError("Graph export failed")
        graph = json.loads(merged_path.read_text())
        graph["edges"] = graph.pop("links")
    for key in ("nodes", "edges", "hyperedges"):
        graph[key] = sorted(
            graph.get(key, []), key=lambda item: json.dumps(item, sort_keys=True)
        )
    graph["graph"] = {
        "graphify_version": version("graphifyy"),
        "source_sha256": source_digest(files),
        "tool_sha256": tool_digest(),
        "content_sha256": content_digest(graph),
        "scope": "Application code, tests and authored Markdown documentation",
    }
    return graph


def render(graph: dict[str, object]) -> str:
    return json.dumps(graph, ensure_ascii=False, indent=2, sort_keys=True) + "\n"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--check",
        action="store_true",
        help="Fail when the committed graph does not match the sources",
    )
    args = parser.parse_args()
    if OUTPUT.is_symlink() or OUTPUT.parent.is_symlink():
        raise ValueError("Graph output must be a regular repository file")
    graph = build()
    if args.check:
        committed = json.loads(OUTPUT.read_text()) if OUTPUT.exists() else {}
        if committed != json.loads(render(graph)):
            for key in ("nodes", "edges", "graph"):
                old, new = committed.get(key), graph.get(key)
                if key == "graph":
                    print(f"graph metadata: committed={old} built={new}")
                    continue
                old_items = {json.dumps(item, sort_keys=True) for item in old or []}
                new_items = {json.dumps(item, sort_keys=True) for item in new or []}
                print(
                    f"{key}: {len(old_items - new_items)} only committed, {len(new_items - old_items)} only built"
                )
                for item in sorted(old_items ^ new_items)[:3]:
                    print(f"  {item[:300]}")
            raise SystemExit(
                "graphify-out/graph.json is stale. See docs/CODE-GRAPH.md."
            )
        print("Graph matches the sources.")
        return
    OUTPUT.parent.mkdir(exist_ok=True)
    OUTPUT.write_text(render(graph))
    print(f"Wrote {OUTPUT.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
