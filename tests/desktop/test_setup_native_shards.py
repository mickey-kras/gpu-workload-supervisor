"""Exercise shard dispatch without requiring a graphical desktop."""
import contextlib
import io
import json
import pathlib
import tempfile
import unittest
from unittest import mock

import setup_native


class NativeShardTests(unittest.TestCase):
    def expected_cases(self):
        dimensions = [(620, 740, 1), (620, 740, 2), (620, 670, 1),
                      (620, 670, 2), (480, 480, 1), (480, 480, 2)]
        return {(theme, *size) for theme in ('light', 'dark', 'highcontrast') for size in dimensions}

    def test_shards_cover_every_case_once_for_supported_partition_sizes(self):
        for count in (1, 2, 6, 18):
            with self.subTest(count=count):
                assigned = [case for index in range(count)
                            for case in setup_native.select_cases(index, count)]
                self.assertEqual(len(assigned), 18)
                self.assertEqual(len(set(assigned)), 18)
                self.assertEqual(set(assigned), self.expected_cases())
                sizes = [len(setup_native.select_cases(index, count)) for index in range(count)]
                self.assertLessEqual(max(sizes) - min(sizes), 1)

    def test_unsharded_and_quick_commands_remain_available(self):
        self.assertEqual(set(setup_native.select_cases()), self.expected_cases())
        self.assertEqual(setup_native.select_cases(quick=True), [('light', 620, 740, 1)])

    def test_invalid_and_empty_shards_cannot_produce_a_green_manifest(self):
        for index, count, quick in [(None, 6, False), (0, None, False), (-1, 6, False),
                                    (6, 6, False), (0, 0, False), (0, 19, False), (0, 6, True)]:
            with self.subTest(index=index, count=count, quick=quick):
                with self.assertRaises(ValueError):
                    setup_native.select_cases(index, count, quick)

    def run_main(self, output, *flags, failure=False):
        argv = ['setup_native.py', '--output', str(output), *flags]
        captured = []

        def capture_case(root, destination, dimensions):
            self.assertEqual(root / 'tests/desktop/setup_native.py', pathlib.Path(setup_native.__file__).resolve())
            self.assertEqual(destination, output.resolve())
            captured.append(dimensions)
            if failure:
                raise AssertionError('native fixture failed')
            return {'case': list(dimensions)}

        with mock.patch.object(setup_native.sys, 'argv', argv), \
                mock.patch.object(setup_native.subprocess, 'run') as accessibility, \
                mock.patch.object(setup_native, 'run_case', side_effect=capture_case), \
                contextlib.redirect_stdout(io.StringIO()):
            if failure:
                with self.assertRaisesRegex(AssertionError, 'native fixture failed'):
                    setup_native.main()
            else:
                setup_native.main()
            accessibility.assert_called_once()
            self.assertEqual(pathlib.Path(accessibility.call_args.args[0][1]).name, 'test_setup_accessibility.py')
            self.assertTrue(accessibility.call_args.kwargs['check'])
        return captured

    def test_cli_dispatches_all_six_shards_and_writes_only_their_reports(self):
        with tempfile.TemporaryDirectory() as directory:
            assigned = []
            for index in range(6):
                output = pathlib.Path(directory) / f'shard-{index}'
                captured = self.run_main(output, '--shard-index', str(index), '--shard-count', '6')
                self.assertEqual(len(captured), 3)
                reports = json.loads((output / 'manifest.json').read_text())
                self.assertEqual([tuple(report['case']) for report in reports], captured)
                assigned.extend(captured)
            self.assertEqual(set(assigned), self.expected_cases())
            self.assertEqual(len(assigned), 18)

    def test_failed_case_cannot_write_a_success_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            output = pathlib.Path(directory)
            self.run_main(output, '--shard-index', '0', '--shard-count', '6', failure=True)
            self.assertFalse((output / 'manifest.json').exists())

    def test_invalid_cli_fails_before_any_native_work(self):
        with mock.patch.object(setup_native.sys, 'argv', ['setup_native.py', '--output', 'unused', '--shard-count', '6']), \
                mock.patch.object(setup_native.subprocess, 'run') as process, \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as failure:
                setup_native.main()
            self.assertEqual(failure.exception.code, 2)
            process.assert_not_called()


if __name__ == '__main__':
    unittest.main()
