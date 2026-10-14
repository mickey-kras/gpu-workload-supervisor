import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
COVERAGE_SCRIPT = ROOT / '.github/scripts/quality-coverage.py'
REPORT_SCRIPT = ROOT / '.github/scripts/check-scan-reports.py'


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


coverage = load_module('quality_coverage', COVERAGE_SCRIPT)
reports = load_module('scan_reports', REPORT_SCRIPT)


class CoverageReuseTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.original = Path.cwd()
        os.chdir(self.root)
        self.addCleanup(os.chdir, self.original)
        subprocess.run(['git', 'init', '-q'], check=True)
        subprocess.run(['git', '-c', 'user.name=fixture', '-c', 'user.email=fixture@example.invalid',
                        'commit', '--allow-empty', '-qm', 'fixture'], check=True)
        self.environment = mock.patch.dict(os.environ, {
            'GITHUB_REPOSITORY': 'fixture/repository', 'GITHUB_RUN_ID': '123', 'GITHUB_RUN_ATTEMPT': '1',
        })
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.samples = {'go': 'mode: atomic\nfixture/module/file.go:1.1,2.2 1 3\n',
                        'javascript': 'TN:\nSF:src/file.mjs\nDA:1,2\nend_of_record\n'}

    def stage(self, kind):
        source = coverage.REPORTS[kind]
        source.parent.mkdir(parents=True, exist_ok=True)
        source.write_text(self.samples[kind])
        artifact = Path(f'{kind}-artifact')
        coverage.stage(kind, artifact)
        source.unlink()
        return artifact

    def test_exact_successful_coverage_is_restored_without_recomputation(self):
        for kind in self.samples:
            artifact = self.stage(kind)
            coverage.restore(kind, artifact)
            self.assertEqual(coverage.REPORTS[kind].read_text(), self.samples[kind])

    def test_foreign_repository_commit_run_attempt_kind_or_report_is_rejected(self):
        for field, value in [('repository', 'foreign/repository'), ('builtCommit', '0' * 40),
                             ('runID', '124'), ('runAttempt', '2'), ('kind', 'javascript'),
                             ('report', '../coverage.out'), ('schemaVersion', 2)]:
            with self.subTest(field=field):
                artifact = self.stage('go')
                manifest = json.loads((artifact / 'manifest.json').read_text())
                manifest[field] = value
                (artifact / 'manifest.json').write_text(json.dumps(manifest))
                with self.assertRaisesRegex(ValueError, 'identity mismatch'):
                    coverage.restore('go', artifact)
                self.assertFalse(coverage.REPORTS['go'].exists())
                for path in artifact.iterdir():
                    path.unlink()
                artifact.rmdir()

    def test_partial_rerun_cannot_reuse_earlier_attempt_coverage(self):
        artifact = self.stage('go')
        with mock.patch.dict(os.environ, {'GITHUB_RUN_ATTEMPT': '2'}):
            with self.assertRaisesRegex(ValueError, 're-run all jobs'):
                coverage.restore('go', artifact)
        self.assertFalse(coverage.REPORTS['go'].exists())

    def test_missing_changed_or_unreadable_reports_do_not_create_coverage(self):
        for mutation in ('missing-manifest', 'missing-report', 'digest', 'empty', 'malformed'):
            with self.subTest(mutation=mutation):
                artifact = self.stage('go')
                report = artifact / 'coverage.out'
                if mutation == 'missing-manifest':
                    (artifact / 'manifest.json').unlink()
                elif mutation == 'missing-report':
                    report.unlink()
                elif mutation == 'digest':
                    report.write_text(self.samples['go'].replace('1 3', '1 0'))
                elif mutation == 'empty':
                    report.write_text('')
                else:
                    (artifact / 'manifest.json').write_text('not json')
                result = subprocess.run(['python3', str(COVERAGE_SCRIPT), 'restore', 'go', str(artifact)],
                                        capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('re-run all jobs', result.stderr)
                self.assertFalse(coverage.REPORTS['go'].exists())
                for path in artifact.iterdir():
                    path.unlink()
                artifact.rmdir()

    def test_stage_refuses_missing_or_empty_coverage(self):
        for content in (None, '', 'mode: atomic\n'):
            with self.subTest(content=content):
                if content is not None:
                    coverage.REPORTS['go'].write_text(content)
                with self.assertRaises((OSError, ValueError)):
                    coverage.stage('go', Path('artifact'))
                self.assertFalse(Path('artifact').exists())


class ScannerReportTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.report = Path(self.directory.name) / 'report.json'

    def test_trivy_empty_scan_and_real_findings_remain_valid_for_cli_enforcement(self):
        for artifact_type in ('filesystem', 'repository'):
            for results in ([], [{'Target': 'package-lock.json', 'Vulnerabilities': [{'Severity': 'HIGH'}]}]):
                self.report.write_text(json.dumps({'SchemaVersion': 2, 'ArtifactType': artifact_type,
                                                   'ArtifactName': '.', 'Results': results}))
                reports.check_trivy(self.report)

    def test_trivy_missing_unreadable_and_nonreport_json_fail(self):
        for content in (None, '', 'not json', '{}', '[]', '{"SchemaVersion": 1}',
                        '{"SchemaVersion":2,"ArtifactType":"container_image","ArtifactName":"fixture"}'):
            with self.subTest(content=content):
                if content is not None:
                    self.report.write_text(content)
                result = subprocess.run(['python3', str(REPORT_SCRIPT), 'trivy', str(self.report)],
                                        capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)

    def test_aislop_all_findings_are_blocked_except_existing_skipped_audit_marker(self):
        for findings, rejected in [([], False), ([{'ruleId': 'security/dependency-audit-skipped'}], False),
                                    ([{'ruleId': 'ai-slop/console-leftover', 'level': 'warning'}], True),
                                    ([{'level': 'note'}], True)]:
            self.report.write_text(json.dumps({'version': '2.1.0', 'runs': [{
                'tool': {'driver': {'name': 'aislop'}}, 'results': findings,
            }]}))
            if rejected:
                with self.assertRaises(ValueError):
                    reports.check_aislop(self.report)
            else:
                reports.check_aislop(self.report)

    def test_aislop_missing_unreadable_or_absent_scanner_results_fail(self):
        for content in ('', 'not json', '{}', '{"version":"2.1.0","runs":[]}',
                        '{"version":"2.1.0","runs":[{"results":[]}]}'):
            self.report.write_text(content)
            result = subprocess.run(['python3', str(REPORT_SCRIPT), 'aislop', str(self.report)],
                                    capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)


class AislopCLIBehaviorTests(unittest.TestCase):
    def test_single_sarif_scan_preserves_score_threshold_exit_status_and_findings(self):
        binary = ROOT / '.github/aislop/node_modules/.bin/aislop'
        if not binary.exists():
            self.skipTest('installed Aislop toolchain required')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / '.aislop').mkdir()
            (root / 'index.js').write_text('export function getValue(input) {\n  console.log(input);\n  return input;\n}\n')
            for threshold, expected in ((100, 1), (0, 0)):
                (root / '.aislop/config.yml').write_text(f'ci:\n  failBelow: {threshold}\ntelemetry:\n  enabled: false\n')
                result = subprocess.run([str(binary), 'ci', '--sarif', str(root)], capture_output=True, text=True)
                self.assertEqual(result.returncode, expected, result.stderr)
                self.assertIn('ai-slop/console-leftover', [finding['ruleId'] for run in json.loads(result.stdout)['runs']
                                                           for finding in run['results']])
            (root / '.aislop/config.yml').write_text('ci:\n  failBelow: 100\ntelemetry:\n  enabled: false\n')
            result = subprocess.run([str(binary), 'ci', '--human', str(root)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1, result.stderr)


if __name__ == '__main__':
    unittest.main()
