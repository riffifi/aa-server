"""Counter arithmetic and restart semantics without Docker/host changes."""
import importlib.util
import json
import os
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
import subprocess
import sys
import threading
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('resource_collector', ROOT / 'scripts/collect_metrics.py')
collector = importlib.util.module_from_spec(spec)
spec.loader.exec_module(collector)


class FakeDocker:
    def __init__(self):
        self.cid = 'first'
        self.value = 100
        self.running = True

    def containers(self, project):
        return [{'Id': self.cid, 'State': 'running' if self.running else 'exited',
                 'Labels': {'com.docker.compose.project': project, 'com.docker.compose.service': 'sessions'}},
                {'Id': 'foreign', 'Labels': {'com.docker.compose.project': 'other', 'com.docker.compose.service': 'sessions'}}]

    def stats(self, cid):
        assert cid == self.cid
        return {'networks': {'eth0': {'rx_bytes': self.value, 'tx_bytes': self.value * 2}},
                'cpu_stats': {'cpu_usage': {'total_usage': self.value * 10000000}},
                'memory_stats': {'usage': 500, 'stats': {'inactive_file': 100}, 'limit': 1000},
                'pids_stats': {'current': 12},
                'blkio_stats': {'io_service_bytes_recursive': [{'op': 'Read', 'value': self.value * 3}, {'op': 'Write', 'value': self.value * 4}]}}


class ResourceMetricsTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.stat = patch.object(collector.os, 'statvfs', return_value=SimpleNamespace(f_blocks=1000, f_frsize=4096, f_bavail=500), create=True)
        self.stat.start()
        self.addCleanup(self.stat.stop)
        # fchmod is not available on Windows; publication semantics still run.
        self.chmod = patch.object(collector.os, 'fchmod', create=True)
        self.chmod.start()
        self.addCleanup(self.chmod.stop)
        self.docker = FakeDocker()

    def instance(self):
        return collector.Collector(self.directory.name, 'test-project', self.docker)

    def test_totals_rates_restarts_recreation_and_resets(self):
        instance = self.instance()
        first = instance.sample(now=1000)
        self.assertEqual(first['total']['rx_bytes'], 100)
        self.assertIsNone(first['total']['cpu_percent'])
        self.docker.value = 150
        second = instance.sample(now=1010)
        self.assertEqual(second['total']['rx_bytes'], 150)
        self.assertEqual(second['total']['rx_bytes_per_second'], 5)
        self.assertEqual(second['total']['cpu_percent'], 5)
        self.assertEqual(second['total']['memory_bytes'], 400)
        self.assertEqual(len(second['components']), 1)
        self.assertEqual(self.instance().sample(now=1020)['total']['rx_bytes'], 150)
        self.docker.cid, self.docker.value = 'replacement', 20
        self.assertEqual(self.instance().sample(now=1030)['total']['rx_bytes'], 170)
        self.docker.value = 5  # counter reset within the same ID
        self.assertEqual(self.instance().sample(now=1040)['total']['rx_bytes'], 175)
        self.docker.running = False
        self.assertEqual(self.instance().sample(now=1050)['total']['rx_bytes'], 175)
        published = json.loads((Path(self.directory.name) / 'snapshot.json').read_text())
        self.assertNotIn('foreign', json.dumps(published))
        self.assertNotIn('Labels', json.dumps(published))

    def test_history_is_bounded_and_project_cannot_reuse_state(self):
        instance = self.instance()
        instance.history = [dict(time=1, cpu_percent=0)] * 9000
        self.assertLessEqual(len(instance.sample(now=1000)['history']), 1440)
        with self.assertRaises(ValueError):
            collector.Collector(self.directory.name, 'another-project', self.docker)

    def test_failed_stats_preserves_counters_and_reports_partial_data(self):
        instance = self.instance()
        instance.sample(now=1000)
        with patch.object(self.docker, 'stats', side_effect=OSError('not logged')):
            failed = instance.sample(now=1010)
        self.assertEqual(failed['errors'], 1)
        self.assertEqual(failed['total']['rx_bytes'], 100)
        self.assertTrue(failed['components'][0]['unavailable'])

    @unittest.skipUnless(os.name == 'posix', 'Linux Unix-socket integration')
    def test_cli_reads_unix_socket_without_a_listener_or_network(self):
        import http.server
        import socketserver
        docker = self.docker
        routes = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                routes.append(self.path)
                if self.path.startswith('/containers/json?'):
                    result = docker.containers('test-project')
                elif self.path == '/containers/first/stats?stream=false&one-shot=true':
                    result = docker.stats('first')
                else:
                    raise AssertionError(self.path)
                body = json.dumps(result).encode()
                self.send_response(200)
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_):
                pass

        path = Path(self.directory.name) / 'docker.sock'
        with socketserver.UnixStreamServer(str(path), Handler) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                output = Path(self.directory.name) / 'data'
                result = subprocess.run([sys.executable, '-I', str(ROOT / 'scripts/collect_metrics.py'),
                                         '--project', 'test-project', '--directory', str(output),
                                         '--socket', str(path), '--once'], capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                snapshot = json.loads((output / 'snapshot.json').read_text())
                self.assertEqual(snapshot['total']['rx_bytes'], 100)
                self.assertEqual((output / 'snapshot.json').stat().st_mode & 0o777, 0o644)
                self.assertEqual(len(json.loads((output / 'history.json').read_text())), 1)
                self.assertEqual(len(routes), 2)
            finally:
                server.shutdown()
                thread.join(timeout=2)


if __name__ == '__main__':
    unittest.main()
