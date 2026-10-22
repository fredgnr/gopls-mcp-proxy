"""Export the pinned collector's existing samples, not another step's Summary.

Run after ordinary job steps and before post actions. Only numeric metric fields
and allowlisted public runner identity are saved; no logs, arguments or secret values.
"""

import json
import math
import os
from pathlib import Path
import re
import tempfile
import time
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener


COLLECTOR = 'ycfreeman/workflow-telemetry-action@72ec425db8a31670fdc4419966940a6d980a0b49'
FIELDS = {
    'cpu': ('totalLoad', 'userLoad', 'systemLoad'),
    'memory': ('totalMemoryMb', 'activeMemoryMb', 'availableMemoryMb'),
    'network': ('rxMb', 'txMb'),
    'disk': ('rxMb', 'wxMb'),
    'disk_size': ('availableSizeMb', 'usedSizeMb'),
}
MAX_RESPONSE_BYTES = 4 * 1024 * 1024


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def read_samples(opener, metric):
    # Never send a GitHub token or honor an HTTP proxy for the loopback service.
    request = Request(f'http://localhost:7777/{metric}', headers={'Accept': 'application/json'})
    with opener.open(request, timeout=2) as response:
        raw = response.read(MAX_RESPONSE_BYTES + 1)
    if len(raw) > MAX_RESPONSE_BYTES:
        raise ValueError('Metric response exceeds the export limit')
    rows = json.loads(raw)
    if not isinstance(rows, list):
        raise ValueError('Expected a metric sample array')
    samples = []
    for row in rows:
        values = {key: row[key] for key in ('time', *FIELDS[metric])}
        if any(type(value) not in (int, float) or not math.isfinite(value) or value < 0
               for value in values.values()) or values['time'] == 0:
            raise ValueError('Invalid numeric metric sample')
        samples.append(values)
    return samples


def snapshot():
    identity = {name: os.environ.get(name, '') for name in (
        'GITHUB_REPOSITORY', 'GITHUB_WORKFLOW', 'GITHUB_WORKFLOW_REF', 'GITHUB_WORKFLOW_SHA',
        'GITHUB_RUN_ID', 'GITHUB_RUN_ATTEMPT', 'GITHUB_SHA', 'GITHUB_JOB',
        'RUNNER_NAME', 'RUNNER_OS', 'RUNNER_ARCH',
    )}
    report = {
        'schemaVersion': 1,
        'collector': COLLECTOR,
        'identity': identity,
        'captureStartedAtUnixMs': int(time.time() * 1000),
        'scope': {
            'level': 'runner', 'phase': 'end-of-main-steps', 'includesPostSteps': False,
            'samplingIntervalSeconds': 5,
            'notes': 'Periodic samples only; the final unsampled interval and post actions are excluded. '
                     'Series are read sequentially. Join identity to the exact run attempt jobs by runner_name. '
                     'These are runner totals, not per-process or exclusive per-step measurements.',
        },
        'units': {
            'time': 'Unix epoch milliseconds (UTC)', 'cpu': 'percent',
            'memory': 'MiB', 'disk_size': 'MiB',
            'network': 'MiB transferred during each collector sampling interval, not MiB/s',
            'disk': 'MiB transferred during each collector sampling interval, not MiB/s',
            'notes': 'Upstream I/O values are rounded down to whole MiB; zeros are not proof of inactivity. '
                     'Unsupported sensors may report zero upstream; this exporter does not infer support.',
        },
        'metrics': {},
    }
    opener = build_opener(ProxyHandler({}), NoRedirect())
    for metric in FIELDS:
        try:
            samples = read_samples(opener, metric)
            entry = {'status': 'available' if samples else 'no-samples', 'samples': samples}
        except Exception as error:
            # Error messages/bodies may contain unexpected data: retain only the class.
            entry = {'status': 'unavailable', 'samples': [], 'errorType': type(error).__name__}
        report['metrics'][metric] = entry
    report['captureFinishedAtUnixMs'] = int(time.time() * 1000)
    return report


def main():
    report = snapshot()
    directory = Path(tempfile.mkdtemp(prefix='gopls-mcp-proxy-telemetry-', dir=os.environ['RUNNER_TEMP']))
    filename = directory / 'metrics.json'
    if '\n' in str(filename) or '\r' in str(filename):
        raise ValueError('Invalid output path')
    filename.write_text(json.dumps(report, indent=2, allow_nan=False) + '\n', encoding='utf-8')
    identity = report['identity']
    name = '-'.join((identity['GITHUB_RUN_ID'], identity['GITHUB_RUN_ATTEMPT'],
                     identity['GITHUB_JOB'], directory.name.removeprefix('gopls-mcp-proxy-telemetry-')))
    name = 'telemetry-' + re.sub(r'[^A-Za-z0-9_-]', '_', name)
    with open(os.environ['GITHUB_OUTPUT'], 'a', encoding='utf-8') as output:
        output.write(f'file={filename}\nartifact={name}\n')
    for metric, entry in report['metrics'].items():
        if entry['status'] != 'available':
            print(f"::warning::Telemetry {metric}: {entry['status']}; see metrics.json")
    print('Exported runner metrics snapshot (ordinary steps only; post actions excluded).')


if __name__ == '__main__':
    main()
