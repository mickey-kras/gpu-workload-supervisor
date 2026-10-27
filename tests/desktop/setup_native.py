#!/usr/bin/env python3
"""Capture the production GTK factory on a disposable X11 desktop, with generic data.

Requires gjs, GTK4/Libadwaita typelibs, Xvfb, dbus-run-session, xdotool and
ImageMagick. No GNOME Shell or GPU hardware is qualified by this fixture.
"""
import argparse
import json
import os
import pathlib
import subprocess
import sys


def select_cases(shard_index=None, shard_count=None, quick=False):
    cases = [(theme, width, height, scale)
             for theme in ('light', 'dark', 'highcontrast')
             for width, height in ((620, 740), (620, 670), (480, 480))
             for scale in (1, 2)]
    if quick:
        if shard_index is not None or shard_count is not None:
            raise ValueError('--quick cannot be combined with sharding')
        return [('light', 620, 740, 1)]
    if shard_index is None and shard_count is None:
        return cases
    if shard_count is None or shard_index is None:
        raise ValueError('--shard-index and --shard-count must be specified together')
    if not 1 <= shard_count <= len(cases) or not 0 <= shard_index < shard_count:
        raise ValueError('shard count must be 1..18 and index must be 0..count-1')
    return cases[shard_index::shard_count]


def run_case(root, output, dimensions):
    theme, width, height, scale = dimensions
    case = output / f'{theme}-{width}x{height}-{scale}x'
    case.mkdir(parents=True, exist_ok=True)
    environment = {**os.environ, 'GDK_BACKEND': 'x11', 'GSK_RENDERER': 'cairo',
                   'GDK_SCALE': str(scale), 'XDG_CURRENT_DESKTOP': 'GNOME',
                   'GSETTINGS_BACKEND': 'memory', 'GTK_A11Y': 'atspi'}
    # Settings, long-diagnostic, stale async, and consent/cleanup paths now
    # run in each matrix cell; retain a bounded process deadline for hangs.
    result = subprocess.run(['xvfb-run', '-a', '-s', '-screen 0 1600x1600x24',
                    'dbus-run-session', '--', 'gjs', '-m',
                    str(root / 'tests/desktop/setup_native.mjs'), str(case), theme,
                    str(width), str(height)], cwd=root, env=environment, check=False, timeout=240, capture_output=True, text=True)
    log = result.stdout + result.stderr
    (case / 'native.log').write_text(log)
    print(log, end='')
    assert result.returncode == 0, f'native fixture failed: {case}'
    assert 'Theme parser error' not in log and 'CSS parser error' not in log, 'GTK stylesheet parse error'
    assert 'Failed to load' not in log and 'Unrecognized image file format' not in log, 'native icon load failure'
    report = json.loads((case / 'report.json').read_text())
    # A report can only qualify captured production widgets; require every
    # issue state so an accidentally skipped path cannot produce a green run.
    required = {'settings-rechecked-ready', 'settings-stopped', 'settings-installation-long-identity', 'installation-selection', 'application-settings', 'settings-advanced-edited',
                'settings-resource-checks', 'settings-launch-details', 'settings-draft-cancelled',
                'configuration-checking', 'settings-stale-check', 'settings-stale-endpoint-check'}
    required.update(('diagnostic-export', 'settings-installed-blocked', 'correction-unavailable', 'correction-selected-preview', 'correction-inventory-disappeared'))
    required.update(('included-unresolved-settings', 'included-unresolved-blocked', 'included-valid-rest-review'))
    required.update(f'settings-{state}' for state in
                    ('ready', 'running', 'missing-location', 'unreachable', 'recognized-unreachable', 'invalid-contradictory', 'gpu-unresolved', 'unsupported-trust', 'unsupported-home', 'inspection-failed'))
    required.update(f'configuration-{state}' for state in
                    ('missing-location', 'unreachable', 'invalid-contradictory', 'gpu-unresolved', 'unsupported-trust', 'unsupported-home', 'inspection-failed'))
    assert required <= set(report['screenshots']), ('missing native state evidence', required - set(report['screenshots']))
    for name in required:
        assert (case / f'{name}.png').is_file(), ('missing native screenshot', name)
    for name in ('correction-selected-preview',):
        evidence = next((item for item in report['geometry'] if item['screen'] == name and 'correction' in item), None)
        assert evidence and {item['control'] for item in evidence['correction']} == {'override-preview', 'apply-override'}, ('missing visible correction evidence', name)
        viewport = evidence['viewport']
        for item in evidence['correction']:
            assert item['x'] >= 0 and item['y'] >= 0 and item['width'] > 0 and item['height'] > 0 and item['x'] + item['width'] <= viewport['width'] and item['y'] + item['height'] <= viewport['height'], ('correction control clipped', name, item)
    assert not report['criticalLogs'], 'native GTK criticals'
    assert 'No GNOME Shell' in report['scope'], 'fixture evidence scope must remain explicit'
    for image in case.glob('*.png'):
        size = subprocess.check_output(['identify', '-format', '%w %h', str(image)], text=True)
        assert tuple(map(int, size.split())) == (width * scale, height * scale), (image, size)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, type=pathlib.Path)
    parser.add_argument('--quick', action='store_true', help='Run the default light 620x740 case only')
    parser.add_argument('--shard-index', type=int, help='Zero-based index of the assigned shard')
    parser.add_argument('--shard-count', type=int, help='Partition all 18 cases across this many shards')
    args = parser.parse_args()
    try:
        cases = select_cases(args.shard_index, args.shard_count, args.quick)
    except ValueError as error:
        parser.error(str(error))
    root = pathlib.Path(__file__).resolve().parents[2]
    subprocess.run([sys.executable, str(root / 'tests/desktop/test_setup_accessibility.py')], check=True)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    reports = []
    for dimensions in cases:
        reports.append(run_case(root, output, dimensions))
    (output / 'manifest.json').write_text(json.dumps(reports, indent=2) + '\n')
    print(f'PASS: {len(reports)} native GTK cases; screenshots in {output}')


if __name__ == '__main__':
    main()

