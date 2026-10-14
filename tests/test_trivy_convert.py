import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


@unittest.skipUnless(shutil.which('trivy'), 'installed Trivy CLI required')
class TrivyConvertTests(unittest.TestCase):
    def test_one_report_preserves_low_sarif_findings_and_high_critical_enforcement(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            report = root / 'report.json'
            sarif = root / 'report.sarif'
            source = {'SchemaVersion': 2, 'ArtifactName': '.', 'ArtifactType': 'filesystem', 'Results': [{
                'Target': 'fixture/package-lock.json', 'Class': 'lang-pkgs', 'Type': 'npm', 'Vulnerabilities': [{
                    'VulnerabilityID': 'CVE-2025-0001', 'PkgName': 'fixture', 'InstalledVersion': '1.0.0',
                    'FixedVersion': '1.0.1', 'Severity': 'LOW', 'Title': 'Fixture vulnerability',
                }],
            }]}
            (root / 'trivy.yaml').write_text('severity:\n  - LOW\n')
            report.write_text(json.dumps(source))
            subprocess.run(['trivy', 'convert', '--format', 'sarif', '--severity', 'UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL', '--output', str(sarif), str(report)], check=True, cwd=root)
            findings = [finding for run in json.loads(sarif.read_text())['runs'] for finding in run['results']]
            self.assertTrue(findings, 'LOW findings must still be included in the existing SARIF scope')
            command = ['trivy', 'convert', '--format', 'table', '--severity', 'HIGH,CRITICAL', '--exit-code', '1', str(report)]
            self.assertEqual(subprocess.run(command, capture_output=True, cwd=root).returncode, 0)
            for severity in ('HIGH', 'CRITICAL'):
                source['Results'][0]['Vulnerabilities'][0]['Severity'] = severity
                report.write_text(json.dumps(source))
                self.assertEqual(subprocess.run(command, capture_output=True, cwd=root).returncode, 1, severity)
                subprocess.run(['trivy', 'convert', '--format', 'sarif', '--severity', 'UNKNOWN,LOW,MEDIUM,HIGH,CRITICAL',
                                '--output', str(sarif), str(report)], check=True, cwd=root)
                self.assertTrue([finding for run in json.loads(sarif.read_text())['runs'] for finding in run['results']])
            source['Results'] = []
            report.write_text(json.dumps(source))
            self.assertEqual(subprocess.run(command, capture_output=True, cwd=root).returncode, 0)

    def test_missing_and_unreadable_scan_output_cannot_be_converted(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory) / 'missing.json'
            command = ['trivy', 'convert', '--format', 'table', '--exit-code', '1', str(report)]
            root = Path(directory)
            self.assertNotEqual(subprocess.run(command, capture_output=True, cwd=root).returncode, 0)
            report.write_text('not json')
            self.assertNotEqual(subprocess.run(command, capture_output=True, cwd=root).returncode, 0)


if __name__ == '__main__':
    unittest.main()
