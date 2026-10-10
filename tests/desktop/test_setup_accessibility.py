#!/usr/bin/env python3
"""Verify bounded AT-SPI readiness without a desktop or native dependencies."""
import importlib.util
from collections.abc import Callable
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
    return [{'name': name, 'role': role, 'showing': True, 'focused': focus and name == 'Continue', 'expanded': False, 'enabled': True, 'sensitive': True}
            for name, role in content]


class AccessibleFixture:
    def __init__(self, node: dict[str, str | bool], children: list['AccessibleFixture'] | None = None,
                 on_access: Callable[[], None] | None = None):
        self.node = node
        self.name = node['name']
        self.children = children or []
        self.on_access = on_access or (lambda: None)

    def getState(self) -> types.SimpleNamespace:
        self.on_access()
        return types.SimpleNamespace(contains=lambda state: self.node.get(state, False))

    def getRoleName(self) -> str:
        self.on_access()
        return str(self.node['role'])

    def __iter__(self):
        self.on_access()
        return iter(self.children)


class MainContextFixture:
    def __init__(self):
        self.depth = 0
        self.sources: dict[int, Callable[[], bool]] = {}
        self.next_source = 0

    def idle_add(self, callback: Callable[[], bool]) -> int:
        self.next_source += 1
        self.sources[self.next_source] = callback
        return self.next_source

    def source_remove(self, source: int) -> None:
        del self.sources[source]

    def pending(self) -> bool:
        return bool(self.sources)

    def iteration(self, _may_block: bool) -> bool:
        if not self.sources:
            return False
        source = next(iter(self.sources))
        self.depth += 1
        try:
            keep = self.sources[source]()
        finally:
            self.depth -= 1
        if not keep:
            del self.sources[source]
        return True

    def glib(self) -> types.SimpleNamespace:
        return types.SimpleNamespace(Error=RuntimeError, SOURCE_REMOVE=False,
            idle_add=self.idle_add, source_remove=self.source_remove,
            MainContext=types.SimpleNamespace(default=lambda: self))


class RegistryFixture:
    """Simulate a root invalidated by inline RPC dispatch outside a GLib callback.

    This exercises observer scheduling, not libatspi's internal disposal path.
    """
    def __init__(self, content: list[dict[str, str | bool]], ticks: list[float],
                 context: MainContextFixture, startup_seconds: float = 0):
        self.content = content
        self.ticks = ticks
        self.context = context
        self.startup_seconds = startup_seconds
        self.desktop: AccessibleFixture | None = None
        self.get_desktop_calls = 0
        self.native_depths: list[int] = []

    def getDesktop(self, index: int) -> AccessibleFixture:
        if index != 0:
            raise AssertionError(index)
        self.get_desktop_calls += 1
        self.native_depths.append(self.context.depth)
        self.ticks[0] += self.startup_seconds
        if self.desktop is None:
            def on_access() -> None:
                self.native_depths.append(self.context.depth)
            children = [AccessibleFixture(node, on_access=on_access) for node in self.content] if self.context.depth else []
            self.desktop = AccessibleFixture({'name': 'main', 'role': 'desktop frame',
                'showing': False, 'focused': False, 'expanded': False}, children, on_access)
        return self.desktop


class SamplingTests(unittest.TestCase):
    def run_main(self, content: list[dict[str, str | bool]], output: pathlib.Path,
                 startup_seconds: float = 0, screen: str = 'models', contract: pathlib.Path | None = None) -> RegistryFixture:
        ticks = [0.0]
        context = MainContextFixture()
        registry = RegistryFixture(content, ticks, context, startup_seconds)
        self.registry = registry
        self.context = context
        native = types.SimpleNamespace(Registry=registry,
            STATE_FOCUSED='focused', STATE_SHOWING='showing', STATE_EXPANDED='expanded', STATE_ENABLED='enabled', STATE_SENSITIVE='sensitive')
        original_observe = accessibility.observe_until_ready
        def observe(sample, validate, advance):
            return original_observe(sample, validate, advance, clock=lambda: ticks[0])
        def sleep(_seconds: float) -> None:
            ticks[0] += 1
        argv = ['setup_accessibility.py', str(output), screen, 'Continue']
        if contract is not None:
            argv.append(str(contract))
        with patch.object(accessibility, 'pyatspi', native), patch.object(accessibility, 'GLib', context.glib()), \
                patch.object(accessibility, 'observe_until_ready', observe), \
                patch.object(accessibility.time, 'monotonic', lambda: ticks[0]), \
                patch.object(accessibility.time, 'sleep', sleep), \
                patch.object(sys, 'argv', argv), \
                contextlib.redirect_stdout(io.StringIO()):
            accessibility.main()
        return registry

    def test_settings_contract_cli_does_not_trigger_model_disclosure_checks(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            contract = pathlib.Path(directory) / 'contract.json'
            contract.write_text(json.dumps({'required': [{'name': 'Example small'}, {'name': 'Continue', 'sensitive': True}],
                                            'requiredContains': ['Example']}))
            self.run_main(model_tree(), output, screen='settings', contract=contract)

    def test_sampling_retains_distinct_enabled_and_sensitive_states(self):
        content = model_tree()
        content[0].update(enabled=False, sensitive=True)
        content[1].update(enabled=True, sensitive=False)
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            self.run_main(content, output, screen='diagnostic')
            sampled = json.loads(output.read_text())[1:]
            self.assertEqual(sampled, content)
            self.assertFalse(sampled[0]['enabled'])
            self.assertTrue(sampled[0]['sensitive'])
            self.assertTrue(sampled[1]['enabled'])
            self.assertFalse(sampled[1]['sensitive'])

    def test_native_sampling_runs_inside_main_context_callback(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            registry = self.run_main(model_tree(), output)
            self.assertEqual(registry.get_desktop_calls, 1)
            self.assertGreater(len(registry.native_depths), 1)
            self.assertEqual(set(registry.native_depths), {1})
            self.assertEqual(self.context.sources, {})
            accessibility.validate_tree(json.loads(output.read_text()), 'models', 'Continue')

    def test_outside_callback_reproduces_persistent_empty_desktop(self):
        # Removing callback dispatch exercises the same main/acquisition/traversal
        # path but simulates the vulnerable g_main_depth() == 0 RPC condition.
        with patch.object(accessibility, 'sample_in_main_context', lambda sample, _deadline, clock: sample()), \
                tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            with self.assertRaisesRegex(AssertionError, 'missing model choice: Example small'):
                self.run_main(model_tree(), output)
            self.assertEqual(set(self.registry.native_depths), {0})
            self.assertEqual(len(json.loads(output.read_text())), 1)

    def test_persistent_root_only_and_missing_content_still_fail(self):
        for content in ([], [node for node in model_tree() if node['name'] != 'Example small'],
                        model_tree(focus=False)):
            with self.subTest(content=content), tempfile.TemporaryDirectory() as directory:
                output = pathlib.Path(directory) / 'snapshot.json'
                with self.assertRaisesRegex(AssertionError, 'AT-SPI tree did not become ready'):
                    self.run_main(content, output)
                self.assertEqual(self.context.sources, {})
                self.assertEqual(json.loads(output.read_text())[1:], content)

    def test_initialization_counts_toward_existing_deadline(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            with self.assertRaisesRegex(AssertionError, 'after deadline'):
                self.run_main(model_tree(), output, startup_seconds=4)
            self.assertEqual(self.context.sources, {})
            accessibility.validate_tree(json.loads(output.read_text()), 'models', 'Continue')

    def test_diagnostic_sampling_runs_inside_callback(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory) / 'snapshot.json'
            registry = self.run_main(model_tree(), output, screen='diagnostic')
            self.assertEqual(set(registry.native_depths), {1})
            self.assertEqual(self.context.sources, {})

    def test_callback_errors_propagate_and_remove_source(self):
        for error in (ValueError('snapshot failed'), KeyboardInterrupt()):
            with self.subTest(error=error):
                context = MainContextFixture()
                def sample() -> list[dict[str, str | bool]]:
                    raise error
                with patch.object(accessibility, 'GLib', context.glib()), \
                        self.assertRaises(type(error)) as caught:
                    accessibility.sample_in_main_context(sample, 3, clock=lambda: 0)
                self.assertIs(caught.exception, error)
                self.assertEqual(context.sources, {})

    def test_context_error_removes_unexecuted_source(self):
        context = MainContextFixture()
        with patch.object(accessibility, 'GLib', context.glib()), \
                patch.object(context, 'iteration', side_effect=RuntimeError('dispatch failed')), \
                self.assertRaisesRegex(RuntimeError, 'dispatch failed'):
            accessibility.sample_in_main_context(lambda: model_tree(), 3, clock=lambda: 0)
        self.assertEqual(context.sources, {})

    def test_competing_source_cannot_extend_shared_observation_deadline(self):
        context = MainContextFixture()
        ticks = [0.0]
        acquired: list[float] = []
        dispatch_modes: list[bool] = []
        competing_sources: list[int] = []
        def competitor() -> bool:
            ticks[0] += 0.5
            return True
        original_iteration = context.iteration
        def iteration(may_block: bool) -> bool:
            dispatch_modes.append(may_block)
            return original_iteration(may_block)
        def sample(deadline: float) -> list[dict[str, str | bool]]:
            if acquired:
                competing_sources.append(context.idle_add(competitor))
            def acquire() -> list[dict[str, str | bool]]:
                acquired.append(ticks[0])
                # The first incomplete observation consumes two seconds. The
                # next queued observation has only the remaining second.
                ticks[0] += 2
                return model_tree(focus=False)
            return accessibility.sample_in_main_context(acquire, deadline, clock=lambda: ticks[0])
        with patch.object(accessibility, 'GLib', context.glib()), \
                patch.object(context, 'iteration', iteration), \
                self.assertRaisesRegex(AssertionError, 'queued observation reached deadline'):
            accessibility.observe_until_ready(sample,
                lambda tree: accessibility.validate_tree(tree, 'models', 'Continue'),
                lambda: None, timeout=3, clock=lambda: ticks[0])
        self.assertEqual(ticks[0], 3)
        self.assertEqual(acquired, [0])
        self.assertEqual(dispatch_modes, [False] * 3)
        self.assertEqual(list(context.sources), competing_sources)


class ReadinessTests(unittest.TestCase):
    def observe(self, samples, output, advance_seconds=1, sample_seconds=0, timeout=2):
        observed = []
        ticks = [0]
        def sample(_deadline):
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


class SettingsContractTests(unittest.TestCase):
    def setUp(self):
        self.identity = 'Service: comfyui-production-long-installation.service · /home/example/.config/systemd/user/comfyui-production-long-installation.service · http://127.0.0.1:8188'
        self.tree = [{'name': name, 'role': role, 'showing': True, 'focused': name == 'ComfyUI', 'enabled': False, 'sensitive': sensitive}
                     for name, role, sensitive in [('ComfyUI', 'label', True), (self.identity, 'label', True),
                                                 ('Use installation', 'push button', False), ('Cancel', 'push button', True)]]
        self.contract = {'required': [{'name': self.identity}, {'name': 'Use installation', 'role': 'push button', 'sensitive': False}],
                         'absent': ['Display name', 'Measured VRAM requirement (MiB; optional)']}

    def validate(self, tree=None):
        accessibility.validate_tree(self.tree if tree is None else tree, 'settings', 'ComfyUI', contract=self.contract)

    def test_full_identity_and_disabled_action_are_required_together(self):
        self.validate()
        self.tree[1]['name'] = 'Service: comfyui-production…'
        with self.assertRaisesRegex(AssertionError, 'missing settings content'):
            self.validate()

    def test_sensitive_unsafe_action_fails(self):
        self.tree[2]['sensitive'] = True
        with self.assertRaisesRegex(AssertionError, 'wrong action availability'):
            self.validate()

    def test_sensitive_action_passes_without_enabled_state(self):
        self.tree[2].update(enabled=False, sensitive=True)
        self.contract['required'][1]['sensitive'] = True
        self.validate()

    def test_insensitive_action_does_not_become_operable_from_enabled_state(self):
        self.tree[2].update(enabled=True, sensitive=False)
        self.validate()
        self.contract['required'][1]['sensitive'] = True
        with self.assertRaisesRegex(AssertionError, 'wrong action availability'):
            self.validate()

    def test_contract_cannot_silently_keep_using_enabled_for_availability(self):
        self.contract['required'][1] = {'name': 'Use installation', 'enabled': False}
        with self.assertRaisesRegex(AssertionError, 'must specify sensitive'):
            self.validate()

    def test_hidden_or_unrelated_content_cannot_satisfy_contract(self):
        self.tree[1]['showing'] = False
        with self.assertRaisesRegex(AssertionError, 'missing settings content'):
            self.validate()
        self.tree[1]['showing'] = True
        self.tree.append({'name': 'Display name', 'role': 'entry', 'showing': True, 'focused': False})
        with self.assertRaisesRegex(AssertionError, 'unrelated settings content visible'):
            self.validate()

    def test_contract_does_not_replace_focus_requirement(self):
        self.tree[0]['focused'] = False
        with self.assertRaises(AssertionError):
            self.validate()

    def test_required_diagnostic_fragment_must_be_showing(self):
        self.contract['requiredContains'] = ['Untrusted path:']
        with self.assertRaisesRegex(AssertionError, 'missing settings text'):
            self.validate()
        self.tree.append({'name': 'Untrusted path: /home/example/shared/ComfyUI/main.py', 'role': 'label',
                          'showing': True, 'focused': False})
        self.validate()


if __name__ == '__main__':
    unittest.main()
