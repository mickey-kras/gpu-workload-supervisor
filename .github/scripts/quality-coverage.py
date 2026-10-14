#!/usr/bin/env python3
"""Pass successful quality coverage to Sonar for the same tested checkout and attempt."""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import subprocess


REPORTS = {'go': Path('coverage.out'), 'javascript': Path('clients/gnome/coverage/lcov.info')}


def identity():
    return {
        'repository': os.environ['GITHUB_REPOSITORY'],
        'builtCommit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip(),
        'runID': os.environ['GITHUB_RUN_ID'],
        'runAttempt': os.environ['GITHUB_RUN_ATTEMPT'],
    }


def checked_report(path, kind):
    content = path.read_bytes()
    text = content.decode('utf-8')
    if kind == 'go':
        lines = text.splitlines()
        valid = len(lines) > 1 and lines[0] in ('mode: set', 'mode: count', 'mode: atomic') and \
            all(re.fullmatch(r'[^:]+:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+ [0-9]+ [0-9]+', line) for line in lines[1:])
    else:
        lines = text.splitlines()
        files = [line for line in lines if line.startswith('SF:')]
        records = [line for line in lines if line.startswith('DA:')]
        valid = bool(files and records) and all(len(line) > 3 for line in files) and \
            len(files) == lines.count('end_of_record') and \
            all(re.fullmatch(r'DA:[0-9]+,[0-9]+(?:,[0-9a-fA-F]+)?', line) for line in records)
    if not valid:
        raise ValueError(f'Missing or unreadable {kind} coverage; re-run all jobs')
    return content


def stage(kind, directory):
    report = REPORTS[kind]
    content = checked_report(report, kind)
    directory.mkdir(parents=True, exist_ok=False)
    (directory / report.name).write_bytes(content)
    manifest = {'schemaVersion': 1, 'kind': kind, **identity(),
                'report': report.name, 'sha256': hashlib.sha256(content).hexdigest()}
    (directory / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')


def restore(kind, directory):
    manifest = json.loads((directory / 'manifest.json').read_text())
    expected = {'schemaVersion': 1, 'kind': kind, **identity(), 'report': REPORTS[kind].name}
    if set(manifest) != {*expected, 'sha256'} or any(manifest[key] != value for key, value in expected.items()):
        raise ValueError(f'{kind} coverage identity mismatch; re-run all jobs')
    source = directory / REPORTS[kind].name
    content = checked_report(source, kind)
    if hashlib.sha256(content).hexdigest() != manifest['sha256']:
        raise ValueError(f'{kind} coverage digest mismatch; re-run all jobs')
    destination = REPORTS[kind]
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_bytes(content)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=('stage', 'restore'))
    parser.add_argument('kind', choices=REPORTS)
    parser.add_argument('directory', type=Path)
    args = parser.parse_args()
    try:
        (stage if args.operation == 'stage' else restore)(args.kind, args.directory)
    except (OSError, ValueError, KeyError) as error:
        parser.exit(1, f'Coverage transfer failed: {error}; re-run all jobs\n')


if __name__ == '__main__':
    main()
