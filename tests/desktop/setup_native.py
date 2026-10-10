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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', required=True, type=pathlib.Path)
    parser.add_argument('--quick', action='store_true', help='Run the representative light 620x670 case only')
    args = parser.parse_args()
    root = pathlib.Path(__file__).resolve().parents[2]
    subprocess.run([sys.executable, str(root / 'tests/desktop/test_setup_accessibility.py')], check=True)
    output = args.output.resolve()
    cases = [('light', 620, 670, 1)] if args.quick else [
        (theme, width, height, scale)
        for theme in ('light', 'dark', 'highcontrast')
        for width, height in ((620, 670), (540, 620))
        for scale in (1, 2)
    ]
    reports = []
    for theme, width, height, scale in cases:
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
                    'configuration-checking', 'settings-stale-check', 'temporary-without-consent',
                    'temporary-checking', 'temporary-cleanup-required', 'temporary-close-blocked',
                    'temporary-cleanup-complete', 'temporary-cancelled-restored'}
        required.update(f'settings-{state}' for state in
                        ('ready', 'running', 'missing-location', 'unreachable', 'unsupported-trust', 'unsupported-home', 'inspection-failed'))
        required.update(f'configuration-{state}' for state in
                        ('missing-location', 'unreachable', 'unsupported-trust', 'unsupported-home', 'inspection-failed'))
        assert required <= set(report['screenshots']), ('missing native state evidence', required - set(report['screenshots']))
        for name in required:
            assert (case / f'{name}.png').is_file(), ('missing native screenshot', name)
        assert not report['criticalLogs'], 'native GTK criticals'
        assert 'No GNOME Shell' in report['scope'], 'fixture evidence scope must remain explicit'
        for image in case.glob('*.png'):
            size = subprocess.check_output(['identify', '-format', '%w %h', str(image)], text=True)
            assert tuple(map(int, size.split())) == (width * scale, height * scale), (image, size)
        reports.append(report)
    (output / 'manifest.json').write_text(json.dumps(reports, indent=2) + '\n')
    print(f'PASS: {len(reports)} native GTK cases; screenshots in {output}')


if __name__ == '__main__':
    main()
