#!/usr/bin/env python3
"""Verify bounded AT-SPI readiness without a desktop or native dependencies."""
import importlib.util
import json
import pathlib
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

module_path = pathlib.Path(__file__).with_name('setup_accessibility.py')
spec = importlib.util.spec_from_file_location('setup_accessibility', module_path)
accessibility = importlib.util.module_from_spec(spec)
with patch.dict(sys.modules, {'pyatspi': types.ModuleType('pyatspi'), 'gi': types.ModuleType('gi'),
                             'gi.repository': types.SimpleNamespace(GLib=None)}):
    spec.loader.exec_module(accessibility)


def model_tree(focus=True):
    content = [('Example small', 'check box'), ('Example large', 'check box'),
               ('Settings for ComfyUI', 'push button'), ('Settings for Ollama', 'push button'),
               ('Continue', 'push button'), ('Back', 'push button')]
    return [{'name': name, 'role': role, 'showing': True, 'focused': focus and name == 'Continue', 'expanded': False}
            for name, role in content]


class ReadinessTests(unittest.TestCase):
    def observe(self, samples, output):
        observed = []
        def sample():
            tree = samples[min(len(observed), len(samples) - 1)]
            observed.append(tree)
            output.write_text(json.dumps(tree))
            return tree
        ticks = [0]
        def advance():
            ticks[0] += 1
        attempts = accessibility.observe_until_ready(sample,
            lambda tree: accessibility.validate_tree(tree, 'models', 'Continue'),
            advance, timeout=2, clock=lambda: ticks[0])
        return attempts, observed

    def test_incomplete_initial_tree_requires_complete_content_and_focus(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            complete = model_tree()
            attempts, observed = self.observe([[], model_tree(focus=False), complete], output)
            self.assertEqual(attempts, 3)
            self.assertEqual(len(observed), 3)
            self.assertEqual(json.loads(output.read_text()), complete)

    def test_missing_model_never_passes_and_retains_final_snapshot(self):
        incomplete = [node for node in model_tree() if node['name'] != 'Example small']
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            with self.assertRaisesRegex(AssertionError, 'missing model choice: Example small'):
                self.observe([[], incomplete], output)
            self.assertEqual(json.loads(output.read_text()), incomplete)

    def test_missing_focus_never_passes_and_retains_final_snapshot(self):
        unfocused = model_tree(focus=False)
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            with self.assertRaisesRegex(AssertionError, 'Continue'):
                self.observe([unfocused], output)
            self.assertEqual(json.loads(output.read_text()), unfocused)


if __name__ == '__main__':
    unittest.main()
