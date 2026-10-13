import importlib.util
import pathlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('configure_nginx', pathlib.Path(__file__).parents[1] / 'scripts/configure_nginx.py')
nginx = importlib.util.module_from_spec(spec)
spec.loader.exec_module(nginx)


class NginxInstallTests(unittest.TestCase):
    def test_include_goes_inside_http_and_is_idempotent(self):
        original = '# http { fake }\nevents {}\nhttp {\n server { set $x "}"; }\n}\n'
        changed = nginx.add_include(original, '/etc/nginx/antiattendance-managed/sessions.conf')
        self.assertIn('http {\n    include', changed)
        self.assertTrue(changed.endswith(' server { set $x "}"; }\n}\n'))
        self.assertEqual(changed, nginx.add_include(changed, '/etc/nginx/antiattendance-managed/sessions.conf'))

    def test_no_http_block_rejected(self):
        with self.assertRaises(ValueError):
            nginx.add_include('events {} # http {}', '/tmp/site')

    def test_validation_failure_restores_global_config_and_site(self):
        with tempfile.TemporaryDirectory() as directory:
            config = pathlib.Path(directory) / 'nginx.conf'
            config.write_text('events {}\nhttp {}\n')
            original = config.read_bytes()
            calls = []

            def run(args, **kwargs):
                calls.append(args)
                if '-T' in args:
                    return subprocess.CompletedProcess(args, 0, f'# configuration file {config}:\n' + config.read_text())
                raise subprocess.CalledProcessError(1, args)

            with patch.object(nginx.subprocess, 'run', side_effect=run):
                with self.assertRaises(subprocess.CalledProcessError):
                    nginx.configure('aa.yellowteacrew.ru', 8080)
            self.assertEqual(config.read_bytes(), original)
            self.assertFalse((config.parent / 'antiattendance-managed/sessions.conf').exists())
            self.assertFalse(any('reload' in call for call in calls))

    def test_success_and_repeat_install_preserve_other_sites(self):
        with tempfile.TemporaryDirectory() as directory:
            config = pathlib.Path(directory) / 'nginx.conf'
            config.write_text('events {}\nhttp { server { server_name other.example; } }\n')

            def run(args, **kwargs):
                return subprocess.CompletedProcess(args, 0, f'# configuration file {config}:\n' + config.read_text())

            with patch.object(nginx.subprocess, 'run', side_effect=run):
                nginx.configure('aa.yellowteacrew.ru', 18088)
                first = config.read_text()
                nginx.configure('aa.yellowteacrew.ru', 18088)
            self.assertEqual(first, config.read_text())
            self.assertIn('server_name other.example;', first)
            site = (config.parent / 'antiattendance-managed/sessions.conf').read_text()
            self.assertIn('server_name aa.yellowteacrew.ru;', site)
            self.assertIn('127.0.0.1:18088', site)

    def test_existing_domain_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            config = pathlib.Path(directory) / 'nginx.conf'
            config.write_text('events {}\nhttp {server {server_name aa.yellowteacrew.ru;}}')
            with patch.object(nginx.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, f'# configuration file {config}:\n' + config.read_text())):
                with self.assertRaises(ValueError):
                    nginx.configure('aa.yellowteacrew.ru', 8080)


if __name__ == '__main__':
    unittest.main()
