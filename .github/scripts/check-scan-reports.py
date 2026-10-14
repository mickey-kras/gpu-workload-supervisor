#!/usr/bin/env python3
"""Reject missing scanner output before report conversion or findings enforcement."""
import argparse
import json
from pathlib import Path


def check_trivy(path):
    report = json.loads(path.read_text())
    if not isinstance(report, dict) or report.get('SchemaVersion') != 2 or \
            report.get('ArtifactType') not in ('filesystem', 'repository') or not isinstance(report.get('ArtifactName'), str) or \
            not report['ArtifactName'] or not isinstance(report.get('Results', []), list):
        raise ValueError('Trivy did not produce a filesystem scan report')
    for result in report.get('Results', []):
        if not isinstance(result, dict) or not isinstance(result.get('Target'), str):
            raise ValueError('Invalid Trivy scan result')
        for key in ('Vulnerabilities', 'Misconfigurations', 'Secrets', 'Licenses'):
            if not isinstance(result.get(key, []), list):
                raise ValueError(f'Invalid Trivy {key}')


def check_aislop(path):
    report = json.loads(path.read_text())
    if not isinstance(report, dict) or report.get('version') != '2.1.0' or \
            not isinstance(report.get('runs'), list) or len(report['runs']) != 1:
        raise ValueError('Aislop did not produce a SARIF report')
    run = report['runs'][0]
    if not isinstance(run, dict):
        raise ValueError('Invalid Aislop scanner run')
    tool = run.get('tool')
    if not isinstance(tool, dict):
        raise ValueError('Invalid Aislop scanner tool')
    driver = tool.get('driver')
    if not isinstance(driver, dict) or driver.get('name') != 'aislop' or not isinstance(run.get('results'), list):
        raise ValueError('Invalid Aislop scanner results')
    findings = []
    for result in run['results']:
        if not isinstance(result, dict) or not isinstance(result.get('ruleId'), str) or not result['ruleId']:
            raise ValueError('Invalid Aislop finding')
        if result['ruleId'] != 'security/dependency-audit-skipped':
            findings.append(result['ruleId'])
    if findings:
        raise ValueError('Aislop findings: ' + ', '.join(sorted(set(findings))))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('scanner', choices=('trivy', 'aislop'))
    parser.add_argument('report', type=Path)
    args = parser.parse_args()
    try:
        (check_trivy if args.scanner == 'trivy' else check_aislop)(args.report)
    except (OSError, ValueError, AttributeError, TypeError) as error:
        parser.exit(1, f'Scanner report rejected: {error}\n')


if __name__ == '__main__':
    main()
