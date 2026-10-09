#!/usr/bin/env python3
"""Inspect the actual AT-SPI tree while the GTK fixture main loop is running."""
import json
import pathlib
import sys
import time
from collections.abc import Callable

import pyatspi
from gi.repository import GLib


def visit(node, result):
    try:
        states = node.getState()
        result.append({'name': node.name, 'role': node.getRoleName(),
                       'focused': states.contains(pyatspi.STATE_FOCUSED),
                       'showing': states.contains(pyatspi.STATE_SHOWING),
                       'expanded': states.contains(pyatspi.STATE_EXPANDED)})
        for child in node:
            visit(child, result)
    except (GLib.Error, RuntimeError):
        pass


def validate_tree(tree, screen, expected_focus, disclosure=None):
    visible = [node for node in tree if node['showing']]
    def present(name, role=None):
        return any(node['name'] == name and (role is None or node['role'] == role)
                   for node in visible)
    if screen == 'applications':
        for application in ('ComfyUI', 'Ollama', 'llama.cpp', 'vLLM'):
            assert present(f'Use {application}', 'check box'), f'missing selection: {application}'
            assert present(f'Settings for {application}', 'push button'), f'missing settings: {application}'
        assert present('Continue', 'push button'), 'missing Continue action'
    elif screen == 'models':
        for model in ('Example small', 'Example large'):
            assert present(model, 'check box'), f'missing model choice: {model}'
        for application in ('ComfyUI', 'Ollama'):
            assert present(f'Settings for {application}', 'push button'), f'missing model settings: {application}'
        assert present('Continue', 'push button') and present('Back', 'push button'), 'missing model navigation'
    elif screen == 'review':
        assert present('Finish setup', 'push button') and present('Back', 'push button'), 'missing review actions'
        for workload in ('ComfyUI', 'Ollama - Example small', 'Ollama - Example large'):
            assert present(workload), f'missing review workload: {workload}'
    else:
        raise AssertionError(f'unknown screen: {screen}')
    if disclosure is not None:
        expanded = disclosure == 'open'
        assert any(node['name'] == 'Choose another model…' and node['role'] == 'push button' and node['expanded'] == expanded for node in visible), 'model chooser accessible expansion state'
        for picker in ('Choose model file...', 'Choose model folder...'):
            assert present(picker, 'push button') == expanded, f'picker visibility: {picker}'
    assert any(node['focused'] and node['name'] == expected_focus for node in visible), (expected_focus, [node for node in visible if node['focused']])


def observe_until_ready(sample, validate, advance, timeout=3.0, clock=time.monotonic):
    """Observe complete content and focus together within a fixed deadline."""
    deadline = clock() + timeout
    attempts = 0
    while True:
        tree = sample()
        attempts += 1
        try:
            validate(tree)
        except AssertionError as error:
            if clock() >= deadline:
                raise AssertionError(f'AT-SPI tree did not become ready: {error}') from error
        else:
            if clock() > deadline:
                raise AssertionError('AT-SPI complete observation arrived after deadline')
            return attempts
        advance()


def sample_in_main_context(sample: Callable[[], list[dict[str, str | bool]]]) -> list[dict[str, str | bool]]:
    """Acquire and traverse native proxies within a GLib dispatch callback."""
    tree: list[dict[str, str | bool]] = []
    error: BaseException | None = None
    completed = False
    def inspect() -> bool:
        nonlocal tree, error, completed
        try:
            # libatspi's synchronous external RPC path otherwise dispatches
            # pending D-Bus messages when g_main_depth() is zero, including while
            # desktop proxies are being initialized. Avoid that inline dispatch.
            tree = sample()
        except BaseException as caught:
            # GLib logs callback exceptions instead of propagating them to main.
            error = caught
        finally:
            completed = True
        return GLib.SOURCE_REMOVE
    context = GLib.MainContext.default()
    source = GLib.idle_add(inspect)
    try:
        while not completed:
            context.iteration(True)
    finally:
        if not completed:
            GLib.source_remove(source)
    if error is not None:
        raise error
    return tree


def main() -> None:
    output = pathlib.Path(sys.argv[1])
    screen = sys.argv[2]
    desktop = None
    def read_tree() -> list[dict[str, str | bool]]:
        nonlocal desktop
        if desktop is None:
            desktop = pyatspi.Registry.getDesktop(0)
        tree: list[dict[str, str | bool]] = []
        visit(desktop, tree)
        # Retain the latest actual tree even when readiness never arrives.
        output.write_text(json.dumps(tree, indent=2) + '\n')
        return tree
    def sample() -> list[dict[str, str | bool]]:
        # observe_until_ready starts its deadline before this callback is queued,
        # so desktop initialization and traversal consume the same fixed budget.
        return sample_in_main_context(read_tree)
    if screen == 'diagnostic':
        print('DIAGNOSTIC: focused AT-SPI nodes:', [node for node in sample() if node['showing'] and node['focused']])
        return
    expected_focus = sys.argv[3]
    disclosure = sys.argv[4] if len(sys.argv) > 4 else None
    def advance():
        context = GLib.MainContext.default()
        for _ in range(100):
            if not context.pending():
                break
            context.iteration(False)
        time.sleep(0.03)
    attempts = observe_until_ready(sample, lambda tree: validate_tree(tree, screen, expected_focus, disclosure), advance)
    print(f'PASS: AT-SPI {screen} content and focused {expected_focus} ({attempts} observations)')


if __name__ == '__main__':
    main()
