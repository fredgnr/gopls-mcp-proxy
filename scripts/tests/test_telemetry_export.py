"""Bounded, credential-free metric snapshots; no live network or Actions writes."""

from contextlib import redirect_stdout
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
from urllib.error import URLError


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('export_workflow_telemetry', ROOT / 'scripts/export-workflow-telemetry.py')
EXPORT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(EXPORT)


class TelemetryExportTests(unittest.TestCase):
    def response(self, payload):
        response = Mock()
        response.__enter__ = Mock(return_value=response)
        response.__exit__ = Mock(return_value=False)
        response.read.return_value = payload
        return response

    def row(self, metric):
        return {'time': 1790511000000, **dict.fromkeys(EXPORT.FIELDS[metric], 12.5)}

    def opener(self, overrides=None):
        overrides = overrides or {}
        opener = Mock()
        def request(req, timeout):
            metric = req.full_url.rsplit('/', 1)[-1]
            value = overrides.get(metric, [self.row(metric)])
            if isinstance(value, Exception):
                raise value
            return self.response(value if isinstance(value, bytes) else json.dumps(value).encode())
        opener.open.side_effect = request
        return opener

    def test_retains_exact_numeric_samples_and_strips_unknown_fields(self):
        row = {**self.row('cpu'), 'command': 'private-command', 'TOKEN': 'private-token'}
        opener = self.opener({'cpu': [row]})
        samples = EXPORT.read_samples(opener, 'cpu')
        self.assertEqual(samples, [self.row('cpu')])
        request = opener.open.call_args.args[0]
        self.assertEqual(request.full_url, 'http://localhost:7777/cpu')
        self.assertEqual(request.get_method(), 'GET')
        self.assertEqual(request.header_items(), [('Accept', 'application/json')])
        self.assertEqual(opener.open.call_args.kwargs['timeout'], 2)

    def test_rejects_malformed_nonfinite_negative_missing_and_oversized_samples(self):
        row = self.row('cpu')
        bad = [b'not json', {}, [None], [{'time': row['time']}],
               [{**row, 'time': 0}], [{**row, 'userLoad': 'private-value'}]]
        bad += [[{**row, 'userLoad': value}] for value in (True, None, -1, float('nan'), float('inf'))]
        bad.append(b' ' * (EXPORT.MAX_RESPONSE_BYTES + 1))
        for value in bad:
            with self.subTest(value_type=type(value).__name__), self.assertRaises((ValueError, TypeError, KeyError)):
                EXPORT.read_samples(self.opener({'cpu': value}), 'cpu')

    def test_one_failed_metric_does_not_erase_other_series_or_expose_error_body(self):
        with patch.object(EXPORT, 'build_opener', return_value=self.opener({'network': URLError('private-token')})):
            report = EXPORT.snapshot()
        self.assertEqual(report['metrics']['network'], {
            'status': 'unavailable', 'samples': [], 'errorType': 'URLError',
        })
        self.assertEqual(report['metrics']['cpu']['samples'], [self.row('cpu')])
        self.assertNotIn('private-token', json.dumps(report))

    def test_empty_zero_and_unavailable_are_different_states(self):
        zeros = {key: (value if key == 'time' else 0) for key, value in self.row('cpu').items()}
        opener = self.opener({'cpu': [zeros], 'memory': [], 'disk': TimeoutError()})
        with patch.object(EXPORT, 'build_opener', return_value=opener):
            report = EXPORT.snapshot()
        self.assertEqual(report['metrics']['cpu']['status'], 'available')
        self.assertEqual(report['metrics']['memory']['status'], 'no-samples')
        self.assertEqual(report['metrics']['disk']['status'], 'unavailable')
        self.assertEqual(report['metrics']['cpu']['samples'][0]['userLoad'], 0)

    def test_snapshot_identity_scope_and_units_do_not_misrepresent_the_summary(self):
        environment = {'GITHUB_RUN_ID': '123', 'GITHUB_RUN_ATTEMPT': '2', 'GITHUB_JOB': 'api',
                       'RUNNER_NAME': 'GitHub Actions 42', 'GH_TOKEN': 'private-token',
                       'UNRELATED_PASSWORD': 'private-password', 'UNRELATED': 'private-other'}
        with patch.dict(os.environ, environment), patch.object(EXPORT, 'build_opener', return_value=self.opener()):
            report = EXPORT.snapshot()
        self.assertEqual(report['schemaVersion'], 1)
        self.assertEqual(report['identity']['GITHUB_RUN_ATTEMPT'], '2')
        self.assertEqual(report['identity']['RUNNER_NAME'], 'GitHub Actions 42')
        self.assertIs(report['scope']['includesPostSteps'], False)
        self.assertEqual(report['scope']['phase'], 'end-of-main-steps')
        self.assertIn('not MiB/s', report['units']['network'])
        self.assertIn('Unix epoch milliseconds', report['units']['time'])
        self.assertLessEqual(report['captureStartedAtUnixMs'], report['captureFinishedAtUnixMs'])
        self.assertNotIn('private-', json.dumps(report))

    def test_no_proxy_or_redirect_can_receive_loopback_requests(self):
        with patch.object(EXPORT, 'build_opener', return_value=self.opener()) as build:
            EXPORT.snapshot()
        proxy, redirect = build.call_args.args
        self.assertEqual(proxy.proxies, {})
        self.assertIsInstance(redirect, EXPORT.NoRedirect)
        self.assertIsNone(redirect.redirect_request(None, None, 302, '', {}, 'https://example.invalid'))

    def test_outputs_are_unique_per_job_instance_and_upload_only_the_json_file(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / 'output'
            environment = {'RUNNER_TEMP': str(root), 'GITHUB_OUTPUT': str(output),
                           'GITHUB_RUN_ID': '123', 'GITHUB_RUN_ATTEMPT': '1', 'GITHUB_JOB': 'api'}
            with patch.dict(os.environ, environment), patch.object(EXPORT, 'build_opener', return_value=self.opener()):
                with redirect_stdout(io.StringIO()):
                    EXPORT.main(); EXPORT.main()
            rows = output.read_text().splitlines()
            files = [Path(line.removeprefix('file=')) for line in rows if line.startswith('file=')]
            names = [line.removeprefix('artifact=') for line in rows if line.startswith('artifact=')]
            self.assertEqual(len(set(names)), 2)
            self.assertEqual(len(set(files)), 2)
            for name, filename in zip(names, files):
                self.assertRegex(name, r'^telemetry-123-1-api-[A-Za-z0-9_-]+$')
                self.assertEqual(filename.name, 'metrics.json')
                self.assertTrue(filename.is_relative_to(root))
                self.assertEqual(list(filename.parent.iterdir()), [filename])
                self.assertEqual(json.loads(filename.read_text())['metrics']['cpu']['status'], 'available')

    def test_unavailable_collector_still_creates_an_honest_diagnostic_artifact(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / 'output'
            opener = Mock(); opener.open.side_effect = URLError('private-sentinel')
            with patch.dict(os.environ, {'RUNNER_TEMP': temporary, 'GITHUB_OUTPUT': str(output)}), \
                    patch.object(EXPORT, 'build_opener', return_value=opener), redirect_stdout(io.StringIO()) as log:
                EXPORT.main()
            filename = Path(output.read_text().splitlines()[0].removeprefix('file='))
            report = json.loads(filename.read_text())
            self.assertTrue(all(entry['status'] == 'unavailable' for entry in report['metrics'].values()))
            self.assertNotIn('private-sentinel', filename.read_text() + log.getvalue())
            self.assertIn('::warning::', log.getvalue())

    def test_failed_write_never_publishes_an_upload_path(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / 'output'
            with patch.dict(os.environ, {'RUNNER_TEMP': temporary, 'GITHUB_OUTPUT': str(output)}), \
                    patch.object(EXPORT, 'build_opener', return_value=self.opener()), \
                    patch.object(Path, 'write_text', side_effect=OSError('disk full')):
                with self.assertRaises(OSError):
                    EXPORT.main()
            self.assertFalse(output.exists())


if __name__ == '__main__':
    unittest.main()
