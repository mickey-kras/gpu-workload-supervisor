"""Check input selection, extraction, and output safety."""

import importlib.util
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "snapshot", Path(__file__).with_name("snapshot.py")
)
assert spec is not None and spec.loader is not None
snapshot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(snapshot)


def stage(root: Path) -> None:
    subprocess.run(["git", "init", "--quiet", str(root)], check=True)
    subprocess.run(["git", "add", "--all"], cwd=root, check=True)


class SnapshotTests(unittest.TestCase):
    def test_input_scope(self) -> None:
        for path in (
            "cmd/gpu-mode/main.go",
            "internal/proxy/proxy_test.go",
            "clients/setup/app.mjs",
            "training/axolotl/smoke.py",
            "tests/test_ci_reuse.py",
            "docs/api.md",
            "README.md",
        ):
            self.assertTrue(snapshot.is_source(path), path)
        for path in (
            "clients/gnome/node_modules/a.js",
            "docs/image.svg",
            "graphify-out/README.md",
            "LICENSE",
            "docs/architecture/generated/a.md",
            ".github/scripts/policy-guard.cjs",
            "cmd/a.py",
            "internal/testdata/a.json",
        ):
            self.assertFalse(snapshot.is_source(path), path)

    def test_markdown_pass_preserves_code_edges_and_resolves_citations(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "internal").mkdir()
            (root / "docs").mkdir()
            (root / "internal/greet.go").write_text(
                'package greet\n\nfunc Greet() string { return "hello" }\n\nfunc Run() string { return Greet() }\n'
            )
            (root / "docs/api.md").write_text("# API\nCall `Greet()` for a greeting.\n")
            stage(root)
            with patch.object(snapshot, "ROOT", root):
                graph = snapshot.build()
                self.assertTrue(
                    any(node.get("node_kind") == "heading" for node in graph["nodes"])
                )
                self.assertTrue(
                    any(edge["relation"] == "calls" for edge in graph["edges"])
                )
                self.assertTrue(
                    any(
                        edge["relation"] == "references"
                        and edge["source_file"] == "docs/api.md"
                        for edge in graph["edges"]
                    )
                )

    def test_sources_come_from_the_index_not_unstaged_edits(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "internal").mkdir()
            (root / "internal/a.go").write_text("package a\n")
            stage(root)
            (root / "internal/a.go").write_text("package a // unstaged\n")
            (root / "internal/new.go").write_text("package a\n")
            with patch.object(snapshot, "ROOT", root):
                self.assertEqual(
                    snapshot.source_files(), {"internal/a.go": b"package a\n"}
                )

    def test_check_rejects_a_stale_graph_and_accepts_a_current_one(self) -> None:
        graph = {"nodes": [{"id": "current"}], "edges": [], "graph": {}}
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "graph.json"
            with (
                patch.object(snapshot, "OUTPUT", output),
                patch.object(snapshot, "build", return_value=graph),
                patch.object(snapshot.sys, "argv", ["snapshot.py", "--check"]),
            ):
                output.write_text(
                    snapshot.render({"nodes": [], "edges": [], "graph": {}})
                )
                with self.assertRaisesRegex(SystemExit, "stale"):
                    snapshot.main()
                output.write_text(snapshot.render(graph))
                snapshot.main()

    def test_output_symlink_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "graph.json"
            output.symlink_to(Path(directory) / "other.json")
            with (
                patch.object(snapshot, "OUTPUT", output),
                patch.object(snapshot.sys, "argv", ["snapshot.py"]),
                patch.object(
                    snapshot,
                    "build",
                    side_effect=AssertionError("Unexpected extraction"),
                ),
            ):
                with self.assertRaisesRegex(ValueError, "regular repository file"):
                    snapshot.main()


if __name__ == "__main__":
    unittest.main()
