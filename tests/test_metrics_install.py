"""Collector upgrades must not contend with the running collector's state lock."""
import importlib.util
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch
import subprocess

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('metrics_install', ROOT / 'scripts/install_metrics.py')
installer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(installer)


class MetricsInstallTests(unittest.TestCase):
    def test_update_stops_collector_before_lock_probe_and_preserves_totals(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            data = root / 'var/lib/antiattendance-metrics/test-project'
            data.mkdir(parents=True)
            (data / 'state.json').write_text('{"saved":"totals"}')
            running = True
            calls = []

            def run(command, **kwargs):
                nonlocal running
                calls.append(command)
                if command[:3] == ['systemctl', 'is-active', '--quiet']:
                    return SimpleNamespace(returncode=0 if running else 3)
                if command[:2] == ['systemctl', 'stop']:
                    running = False
                if '--once' in command:
                    self.assertFalse(running, 'probe collides with the daemon lock')
                if command[:2] == ['systemctl', 'restart']:
                    running = True
                return SimpleNamespace(returncode=0)

            with patch.object(installer.subprocess, 'run', side_effect=run), patch.object(installer.shutil, 'which', return_value='/usr/bin/python3'):
                installer.install('test-project', root)
            self.assertTrue(running)
            self.assertEqual((data / 'state.json').read_text(), '{"saved":"totals"}')
            unit = (root / 'etc/systemd/system/antiattendance-metrics-test-project.service').read_text()
            self.assertIn('RestrictAddressFamilies=AF_UNIX', unit)
            self.assertLess(next(i for i, c in enumerate(calls) if c[:2] == ['systemctl', 'stop']), next(i for i, c in enumerate(calls) if '--once' in c))

    def test_probe_failure_restores_a_previously_running_service(self):
        with tempfile.TemporaryDirectory() as temporary:
            calls = []
            target = Path(temporary) / 'usr/local/lib/antiattendance-metrics/collect_metrics.py'
            target.parent.mkdir(parents=True)
            target.write_text('# previous working collector')

            def run(command, **kwargs):
                calls.append(command)
                if '--once' in command:
                    raise subprocess.CalledProcessError(1, command)
                return SimpleNamespace(returncode=0)

            with patch.object(installer.subprocess, 'run', side_effect=run), patch.object(installer.shutil, 'which', return_value='/usr/bin/python3'):
                with self.assertRaises(subprocess.CalledProcessError):
                    installer.install('test-project', Path(temporary))
            self.assertEqual(calls[-1], ['systemctl', 'start', 'antiattendance-metrics-test-project.service'])
            self.assertEqual(target.read_text(), '# previous working collector')


if __name__ == '__main__':
    unittest.main()
