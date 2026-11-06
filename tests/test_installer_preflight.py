import argparse
import importlib.util
import json
from pathlib import Path
import socket
import subprocess
import unittest
from unittest.mock import MagicMock, patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('installer_preflight', ROOT / 'scripts/installer-preflight.py')
checks = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checks)


class PreflightTests(unittest.TestCase):
    def args(self, **kwargs):
        data = dict(docker_sudo=False, project='test', site_port=18080, aaid_port=18091,
                    panel_port=18092, site_url='https://aa.ytc-srv.org',
                    aaid_url='https://aaid.ytc-srv.org', require_dns=False)
        data.update(kwargs)
        return argparse.Namespace(**data)

    def run_checks(self, *, ports='', busy=False, dns_error=False, require_dns=False):
        commands = [subprocess.CompletedProcess([], 0, ports), subprocess.CompletedProcess([], 0, '')]
        probe = MagicMock()
        probe.__enter__.return_value = probe
        if busy:
            probe.bind.side_effect = OSError('busy')
        disk = argparse.Namespace(free=10 << 30)
        addresses = [(socket.AF_INET, socket.SOCK_STREAM, 0, '', ('192.0.2.1', 0))]
        with patch.object(checks.subprocess, 'run', side_effect=commands), \
                patch.object(checks.shutil, 'disk_usage', return_value=disk), \
                patch.object(checks.socket, 'socket', return_value=probe), \
                patch.object(checks.socket, 'getaddrinfo', side_effect=socket.gaierror() if dns_error else None, return_value=addresses):
            checks.check_host(self.args(require_dns=require_dns))

    def test_foreign_process_on_required_port_has_solution(self):
        with self.assertRaisesRegex(ValueError, 'занят другим процессом.*Решение:'):
            self.run_checks(busy=True)

    def test_update_accepts_ports_owned_by_same_compose_project(self):
        self.run_checks(busy=True, ports='127.0.0.1:18080->80/tcp,127.0.0.1:18091->8090/tcp,127.0.0.1:18092->8081/tcp')

    def test_missing_dns_blocks_certificate_issuance_with_remedy(self):
        with self.assertRaisesRegex(ValueError, 'создайте A/AAAA'):
            self.run_checks(dns_error=True, require_dns=True)

    def test_missing_dns_does_not_prevent_local_only_installation(self):
        self.run_checks(dns_error=True)

    def test_health_validates_both_protocol_and_identity_service(self):
        opener = MagicMock()

        def response(url, timeout):
            data = {'ok': True, 'service': 'aa-id'} if ':18091/' in url else {'ok': True, 'protocol': 2}
            stream = MagicMock()
            stream.__enter__.return_value = stream
            stream.status = 200
            stream.read.return_value = json.dumps(data).encode()
            return stream

        opener.open.side_effect = response
        with patch.object(checks.urllib.request, 'build_opener', return_value=opener):
            checks.check_health(self.args())
        self.assertEqual(opener.open.call_count, 3)

    def test_wrong_health_payload_has_remedy(self):
        stream = MagicMock()
        stream.__enter__.return_value = stream
        stream.status = 200
        stream.read.return_value = b'{"ok":true}'
        opener = MagicMock()
        opener.open.return_value = stream
        with patch.object(checks.urllib.request, 'build_opener', return_value=opener):
            with self.assertRaisesRegex(ValueError, 'Решение:'):
                checks.check_health(self.args())


if __name__ == '__main__':
    unittest.main()
