#!/usr/bin/env python3
"""Inspect the actual AT-SPI tree while the GTK fixture main loop is running."""
import json
import pathlib
import sys

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


def main():
    tree = []
    visit(pyatspi.Registry.getDesktop(0), tree)
    pathlib.Path(sys.argv[1]).write_text(json.dumps(tree, indent=2) + '\n')
    visible = [node for node in tree if node['showing']]
    screen = sys.argv[2]
    if screen == 'diagnostic':
        print('DIAGNOSTIC: focused AT-SPI nodes:', [node for node in visible if node['focused']])
        return
    expected_focus = sys.argv[3]
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
    if len(sys.argv) > 4:
        expanded = sys.argv[4] == 'open'
        assert any(node['name'] == 'Choose another model…' and node['role'] == 'push button' and node['expanded'] == expanded for node in visible), 'model chooser accessible expansion state'
        for picker in ('Choose model file...', 'Choose model folder...'):
            assert present(picker, 'push button') == expanded, f'picker visibility: {picker}'
    assert any(node['focused'] and node['name'] == expected_focus for node in visible), (expected_focus, [node for node in visible if node['focused']])
    print(f'PASS: AT-SPI {screen} content and focused {expected_focus}')


if __name__ == '__main__':
    main()
