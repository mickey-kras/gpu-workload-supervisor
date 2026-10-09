#!/usr/bin/env python3
"""Verify bounded AT-SPI readiness without a desktop or native dependencies."""
import importlib.util
import contextlib
import io
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


class AccessibleFixture:
    def __init__(self, node: dict[str, str | bool], children: list['AccessibleFixture'] | None = None):
        self.node = node
        self.name = node['name']
        self.children = children or []

    def getState(self) -> types.SimpleNamespace:
        return types.SimpleNamespace(contains=lambda state: self.node[state])

    def getRoleName(self) -> str:
        return str(self.node['role'])

    def __iter__(self):
        return iter(self.children)


class RegistryFixture:
    """Model a desktop singleton poisoned by acquiring it before registration."""
    def __init__(self, content: list[dict[str, str | bool]], ticks: list[float], startup_seconds: float = 0):
        self.content = content
        self.ticks = ticks
        self.startup_seconds = startup_seconds
        self.listener = None
        self.desktop = None
        self.get_desktop_calls = 0
        self.deregistered = False

    def registerEventListener(self, listener, name: str) -> None:
        if name != 'object:children-changed':
            raise AssertionError(name)
        self.listener = listener
        self.ticks[0] += self.startup_seconds

    def getDesktop(self, index: int) -> AccessibleFixture:
        if index != 0:
            raise AssertionError(index)
        self.get_desktop_calls += 1
        if self.desktop is None:
            # Like the failed native artifact, an invalid initial singleton has
            # only the desktop node and remains empty on subsequent lookups.
            children = [AccessibleFixture(node) for node in self.content] if self.listener else []
            self.desktop = AccessibleFixture({'name': 'main', 'role': 'desktop frame',
                'showing': False, 'focused': False, 'expanded': False}, children)
        return self.desktop

    def deregisterEventListener(self, listener, name: str) -> None:
        if listener is not self.listener or name != 'object:children-changed':
            raise AssertionError('listener cleanup must match registration')
        self.listener = None
        self.deregistered = True


class SamplingTests(unittest.TestCase):
    def run_main(self, content: list[dict[str, str | bool]], output: pathlib.Path,
                 startup_seconds: float = 0, screen: str = 'models') -> RegistryFixture:
        ticks = [0.0]
        registry = RegistryFixture(content, ticks, startup_seconds)
        self.registry = registry
        context = types.SimpleNamespace(pending=lambda: False)
        glib = types.SimpleNamespace(Error=RuntimeError,
            MainContext=types.SimpleNamespace(default=lambda: context))
        native = types.SimpleNamespace(Registry=registry,
            STATE_FOCUSED='focused', STATE_SHOWING='showing', STATE_EXPANDED='expanded')
        original_observe = accessibility.observe_until_ready
        def observe(sample, validate, advance):
            return original_observe(sample, validate, advance, clock=lambda: ticks[0])
        def sleep(_seconds: float) -> None:
            ticks[0] += 1
        with patch.object(accessibility, 'pyatspi', native), patch.object(accessibility, 'GLib', glib), \
                patch.object(accessibility, 'observe_until_ready', observe), \
                patch.object(accessibility.time, 'sleep', sleep), \
                patch.object(sys, 'argv', ['setup_accessibility.py', str(output), screen, 'Continue']), \
                contextlib.redirect_stdout(io.StringIO()):
            accessibility.main()
        return registry

    def test_registration_precedes_desktop_sampling(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            registry = self.run_main(model_tree(), output)
            self.assertEqual(registry.get_desktop_calls, 1)
            self.assertTrue(registry.deregistered)
            accessibility.validate_tree(json.loads(output.read_text()), 'models', 'Continue')

    def test_persistent_root_only_and_missing_content_still_fail(self):
        for content in ([], [node for node in model_tree() if node['name'] != 'Example small'],
                        model_tree(focus=False)):
            with self.subTest(content=content), tempfile.TemporaryDirectory() as directory:
                output = pathlib.Path(directory) / 'snapshot.json'
                with self.assertRaisesRegex(AssertionError, 'AT-SPI tree did not become ready'):
                    self.run_main(content, output)
                self.assertTrue(self.registry.deregistered)
                actual = json.loads(output.read_text())
                self.assertEqual(actual[1:], content)

    def test_initialization_counts_toward_existing_deadline(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            with self.assertRaisesRegex(AssertionError, 'after deadline'):
                self.run_main(model_tree(), output, startup_seconds=4)
            self.assertTrue(self.registry.deregistered)
            accessibility.validate_tree(json.loads(output.read_text()), 'models', 'Continue')

    def test_diagnostic_sampling_releases_listener(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            registry = self.run_main(model_tree(), output, screen='diagnostic')
            self.assertTrue(registry.deregistered)


class ReadinessTests(unittest.TestCase):
    def observe(self, samples, output, advance_seconds=1, sample_seconds=0, timeout=2):
        observed = []
        ticks = [0]
        def sample():
            tree = samples[min(len(observed), len(samples) - 1)]
            observed.append(tree)
            output.write_text(json.dumps(tree))
            ticks[0] += sample_seconds
            return tree
        def advance():
            ticks[0] += advance_seconds
        attempts = accessibility.observe_until_ready(sample,
            lambda tree: accessibility.validate_tree(tree, 'models', 'Continue'),
            advance, timeout=timeout, clock=lambda: ticks[0])
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

    def test_complete_snapshot_after_blocking_sample_misses_deadline(self):
        complete = model_tree()
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            with self.assertRaisesRegex(AssertionError, 'after deadline'):
                self.observe([complete], output, sample_seconds=4, timeout=3)
            self.assertEqual(json.loads(output.read_text()), complete)

    def test_complete_snapshot_after_slow_advance_misses_deadline(self):
        complete = model_tree()
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            with self.assertRaisesRegex(AssertionError, 'after deadline'):
                self.observe([model_tree(focus=False), complete], output, advance_seconds=4, timeout=3)
            self.assertEqual(json.loads(output.read_text()), complete)


if __name__ == '__main__':
    unittest.main()
