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
                       'showing': states.contains(pyatspi.STATE_SHOWING)})
        for child in node:
            visit(child, result)
    except (GLib.Error, RuntimeError):
        pass


def main():
    tree = []
    visit(pyatspi.Registry.getDesktop(0), tree)
    pathlib.Path(sys.argv[1]).write_text(json.dumps(tree, indent=2) + '\n')
    visible = [node for node in tree if node['showing']]
    for application in ('ComfyUI', 'Ollama', 'llama.cpp', 'vLLM'):
        assert any(node['name'] == f'Use {application}' and node['role'] == 'check box'
                   for node in visible), f'missing accessible selection: {application}'
        assert any(node['name'] == f'Settings for {application}' and node['role'] == 'push button'
                   for node in visible), f'missing accessible settings: {application}'
    assert any(node['focused'] for node in visible), 'keyboard focus absent from AT-SPI'
    print('PASS: AT-SPI application selections, settings labels and keyboard focus')


if __name__ == '__main__':
    main()
